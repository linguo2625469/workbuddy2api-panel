package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestTranslateRequestBasic 覆盖 instructions + input 文本 + 参数透传。
func TestTranslateRequestBasic(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4.1-flash",
		"instructions": "You are a coding agent.",
		"input": "hello",
		"stream": true,
		"max_output_tokens": 1024,
		"reasoning": {"effort": "high"},
		"tool_choice": "auto",
		"parallel_tool_calls": false
	}`)
	chat, meta, err := TranslateRequest(body)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if out["model"] != "deepseek-v4.1-flash" {
		t.Errorf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true", out["stream"])
	}
	if out["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want 1024", out["max_tokens"])
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", out["reasoning_effort"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (%v)", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are a coding agent." {
		t.Errorf("system message = %v", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "hello" {
		t.Errorf("user message = %v", second)
	}
	if meta.ToolChoice != "auto" {
		t.Errorf("meta.ToolChoice = %v", meta.ToolChoice)
	}
	if meta.ParallelToolCalls != false {
		t.Errorf("meta.ParallelToolCalls = %v", meta.ParallelToolCalls)
	}
}

// TestTranslateRequestToolSequence 覆盖 function_call / 输出 / custom_tool_call 的配对。
//
// 注意：Responses 的 function_call 与 function_call_output 是交替出现的，Chat 侧
// 也必须保持「assistant(call) → tool(result) → assistant(call) → tool(result)」，
// 相邻的两次调用之间隔着工具结果，不能合并（合并会破坏工具配对）。
func TestTranslateRequestToolSequence(t *testing.T) {
	body := []byte(`{
		"model": "gpt-6-astra",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"fix it"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
			{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"},
			{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch","call_id":"call_2"},
			{"type":"custom_tool_call_output","call_id":"call_2","output":"Done!"}
		]
	}`)
	chat, _, err := TranslateRequest(body)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages len = %d, want 5: %s", len(msgs), chat)
	}
	// 第 2 条：assistant 携带 reasoning_content + 第一个工具调用。
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msg[1].role = %v", asst["role"])
	}
	if asst["reasoning_content"] != "thinking" {
		t.Errorf("reasoning_content = %v", asst["reasoning_content"])
	}
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("msg[1].tool_calls len = %d, want 1", len(calls))
	}
	first, _ := calls[0].(map[string]any)
	if first["id"] != "call_1" {
		t.Errorf("first call id = %v", first["id"])
	}
	// 第 3 条：第一个工具结果按 call_id 回填。
	tool1, _ := msgs[2].(map[string]any)
	if tool1["role"] != "tool" || tool1["tool_call_id"] != "call_1" || tool1["content"] != "a.txt" {
		t.Errorf("tool1 = %v", tool1)
	}
	// 第 4 条：第二个 assistant（custom 工具调用，参数包成 {"input": "..."}）。
	asst2, _ := msgs[3].(map[string]any)
	if asst2["role"] != "assistant" {
		t.Fatalf("msg[3].role = %v", asst2["role"])
	}
	calls2, _ := asst2["tool_calls"].([]any)
	if len(calls2) != 1 {
		t.Fatalf("msg[3].tool_calls len = %d, want 1", len(calls2))
	}
	second, _ := calls2[0].(map[string]any)
	fn, _ := second["function"].(map[string]any)
	if !strings.Contains(stringOf(fn["arguments"]), `"input"`) {
		t.Errorf("custom tool arguments = %v, want wrapped input", fn["arguments"])
	}
	// 第 5 条：第二个工具结果。
	tool2, _ := msgs[4].(map[string]any)
	if tool2["tool_call_id"] != "call_2" {
		t.Errorf("tool2 = %v", tool2)
	}
}
// TestTranslateRequestNamespaceAndCustomTools 覆盖 namespace 展开与 custom 降级。
func TestTranslateRequestNamespaceAndCustomTools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-6-astra",
		"input": "hi",
		"tools": [
			{"type":"namespace","name":"codex_app","tools":[
				{"name":"list_threads","description":"list","parameters":{"type":"object","properties":{}}},
				{"name":"read_thread","parameters":{"type":"object","properties":{}}}
			]},
			{"type":"custom","name":"apply_patch","description":"edit files","format":{"definition":"grammar"}}
		]
	}`)
	chat, meta, err := TranslateRequest(body)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools len = %d, want 3: %s", len(tools), chat)
	}
	// namespace 展开出的工具是扁平 function。
	ns0, _ := tools[0].(map[string]any)
	if ns0["type"] != "function" || ns0["name"] != "list_threads" {
		t.Errorf("tool[0] = %v", ns0)
	}
	// custom 工具被降级成单 input 参数的 function。
	custom, _ := tools[2].(map[string]any)
	if custom["type"] != "function" || custom["name"] != "apply_patch" {
		t.Errorf("custom tool = %v", custom)
	}
	params, _ := custom["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Errorf("custom tool parameters = %v, want input property", params)
	}
	if !meta.IsCustomTool("apply_patch") {
		t.Errorf("meta.CustomTools = %v", meta.CustomTools)
	}
	if got := meta.NamespaceMap["list_threads"]; got != "codex_app" {
		t.Errorf("namespace map = %v", meta.NamespaceMap)
	}
}

