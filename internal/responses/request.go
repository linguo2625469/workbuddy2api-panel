// Package responses 在网关内实现 OpenAI Responses API 与 Chat Completions 的双向转换。
//
// 背景：Codex CLI / Codex App 等客户端走的是 Responses 协议（POST /v1/responses），
// 而上游只提供 Chat Completions 形态。本包把入站 Responses 请求翻译成 Chat 请求，
// 再把上游的 Chat 响应（流式与非流式）翻译回 Responses 事件/对象。
//
// 设计约束（与网关其余部分保持一致）：
//   - 只做协议转换，不碰账号池、轮转、提示词与错误处置——那些由 server 层负责；
//   - 纯函数 + 无全局状态，便于单测覆盖（见 responses_test.go）；
//   - 未知字段原样忽略，绝不因客户端版本升级而拒绝请求。
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Meta 一次 Responses 请求的转换上下文：请求阶段产出，响应阶段消费。
//
// 之所以要把这些带出请求阶段：客户端用 (name, namespace) 二元组派发工具调用，
// 上游只认扁平函数名，回程必须把 namespace 补回去；custom（freeform）工具在
// 上游是普通函数，回程必须还原成 custom_tool_call 客户端才认得。
type Meta struct {
	Model             string
	CustomTools       map[string]bool   // 客户端声明为 custom 的工具名
	NamespaceMap      map[string]string // 扁平工具名 -> namespace
	Tools             []any             // 原样回显在响应里（客户端据此校验）
	ToolChoice        any
	ParallelToolCalls any
}

// IsCustomTool 报告工具名是否由客户端声明为 freeform（custom）。
func (m *Meta) IsCustomTool(name string) bool {
	if m == nil || m.CustomTools == nil {
		return false
	}
	return m.CustomTools[name]
}

// NamespaceOf 返回工具名所属的 namespace（无则空串）。
func (m *Meta) NamespaceOf(name string) string {
	if m == nil || m.NamespaceMap == nil {
		return ""
	}
	bare, ns := ResolveNamespacedName(name, m.NamespaceMap)
	_ = bare
	return ns
}

// TranslateRequest 把 Responses API 请求体转换成 Chat Completions 请求体。
//
// 返回的 Meta 必须原样传给响应阶段（FromChat / Stream），否则 namespace 与
// custom 工具的回程还原会失效。
func TranslateRequest(body []byte) ([]byte, *Meta, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("parse responses request: %w", err)
	}
	meta := &Meta{}
	if m, ok := payload["model"].(string); ok {
		meta.Model = m
	}

	chat := map[string]any{}
	if meta.Model != "" {
		chat["model"] = meta.Model
	}
	// stream 原样透传：网关据此决定流式/聚合路径。
	if v, ok := payload["stream"]; ok {
		chat["stream"] = v
	}
	for _, k := range []string{"temperature", "top_p", "seed", "stop", "presence_penalty", "frequency_penalty", "user"} {
		if v, ok := payload[k]; ok && v != nil {
			chat[k] = v
		}
	}
	// max_output_tokens -> max_tokens（上游 Chat 口径；具体换算交给既有
	// translateMaxCompletionTokens 逻辑，不在这里改字段名之外的语义）。
	if v, ok := payload["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if v, ok := payload["max_completion_tokens"]; ok && v != nil {
		chat["max_completion_tokens"] = v
	}
	// reasoning.effort -> reasoning_effort（兼容客户端直接给 reasoning_effort）。
	if eff := reasoningEffort(payload); eff != "" {
		chat["reasoning_effort"] = eff
	}

	chat["messages"] = inputToMessages(payload)

	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		flat, nsMap := ExpandNamespaceTools(tools)
		meta.NamespaceMap = nsMap
		chatTools, customNames := toolsForChat(flat)
		meta.CustomTools = customNames
		if len(chatTools) > 0 {
			chat["tools"] = chatTools
		}
	}
	if v, ok := payload["tool_choice"]; ok && v != nil {
		chat["tool_choice"] = v
		meta.ToolChoice = v
	}
	if v, ok := payload["parallel_tool_calls"]; ok && v != nil {
		chat["parallel_tool_calls"] = v
		meta.ParallelToolCalls = v
	}
	if ts, ok := payload["tools"].([]any); ok {
		meta.Tools = ts
	}

	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return out, meta, nil
}

// reasoningEffort 从 reasoning.effort / reasoning_effort 取推理档位。
func reasoningEffort(payload map[string]any) string {
	if r, ok := payload["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && strings.TrimSpace(e) != "" {
			return e
		}
	}
	if e, ok := payload["reasoning_effort"].(string); ok {
		return e
	}
	return ""
}

