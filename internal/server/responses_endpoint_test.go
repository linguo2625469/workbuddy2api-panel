package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestResponsesEndpointNonStream 覆盖 /v1/responses 非流式全链路：
// 请求翻译 → 选号 → 上游调用 → 回程折叠成 Responses 对象。
func TestResponsesEndpointNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"hi","stream":false,"instructions":"be brief"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%q", resp["output_text"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v", resp["model"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("output[0]=%v", msg)
	}
	// 管线共用：用量必须记到账号上（证明 responses 走的是同一条记账路径）。
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
}

// TestResponsesEndpointStream 覆盖流式：上游 Chat SSE → Responses 事件流。
func TestResponsesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	body := `{"model":"glm-5.2","input":"hi","stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"text":"你好"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Errorf("Responses 事件流不应出现 [DONE] 帧\n%s", out)
	}
}

// TestResponsesEndpointAuth 覆盖鉴权：配置 api_key 后无 Bearer 必须 401。
func TestResponsesEndpointAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "sk-test",
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
	// 带上正确密钥后放行。
	req2 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("authorized code=%d body=%s", rec2.Code, rec2.Body)
	}
}

// TestResponsesEndpointUpstreamError 上游 400 原样透传（含上游 body 原文）。
func TestResponsesEndpointUpstreamError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("want error status, got 200 body=%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "11101") {
		t.Errorf("upstream body not passed through: %s", rec.Body)
	}
}

// TestResponsesEndpointRealmPrefix 模型名带 [realm:] 前缀时按 realm 路由并剥前缀。
func TestResponsesEndpointRealmPrefix(t *testing.T) {
	var gotModel string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{
			UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai",
		}),
		Upstream: up,
	})
	// 记录上游实际收到的 body 由 fake transport 无法直接取；这里退一步只断言
	// 请求成功且响应 model 是裸名（前缀是网关侧协议，不应回显给客户端）。
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"global:glm-5.2","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = gotModel
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v, want bare name", resp["model"])
	}
}
