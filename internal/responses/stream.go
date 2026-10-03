package responses

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ErrEmptyStream 上游返回 200 但没有任何有效帧。
var ErrEmptyStream = errors.New("responses: empty upstream stream")

// toolState 一次工具调用的流式累积状态。
type toolState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   string
	custom      bool
	started     bool
}

// streamState 一次 Responses 流式响应的全部状态。
type streamState struct {
	w      io.Writer
	meta   *Meta
	model  string
	seq    int
	respID string
	msgID  string
	rsID   string
	created int64

	outputs     []any
	reasonIndex int
	msgIndex    int
	textParts   []string
	reasonParts []string
	tools       map[int]*toolState
	usage       map[string]any
	finish      string
	frames      int
}

// Stream 把上游 Chat Completions 的 SSE 流翻译成 Responses API 的事件流。
//
// 事件序列与官方 Responses 流式形态对齐：response.created / in_progress →
// output_item.added（reasoning / message / function_call / custom_tool_call）→
// 各类 delta → 对应的 .done 事件 → response.completed。
func Stream(w io.Writer, src io.Reader, meta *Meta, model string) error {
	st := &streamState{
		w:           w,
		meta:        meta,
		model:       model,
		respID:      newID("resp_"),
		msgID:       newID("msg_"),
		rsID:        newID("rs_"),
		created:     nowUnix(),
		reasonIndex: -1,
		msgIndex:    -1,
		tools:       map[int]*toolState{},
	}
	if err := st.emit("response.created", map[string]any{"response": st.responseObject("in_progress")}); err != nil {
		return err
	}
	if err := st.emit("response.in_progress", map[string]any{"response": st.responseObject("in_progress")}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 非 JSON 帧：上游噪声，跳过（与既有 Stream 语义一致）
		}
		if errObj, ok := chunk["error"]; ok && errObj != nil {
			return st.fail(fmt.Sprintf("upstream error: %s", jsonString(errObj)))
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			st.usage = u
		}
		if fin := chatFinish(chunk); fin != "" {
			st.finish = fin
		}
		delta := chatDelta(chunk)
		if delta == nil {
			continue
		}
		st.frames++
		if piece := stringOf(delta["reasoning_content"]); piece != "" {
			if err := st.emitReasoningDelta(piece); err != nil {
				return err
			}
		}
		if piece := stringOf(delta["content"]); piece != "" {
			if err := st.emitTextDelta(piece); err != nil {
				return err
			}
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range calls {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if err := st.emitToolDelta(tc); err != nil {
					return err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if st.frames == 0 {
		return ErrEmptyStream
	}
	return st.finalize()
}

// emit 写一帧 SSE（event: <name> + data: {...}），并尽力 flush。
func (st *streamState) emit(name string, payload map[string]any) error {
	st.seq++
	data := make(map[string]any, len(payload)+2)
	data["type"] = name
	data["sequence_number"] = st.seq
	for k, v := range payload {
		data[k] = v
	}
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	if f, ok := st.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// fail 发出 response.failed 并返回错误。
func (st *streamState) fail(msg string) error {
	obj := st.responseObject("failed")
	obj["error"] = map[string]any{"code": "upstream_error", "message": msg}
	_ = st.emit("response.failed", map[string]any{"response": obj})
	return errors.New("responses: " + msg)
}

// responseObject 组装当前状态下的 response 对象。
func (st *streamState) responseObject(status string) map[string]any {
	output := make([]any, 0, len(st.outputs))
	for _, item := range st.outputs {
		if item != nil {
			output = append(output, item)
		}
	}
	obj := map[string]any{
		"id":          st.respID,
		"object":      "response",
		"created_at":  st.created,
		"status":      status,
		"model":       st.model,
		"output":      output,
		"output_text": strings.Join(st.textParts, ""),
		"metadata":    map[string]any{},
	}
	applyRequestMeta(obj, st.meta)
	if u, ok := usageFromChat(st.usage); ok {
		obj["usage"] = u
	}
	if st.meta != nil && len(st.meta.NamespaceMap) > 0 {
		ApplyNamespaceToCalls(output, st.meta.NamespaceMap)
	}
	return obj
}

// reasoningItem 组装 reasoning 输出项。
func (st *streamState) reasoningItem(status string) map[string]any {
	return map[string]any{
		"id":     st.rsID,
		"type":   "reasoning",
		"status": status,
		"summary": []any{
			map[string]any{"type": "summary_text", "text": strings.Join(st.reasonParts, "")},
		},
	}
}

// messageItem 组装 message 输出项。
func (st *streamState) messageItem(status string) map[string]any {
	content := []any{}
	if text := strings.Join(st.textParts, ""); text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{
		"id":      st.msgID,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": content,
	}
}

// emitReasoningDelta 累积并发出 reasoning 增量。
func (st *streamState) emitReasoningDelta(piece string) error {
	if st.reasonIndex < 0 {
		st.reasonIndex = len(st.outputs)
		st.outputs = append(st.outputs, nil)
		if err := st.emit("response.output_item.added", map[string]any{
			"output_index": st.reasonIndex,
			"item":         st.reasoningItem("in_progress"),
		}); err != nil {
			return err
		}
		if err := st.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id":       st.rsID,
			"output_index":  st.reasonIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		}); err != nil {
			return err
		}
	}
	st.reasonParts = append(st.reasonParts, piece)
	return st.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       st.rsID,
		"output_index":  st.reasonIndex,
		"summary_index": 0,
		"delta":         piece,
	})
}

