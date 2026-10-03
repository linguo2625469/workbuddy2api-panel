package responses

import (
	"encoding/json"
	"strings"
)

// FromChat 把一次 Chat Completions 响应折叠成 Responses 对象（非流式路径）。
//
// customNames 里声明的工具调用会被还原成 custom_tool_call 项，namespace 也会
// 按 meta.NamespaceMap 补回——两者都是客户端派发工具的必要字段。
func FromChat(chat map[string]any, meta *Meta, model string) map[string]any {
	choice := firstChoice(chat)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	text := stringOf(msg["content"])
	reasoning := stringOf(msg["reasoning_content"])

	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      newID("rs_"),
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name := stringOf(fn["name"])
			callID := stringOf(tc["id"])
			if callID == "" {
				callID = newID("call_")
			}
			if meta.IsCustomTool(name) {
				output = append(output, map[string]any{
					"id":      newID("ctc_"),
					"type":    "custom_tool_call",
					"status":  "completed",
					"call_id": callID,
					"name":    name,
					"input":   UnwrapCustomInput(stringOf(fn["arguments"])),
				})
			} else {
				args := stringOf(fn["arguments"])
				if args == "" {
					args = "{}"
				}
				output = append(output, map[string]any{
					"id":        newID("fc_"),
					"type":      "function_call",
					"status":    "completed",
					"call_id":   callID,
					"name":      name,
					"arguments": args,
				})
			}
		}
	}
	if text != "" || len(output) == 0 {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		output = append(output, map[string]any{
			"id":      newID("msg_"),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": content,
		})
	}
	if meta != nil && len(meta.NamespaceMap) > 0 {
		ApplyNamespaceToCalls(output, meta.NamespaceMap)
	}

	finish := stringOf(choice["finish_reason"])
	if finish == "" {
		finish = "stop"
	}
	obj := map[string]any{
		"id":         newID("resp_"),
		"object":     "response",
		"created_at": nowUnix(),
		"status":     statusForFinish(finish),
		"model":      model,
		"output":     output,
		"output_text": text,
		"metadata":   map[string]any{},
	}
	applyRequestMeta(obj, meta)
	if u, ok := usageFromChat(chat["usage"]); ok {
		obj["usage"] = u
	}
	if finish == "length" {
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return obj
}

// applyRequestMeta 回显客户端请求的 tools / tool_choice / parallel_tool_calls。
//
// 早期实现把这三项硬编码成空数组与默认值，客户端拿到的是「自己没要求过」的
// 声明，会在校验环节出错——这里原样回显。
func applyRequestMeta(obj map[string]any, meta *Meta) {
	if meta == nil {
		obj["parallel_tool_calls"] = true
		obj["tool_choice"] = "auto"
		obj["tools"] = []any{}
		return
	}
	if meta.ParallelToolCalls != nil {
		obj["parallel_tool_calls"] = meta.ParallelToolCalls
	} else {
		obj["parallel_tool_calls"] = true
	}
	if meta.ToolChoice != nil {
		obj["tool_choice"] = meta.ToolChoice
	} else {
		obj["tool_choice"] = "auto"
	}
	if meta.Tools != nil {
		obj["tools"] = meta.Tools
	} else {
		obj["tools"] = []any{}
	}
}

// statusForFinish 把 Chat 的 finish_reason 映射成 Responses 的 status。
func statusForFinish(finish string) string {
	switch finish {
	case "length":
		return "incomplete"
	case "content_filter":
		return "incomplete"
	default:
		return "completed"
	}
}

// firstChoice 取 choices[0]（缺省返回空 map）。
func firstChoice(chat map[string]any) map[string]any {
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return map[string]any{}
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return map[string]any{}
	}
	return choice
}

// usageFromChat 把 Chat usage 映射成 Responses usage。
func usageFromChat(raw any) (map[string]any, bool) {
	u, ok := raw.(map[string]any)
	if !ok || len(u) == 0 {
		return nil, false
	}
	det, _ := u["completion_tokens_details"].(map[string]any)
	pdet, _ := u["prompt_tokens_details"].(map[string]any)
	cached := firstNumber(u["prompt_cache_hit_tokens"], det["cached_tokens"], pdet["cached_tokens"])
	return map[string]any{
		"input_tokens":  numOrZero(u["prompt_tokens"]),
		"output_tokens": numOrZero(u["completion_tokens"]),
		"total_tokens":  numOrZero(u["total_tokens"]),
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": numOrZero(det["reasoning_tokens"]),
		},
	}, true
}

// numOrZero 宽松取数字（JSON 数字统一是 float64）。
func numOrZero(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// firstNumber 返回第一个可解析为数字的值。
func firstNumber(vals ...any) float64 {
	for _, v := range vals {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

// chatDelta 从 chat chunk 里取 delta（无 choices 时返回 nil）。
func chatDelta(chunk map[string]any) map[string]any {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil
	}
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

// chatFinish 取 finish_reason。
func chatFinish(chunk map[string]any) string {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	return stringOf(choice["finish_reason"])
}

// jsonString 序列化并保证 UTF-8 不转义（与网关其余部分一致的中文可读性）。
func jsonString(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(sb.String(), "\n")
}