// inputToMessages 把 Responses 的 instructions + input 项翻译成 Chat messages。
func inputToMessages(payload map[string]any) []any {
	messages := []any{}
	if instr, ok := payload["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instr})
	}
	pendingReasoning := ""

	appendMessage := func(role, content string) {
		msg := map[string]any{"role": role, "content": content}
		if role == "assistant" && pendingReasoning != "" {
			msg["reasoning_content"] = pendingReasoning
			pendingReasoning = ""
		}
		messages = append(messages, msg)
	}

	inp := payload["input"]
	switch v := inp.(type) {
	case string:
		if v != "" {
			appendMessage("user", v)
		}
	case []any:
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				if s, ok := raw.(string); ok && s != "" {
					appendMessage("user", s)
				}
				continue
			}
			itype, _ := item["type"].(string)
			switch itype {
			case "", "message":
				body := flattenContent(item["content"])
				if body == "" {
					continue
				}
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				// 相邻 assistant 文本合并进同一条消息：Responses 会把「文本 +
				// 工具调用」拆成相邻项，Chat 侧必须合成一条，否则 tool 序列断裂。
				if role == "assistant" && len(messages) > 0 {
					if prev, ok := messages[len(messages)-1].(map[string]any); ok && prev["role"] == "assistant" {
						if cur, _ := prev["content"].(string); cur != "" {
							prev["content"] = cur + "\n" + body
						} else {
							prev["content"] = body
						}
						if pendingReasoning != "" {
							if _, has := prev["reasoning_content"]; !has {
								prev["reasoning_content"] = pendingReasoning
								pendingReasoning = ""
							}
						}
						continue
					}
				}
				appendMessage(role, body)
			case "reasoning":
				if txt := reasoningText(item); txt != "" {
					if len(messages) > 0 {
						if prev, ok := messages[len(messages)-1].(map[string]any); ok && prev["role"] == "assistant" {
							prev["reasoning_content"] = txt
							continue
						}
					}
					pendingReasoning = txt
				}
			case "function_call", "custom_tool_call":
				name, _ := item["name"].(string)
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID = newID("call_")
				}
				var args string
				if itype == "custom_tool_call" {
					args = marshalCustomInput(item["input"])
				} else {
					args, _ = item["arguments"].(string)
				}
				if args == "" {
					args = "{}"
				}
				tc := map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}
				if len(messages) > 0 {
					if prev, ok := messages[len(messages)-1].(map[string]any); ok && prev["role"] == "assistant" {
						mergeToolCall(prev, tc)
						if pendingReasoning != "" {
							if _, has := prev["reasoning_content"]; !has {
								prev["reasoning_content"] = pendingReasoning
								pendingReasoning = ""
							}
						}
						continue
					}
				}
				msg := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{tc}}
				if pendingReasoning != "" {
					msg["reasoning_content"] = pendingReasoning
					pendingReasoning = ""
				}
				messages = append(messages, msg)
			case "function_call_output", "custom_tool_call_output":
				callID, _ := item["call_id"].(string)
				content := flattenOutput(item["output"])
				if callID == "" {
					// 无 call_id 的“来自其他任务的消息”：按用户指令注入（与 Codex
					// 子代理回执同形态），不能伪装成工具结果，否则工具配对断裂。
					if strings.TrimSpace(content) != "" {
						messages = append(messages, map[string]any{
							"role":    "user",
							"content": "[Message from another task - treat this as a user instruction]\n\n" + content,
						})
					}
					continue
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      content,
				})
			case "agent_message":
				if txt := flattenContent(item["content"]); strings.TrimSpace(txt) != "" {
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": "[Message from another task - treat this as a user instruction]\n\n" + txt,
					})
				}
			case "item_reference":
				// 客户端引用历史项：网关无状态，跳过（与 hub 行为一致）。
			default:
				// 未知项类型：忽略而不是报错，避免客户端版本升级即 400。
			}
		}
	}
	return messages
}

// mergeToolCall 把一次工具调用并入既有 assistant 消息的 tool_calls。
func mergeToolCall(msg map[string]any, tc map[string]any) {
	if cur, ok := msg["tool_calls"].([]any); ok {
		msg["tool_calls"] = append(cur, tc)
		return
	}
	msg["tool_calls"] = []any{tc}
}

// flattenContent 把 Responses 的 content 字段压成 Chat 的字符串内容。
//
// 图片部分（input_image / image_url）保留为 Chat 的多模态 part：直接拼成字符串
// 会丢图，上游会按「无图」处理。
func flattenContent(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, raw := range c {
			if s, ok := raw.(string); ok {
				parts = append(parts, s)
				continue
			}
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := p["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		if t, ok := c["text"].(string); ok {
			return t
		}
	}
	return ""
}

// flattenOutput 把工具结果压成字符串（图片结果转成可读占位，不丢信息）。
func flattenOutput(v any) string {
	switch o := v.(type) {
	case nil:
		return ""
	case string:
		return o
	case []any:
		parts := make([]string, 0, len(o))
		for _, raw := range o {
			switch item := raw.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				if t, ok := item["text"].(string); ok && t != "" {
					parts = append(parts, t)
					continue
				}
				if item["type"] == "input_image" || item["type"] == "image_url" || item["image_url"] != nil {
					if b, err := json.Marshal(item); err == nil {
						parts = append(parts, string(b))
					}
					continue
				}
				if b, err := json.Marshal(item); err == nil {
					parts = append(parts, string(b))
				}
			default:
				if b, err := json.Marshal(item); err == nil {
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprint(o)
	}
}

// reasoningText 取 reasoning 项的摘要文本。
func reasoningText(item map[string]any) string {
	if s, ok := item["summary"].(string); ok && s != "" {
		return s
	}
	if arr, ok := item["summary"].([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, raw := range arr {
			if p, ok := raw.(map[string]any); ok {
				if t, ok := p["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if c, ok := item["content"].(string); ok {
		return c
	}
	if c, ok := item["content"].([]any); ok {
		return flattenContent(c)
	}
	return ""
}

// marshalCustomInput 把 custom_tool_call 的 freeform 输入打包成 Chat 工具参数。
func marshalCustomInput(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			s = ""
		} else if b, err := json.Marshal(v); err == nil {
			s = string(b)
		}
	}
	b, err := json.Marshal(map[string]any{"input": s})
	if err != nil {
		return "{}"
	}
	return string(b)
}