// emitTextDelta 累积并发出正文增量。
func (st *streamState) emitTextDelta(piece string) error {
	if st.msgIndex < 0 {
		st.msgIndex = len(st.outputs)
		st.outputs = append(st.outputs, nil)
		if err := st.emit("response.output_item.added", map[string]any{
			"output_index": st.msgIndex,
			"item": map[string]any{
				"id": st.msgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}); err != nil {
			return err
		}
		if err := st.emit("response.content_part.added", map[string]any{
			"item_id":       st.msgID,
			"output_index":  st.msgIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}); err != nil {
			return err
		}
	}
	st.textParts = append(st.textParts, piece)
	return st.emit("response.output_text.delta", map[string]any{
		"item_id":       st.msgID,
		"output_index":  st.msgIndex,
		"content_index": 0,
		"delta":         piece,
	})
}

// emitToolDelta 处理一次工具调用增量（首次出现时发 output_item.added）。
func (st *streamState) emitToolDelta(tc map[string]any) error {
	idx := 0
	if f, ok := tc["index"].(float64); ok {
		idx = int(f)
	}
	fn, _ := tc["function"].(map[string]any)
	fnName := stringOf(fn["name"])
	fnArgs := stringOf(fn["arguments"])
	callID := stringOf(tc["id"])

	entry, ok := st.tools[idx]
	if !ok {
		callID = orDefault(callID, newID("call_"))
		custom := st.meta.IsCustomTool(fnName)
		itemID := newID("fc_")
		if custom {
			itemID = newID("ctc_")
		}
		entry = &toolState{
			outputIndex: len(st.outputs),
			itemID:      itemID,
			callID:      callID,
			name:        fnName,
			custom:      custom,
		}
		st.tools[idx] = entry
		st.outputs = append(st.outputs, nil)

		item := map[string]any{
			"id":      entry.itemID,
			"status":  "in_progress",
			"call_id": entry.callID,
			"name":    entry.name,
		}
		if custom {
			item["type"] = "custom_tool_call"
			item["input"] = ""
		} else {
			item["type"] = "function_call"
			item["arguments"] = ""
		}
		// namespace 必须在 output_item.added 就带上：客户端是从 added 事件
		// 派发工具执行器的，事后再补只改到 done，调用已经失败了。
		if st.meta != nil {
			StampNamespace(item, st.meta.NamespaceMap)
		}
		if err := st.emit("response.output_item.added", map[string]any{
			"output_index": entry.outputIndex,
			"item":         item,
		}); err != nil {
			return err
		}
	} else {
		if fnName != "" && entry.name == "" {
			entry.name = fnName
			if st.meta.IsCustomTool(fnName) {
				entry.custom = true
			}
		}
		if callID != "" && entry.callID == "" {
			entry.callID = callID
		}
	}
	if fnArgs == "" {
		return nil
	}
	entry.arguments += fnArgs
	if entry.custom {
		return st.emit("response.custom_tool_call_input.delta", map[string]any{
			"output_index": entry.outputIndex,
			"item_id":      entry.itemID,
			"call_id":      entry.callID,
			"delta":        fnArgs,
		})
	}
	return st.emit("response.function_call_arguments.delta", map[string]any{
		"output_index": entry.outputIndex,
		"item_id":      entry.itemID,
		"call_id":      entry.callID,
		"delta":        fnArgs,
	})
}

// finalize 收尾：补完 reasoning / 工具调用 / message，并发出 response.completed。
func (st *streamState) finalize() error {
	if st.reasonIndex >= 0 && st.outputs[st.reasonIndex] == nil {
		full := strings.Join(st.reasonParts, "")
		if err := st.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": st.rsID, "output_index": st.reasonIndex, "summary_index": 0, "text": full,
		}); err != nil {
			return err
		}
		if err := st.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": st.rsID, "output_index": st.reasonIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": full},
		}); err != nil {
			return err
		}
		item := st.reasoningItem("completed")
		st.outputs[st.reasonIndex] = item
		if err := st.emit("response.output_item.done", map[string]any{
			"output_index": st.reasonIndex, "item": item,
		}); err != nil {
			return err
		}
	}

	idxs := make([]int, 0, len(st.tools))
	for idx := range st.tools {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		entry := st.tools[idx]
		if entry.started {
			continue
		}
		entry.started = true
		var item map[string]any
		if entry.custom {
			input := UnwrapCustomInput(entry.arguments)
			if err := st.emit("response.custom_tool_call_input.done", map[string]any{
				"output_index": entry.outputIndex,
				"item_id":      entry.itemID,
				"call_id":      entry.callID,
				"input":        input,
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id": entry.itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "input": input,
			}
		} else {
			args := orDefault(entry.arguments, "{}")
			if err := st.emit("response.function_call_arguments.done", map[string]any{
				"output_index": entry.outputIndex,
				"item_id":      entry.itemID,
				"call_id":      entry.callID,
				"arguments":    args,
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id": entry.itemID, "type": "function_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "arguments": args,
			}
		}
		if st.meta != nil {
			StampNamespace(item, st.meta.NamespaceMap)
		}
		st.outputs[entry.outputIndex] = item
		if err := st.emit("response.output_item.done", map[string]any{
			"output_index": entry.outputIndex, "item": item,
		}); err != nil {
			return err
		}
	}

	fullText := strings.Join(st.textParts, "")
	hasOther := false
	for _, item := range st.outputs {
		if item != nil {
			hasOther = true
			break
		}
	}
	if st.msgIndex >= 0 || fullText != "" || !hasOther {
		if st.msgIndex < 0 {
			st.msgIndex = len(st.outputs)
			st.outputs = append(st.outputs, nil)
			if err := st.emit("response.output_item.added", map[string]any{
				"output_index": st.msgIndex,
				"item": map[string]any{
					"id": st.msgID, "type": "message", "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			}); err != nil {
				return err
			}
			if err := st.emit("response.content_part.added", map[string]any{
				"item_id":       st.msgID,
				"output_index":  st.msgIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}); err != nil {
				return err
			}
		}
		if err := st.emit("response.output_text.done", map[string]any{
			"item_id": st.msgID, "output_index": st.msgIndex, "content_index": 0, "text": fullText,
		}); err != nil {
			return err
		}
		if err := st.emit("response.content_part.done", map[string]any{
			"item_id":       st.msgID,
			"output_index":  st.msgIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": fullText, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := st.messageItem("completed")
		st.outputs[st.msgIndex] = item
		if err := st.emit("response.output_item.done", map[string]any{
			"output_index": st.msgIndex, "item": item,
		}); err != nil {
			return err
		}
	}

	status := statusForFinish(st.finish)
	final := st.responseObject(status)
	if status == "incomplete" {
		final["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return st.emit("response.completed", map[string]any{"response": final})
}

// orDefault 返回 s，空串时返回 def。
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
