package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestResponsesToChat(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.3-codex",
		"instructions":"You are Codex",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"run ls"}]},
			{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"},
			{"type":"reasoning","summary":[]}
		],
		"tools":[{"type":"function","name":"Bash","parameters":{"type":"object"}}],
		"reasoning":{"effort":"medium"},
		"max_output_tokens":128,
		"store":true,
		"previous_response_id":"resp_old"
	}`)
	got, err := responsesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["store"]; ok {
		t.Fatal("store must be dropped")
	}
	if _, ok := obj["previous_response_id"]; ok {
		t.Fatal("previous_response_id must be dropped")
	}
	if _, ok := obj["input"]; ok {
		t.Fatal("input must be translated away")
	}
	if obj["model"] != "gpt-5.3-codex" {
		t.Fatalf("model=%v", obj["model"])
	}
	if obj["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort=%v", obj["reasoning_effort"])
	}
	if obj["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens=%v", obj["max_output_tokens"])
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	tm := tools[0].(map[string]any)
	fn := tm["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("tool name=%v", fn["name"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d %#v", len(msgs), msgs)
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "You are Codex" {
		t.Fatalf("system=%v", m0)
	}
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "run ls" {
		t.Fatalf("user=%v", m1)
	}
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "assistant" {
		t.Fatalf("assistant=%v", m2)
	}
	tcs := m2["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Fatalf("call id=%v", tc["id"])
	}
	tfn := tc["function"].(map[string]any)
	if tfn["name"] != "Bash" || tfn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("function=%v", tfn)
	}
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" || m3["content"] != "a.txt" {
		t.Fatalf("tool msg=%v", m3)
	}
}

func TestResponsesToChatStringInput(t *testing.T) {
	got, err := responsesToChat([]byte(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	msgs := obj["messages"].([]any)
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hi" {
		t.Fatalf("got=%v", m)
	}
	if obj["stream"] != true {
		t.Fatalf("stream=%v", obj["stream"])
	}
}

func TestChatCompletionToResponse(t *testing.T) {
	chat := map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": float64(1),
		"model":   "m",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hi",
				"reasoning_content": "think",
				"tool_calls": []any{map[string]any{
					"id":       "call_a",
					"type":     "function",
					"function": map[string]any{"name": "Bash", "arguments": "{}"},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	}
	got := chatCompletionToResponse(chat)
	if got["id"] != "resp_chatcmpl-1" || got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("envelope=%v", got)
	}
	out := got["output"].([]any)
	if len(out) != 3 {
		t.Fatalf("output len=%d %#v", len(out), out)
	}
	if out[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("item0=%v", out[0])
	}
	if out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("item1=%v", out[1])
	}
	if out[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("item2=%v", out[2])
	}
	fc := out[2].(map[string]any)
	if fc["call_id"] != "call_a" || fc["name"] != "Bash" {
		t.Fatalf("function_call=%v", fc)
	}
	u := got["usage"].(map[string]any)
	if u["input_tokens"] != 1 || u["output_tokens"] != 2 {
		t.Fatalf("usage=%v", u)
	}
}

func TestResponsesStreamEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"x\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "response.created" {
		t.Fatalf("first=%v", types)
	}
	if types[len(types)-1] != "response.completed" {
		t.Fatalf("last=%v", types)
	}
	need := []string{
		"response.reasoning_text.delta",
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.completed",
	}
	for _, n := range need {
		if !containsStr(types, n) {
			t.Fatalf("missing %s in %v", n, types)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":"think"`) || !strings.Contains(body, `"delta":"hi"`) {
		t.Fatalf("deltas missing: %s", body)
	}
	if !strings.Contains(body, `"delta":"{\"x\":"`) || !strings.Contains(body, `"delta":"1}"`) {
		t.Fatalf("tool args missing: %s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatal("chat [DONE] must not leak")
	}
}

func TestResponsesEndpointNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("got=%v", obj)
	}
	out := obj["output"].([]any)
	found := false
	for _, it := range out {
		m := it.(map[string]any)
		if m["type"] == "message" {
			found = true
			content := m["content"].([]any)
			text := content[0].(map[string]any)["text"]
			if text != "你好" {
				t.Fatalf("text=%v", text)
			}
		}
	}
	if !found {
		t.Fatalf("no message in %v", out)
	}
}

func TestResponsesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s", rec.Header().Get("Content-Type"))
	}
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if !containsStr(types, "response.output_text.delta") {
		t.Fatalf("no text delta in %v", types)
	}
}

func TestResponsesEndpointStreamErrorBeforeSSE(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 SSE", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s body=%s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
	types := sseTypes(rec.Body.String())
	if !containsStr(types, "response.failed") {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if containsStr(types, "response.completed") {
		t.Fatalf("failed stream must not complete: %v", types)
	}
}

func sseTypes(body string) []string {
	var types []string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if t, ok := m["type"].(string); ok {
				types = append(types, t)
			}
		}
	}
	return types
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestResponsesToChatInvalidJSON(t *testing.T) {
	_, err := responsesToChat([]byte(`{`))
	if err == nil {
		t.Fatal("want error")
	}
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

// TestUsageCacheFields 上游的缓存命中数必须透出到两种格式的 usage（此前一律 0，
// 客户端和面板都看不见缓存是否生效）。
func TestUsageCacheFields(t *testing.T) {
	up := map[string]any{
		"prompt_tokens":             float64(100),
		"completion_tokens":         float64(5),
		"prompt_cache_hit_tokens":   float64(8960),
		"prompt_cache_write_tokens": float64(0),
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(8960)},
	}
	if got := anthropicUsage(up)["cache_read_input_tokens"]; got != float64(8960) {
		t.Fatalf("anthropic cache_read_input_tokens=%v want 8960", got)
	}
	if got := anthropicUsage(nil)["cache_read_input_tokens"]; got != nil {
		t.Fatalf("nil usage 不该凭空造缓存字段, got=%v", got)
	}
	d, _ := convertUsage(up)["input_tokens_details"].(map[string]any)
	if d == nil || d["cached_tokens"] != float64(8960) {
		t.Fatalf("responses input_tokens_details=%v want cached_tokens 8960", d)
	}

	chat, err := responsesToChat([]byte(`{"model":"m","prompt_cache_key":"sess-1","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(chat, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["prompt_cache_key"] != "sess-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", obj["prompt_cache_key"])
	}
}

// commitTrackingWriter 模拟 net/http 的响应提交语义：Write/Flush 在未提交时
// 隐式 WriteHeader(200)；提交后的重复 WriteHeader 计数（= 线上那条
// "superfluous response.WriteHeader call" 警告的来源）。
type commitTrackingWriter struct {
	hdr         http.Header
	body        bytes.Buffer
	status      int
	committed   bool
	superfluous int
}

func newCommitTrackingWriter() *commitTrackingWriter {
	return &commitTrackingWriter{hdr: http.Header{}}
}

func (c *commitTrackingWriter) Header() http.Header { return c.hdr }
func (c *commitTrackingWriter) WriteHeader(code int) {
	if c.committed {
		c.superfluous++
		return
	}
	c.committed, c.status = true, code
}
func (c *commitTrackingWriter) Write(p []byte) (int, error) {
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(p)
}
func (c *commitTrackingWriter) Flush() {
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
}

// TestResponsesEmitAfterFlushNoSuperfluousWriteHeader 流式路径先 Flush（上游逐帧
// flush 已隐式提交 200）再 emit：不得出现第二次 WriteHeader（net/http 会打
// superfluous 警告，线上实测噪音）。
func TestResponsesEmitAfterFlushNoSuperfluousWriteHeader(t *testing.T) {
	fw := newCommitTrackingWriter()
	w := &responsesWriter{ResponseWriter: fw, stream: true}
	w.Flush() // 模拟 StreamHint 在无输出帧后的 flush：提交 200
	if err := w.emit("response.created", map[string]any{"response": map[string]any{"id": "resp_1"}}); err != nil {
		t.Fatal(err)
	}
	if fw.superfluous != 0 {
		t.Fatalf("emit 二次 WriteHeader 次数=%d want 0", fw.superfluous)
	}
	if fw.status != http.StatusOK {
		t.Fatalf("status=%d want 200", fw.status)
	}
	if !strings.Contains(fw.body.String(), "event: response.created") {
		t.Fatalf("body=%q", fw.body.String())
	}
}
