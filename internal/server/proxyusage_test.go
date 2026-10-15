// proxyusage_test.go 代理池路由（zcode/trae/autoclaw）用量记账的回归测试。
//
// 背景 bug：glm-5.3 落 zcode/autoclaw 等代理池时请求直接透传，完全绕过
// usage.Recorder，面板「用量」页永远看不到这批调用。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func TestExtractUsageTail(t *testing.T) {
	t.Run("sse tail", func(t *testing.T) {
		tail := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8,\"credit\":0.5}}\n\n" +
			"data: [DONE]\n\n")
		u, ok := extractUsageTail(tail)
		if !ok {
			t.Fatal("usage not found")
		}
		if u.PromptTokens == nil || *u.PromptTokens != 3 ||
			u.CompletionTokens == nil || *u.CompletionTokens != 5 ||
			u.TotalTokens == nil || *u.TotalTokens != 8 ||
			u.Credit == nil || *u.Credit != 0.5 {
			t.Fatalf("usage=%+v", u)
		}
	})
	t.Run("sync json tail", func(t *testing.T) {
		tail := []byte(`{"id":"1","choices":[{"message":{"content":"{\"usage\":1}"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
		u, ok := extractUsageTail(tail)
		if !ok {
			t.Fatal("usage not found")
		}
		if *u.PromptTokens != 10 || *u.CompletionTokens != 20 || *u.TotalTokens != 30 {
			t.Fatalf("usage=%+v", u)
		}
	})
	t.Run("no usage", func(t *testing.T) {
		if _, ok := extractUsageTail([]byte("data: {\"choices\":[]}\n\n")); ok {
			t.Fatal("unexpected usage")
		}
	})
}

// TestProxiedChatRecordsUsage 端到端回归：zcode / trae 代理路由的请求必须进
// 用量时序（realm=池名、uid=池名、model=剥前缀裸名），响应本身原样透传。
func TestProxiedChatRecordsUsage(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8,\"credit\":0.5}}\n\n" +
		"data: [DONE]\n\n"
	zcodeFake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})
	traeFake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`)
	})

	t.Run("zcode stream", func(t *testing.T) {
		rec := usage.New("")
		h := NewHandler(Config{
			Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			ZcodeChat: zcodeFake,
			Usage:     rec,
		})
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"zcode/glm-5.3","messages":[]}`)))
		if resp.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", resp.Code, resp.Body)
		}
		if !strings.Contains(resp.Body.String(), `"total_tokens":8`) {
			t.Fatalf("response passthrough broken: %s", resp.Body)
		}
		s := rec.Snapshot(24, nil)
		if s.Totals.PromptTokens != 3 || s.Totals.CompletionTok != 5 || s.Totals.TotalTokens != 8 {
			t.Fatalf("totals=%+v", s.Totals)
		}
		if s.Totals.Credits != 0.5 || s.Totals.CreditSamples != 1 {
			t.Fatalf("credits=%+v", s.Totals)
		}
		if len(s.ByRealm) != 1 || s.ByRealm[0].Key != "zcode" {
			t.Fatalf("by_realm=%+v", s.ByRealm)
		}
		if len(s.ByAccount) != 1 || s.ByAccount[0].Key != "zcode" {
			t.Fatalf("by_account=%+v", s.ByAccount)
		}
		if len(s.ByModel) != 1 || s.ByModel[0].Key != "glm-5.3" {
			t.Fatalf("by_model=%+v", s.ByModel)
		}
	})

	t.Run("trae sync", func(t *testing.T) {
		rec := usage.New("")
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			TraeChat: traeFake,
			Usage:    rec,
		})
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"trae/glm-5.3","messages":[]}`)))
		if resp.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", resp.Code, resp.Body)
		}
		s := rec.Snapshot(24, nil)
		if s.Totals.PromptTokens != 7 || s.Totals.CompletionTok != 9 || s.Totals.TotalTokens != 16 {
			t.Fatalf("totals=%+v", s.Totals)
		}
		if len(s.ByRealm) != 1 || s.ByRealm[0].Key != "trae" || len(s.ByModel) != 1 || s.ByModel[0].Key != "glm-5.3" {
			t.Fatalf("dims=%+v / %+v", s.ByRealm, s.ByModel)
		}
	})

	t.Run("success without usage is not an error", func(t *testing.T) {
		// autoclaw2api 非流式响应合法地不带 usage：必须计成功（errors=0），token 留 0。
		rec := usage.New("")
		h := NewHandler(Config{
			Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			ZcodeChat: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}),
			Usage: rec,
		})
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"zcode/glm-5.3","messages":[]}`)))
		s := rec.Snapshot(24, nil)
		if s.Totals.Requests != 1 || s.Totals.Errors != 0 {
			t.Fatalf("totals=%+v want 1 request / 0 error", s.Totals)
		}
	})

	t.Run("failed request counts as error", func(t *testing.T) {
		rec := usage.New("")
		h := NewHandler(Config{
			Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			ZcodeChat: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream down"}}`)
			}),
			Usage: rec,
		})
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"zcode/glm-5.3","messages":[]}`)))
		s := rec.Snapshot(24, nil)
		if s.Totals.Requests != 1 || s.Totals.Errors != 1 {
			t.Fatalf("totals=%+v want 1 request / 1 error", s.Totals)
		}
	})
}