// TestFromChatText 覆盖纯文本的非流式折叠。
func TestFromChatText(t *testing.T) {
	chat := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "hello"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     float64(10),
			"completion_tokens": float64(2),
			"total_tokens":      float64(12),
		},
	}
	obj := FromChat(chat, &Meta{}, "deepseek-v4.1-flash")
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Errorf("obj = %v", obj)
	}
	if obj["output_text"] != "hello" {
		t.Errorf("output_text = %v", obj["output_text"])
	}
	output, _ := obj["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len = %d", len(output))
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("output[0] = %v", msg)
	}
	usage, _ := obj["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(2) {
		t.Errorf("usage = %v", usage)
	}
}

// TestFromChatToolsAndNamespace 覆盖工具调用还原（custom + namespace）。
func TestFromChatToolsAndNamespace(t *testing.T) {
	chat := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					map[string]any{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "list_threads", "arguments": "{}"},
					},
					map[string]any{
						"id": "call_2", "type": "function",
						"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin Patch"}`},
					},
				},
			},
			"finish_reason": "tool_calls",
		}},
	}
	meta := &Meta{
		CustomTools:  map[string]bool{"apply_patch": true},
		NamespaceMap: map[string]string{"list_threads": "codex_app"},
	}
	obj := FromChat(chat, meta, "gpt-6-astra")
	output, _ := obj["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len = %d: %v", len(output), obj)
	}
	fc, _ := output[0].(map[string]any)
	if fc["type"] != "function_call" || fc["namespace"] != "codex_app" || fc["name"] != "list_threads" {
		t.Errorf("function_call = %v", fc)
	}
	ctc, _ := output[1].(map[string]any)
	if ctc["type"] != "custom_tool_call" {
		t.Errorf("custom call = %v", ctc)
	}
	if ctc["input"] != "*** Begin Patch" {
		t.Errorf("custom input = %v, want unwrapped", ctc["input"])
	}
}

// TestStreamEvents 用一段固定的 Chat SSE 验证 Responses 事件序列。
func TestStreamEvents(t *testing.T) {
	chatSSE := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"he"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"llo"},"index":0}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"list_threads","arguments":"{\"a\""}}]},"index":0}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"index":0}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}]}`,
		`data: {"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	var buf bytes.Buffer
	meta := &Meta{NamespaceMap: map[string]string{"list_threads": "codex_app"}}
	if err := Stream(&buf, strings.NewReader(chatSSE), meta, "gpt-6-astra"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.reasoning_summary_text.delta",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"namespace":"codex_app"`,
		`"text":"hello"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream output missing %q\n---\n%s", want, out)
		}
	}
	// 事件必须按序递增，且 response.completed 是最后一帧。
	if !strings.HasSuffix(strings.TrimSpace(out), "}") {
		t.Errorf("stream output should end with a data frame")
	}
	last := out[strings.LastIndex(out, "event: "):]
	if !strings.HasPrefix(last, "event: response.completed") {
		t.Errorf("last event = %q, want response.completed", last)
	}
}

// TestStreamEmpty 空流返回 ErrEmptyStream（server 据此记 502 观测）。
func TestStreamEmpty(t *testing.T) {
	var buf bytes.Buffer
	err := Stream(&buf, strings.NewReader("data: [DONE]\n\n"), &Meta{}, "m")
	if err != ErrEmptyStream {
		t.Fatalf("err = %v, want ErrEmptyStream", err)
	}
}

// TestStreamUpstreamError 上游 error 帧转成 response.failed。
func TestStreamUpstreamError(t *testing.T) {
	var buf bytes.Buffer
	err := Stream(&buf, strings.NewReader(`data: {"error":{"code":11133,"message":"bad"}}`+"\n\n"), &Meta{}, "m")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(buf.String(), "response.failed") {
		t.Errorf("output = %s", buf.String())
	}
}

// TestResolveNamespacedName 覆盖模型回传名的三种形态。
func TestResolveNamespacedName(t *testing.T) {
	mapping := map[string]string{"list_threads": "codex_app", "js": "codex_app"}
	cases := []struct{ in, wantName, wantNS string }{
		{"list_threads", "list_threads", "codex_app"},
		{"codex_app__list_threads", "list_threads", "codex_app"},
		{"codex_app::js", "js", "codex_app"},
		{"other", "other", ""},
	}
	for _, c := range cases {
		name, ns := ResolveNamespacedName(c.in, mapping)
		if name != c.wantName || ns != c.wantNS {
			t.Errorf("ResolveNamespacedName(%q) = (%q,%q), want (%q,%q)", c.in, name, ns, c.wantName, c.wantNS)
		}
	}
}
