// responses.go Codex Responses 入站适配：POST /v1/responses ↔ 现有 chatCompletions。
// ponytail: 不存 previous_response_id / store；Codex 每轮带全量 input。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chat, &peek)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := &responsesWriter{ResponseWriter: w, stream: peek.Stream}
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

func responsesToChat(src []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range []string{
		"model", "stream", "tool_choice", "max_tokens", "max_output_tokens",
		"max_completion_tokens", "temperature", "top_p", "user", "n", "stop",
		"metadata", "stream_options", "parallel_tool_calls",
		// ponytail: Codex 客户端按会话发 prompt_cache_key；丢掉就只能靠内容派生会话键。
		"prompt_cache_key",
	} {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"]; ok {
			out["reasoning_effort"] = e
		}
	}
	if v, ok := obj["reasoning_effort"]; ok {
		out["reasoning_effort"] = v
	}
	if tools, ok := obj["tools"].([]any); ok {
		out["tools"] = convertTools(tools)
	}
	msgs := []any{}
	if inst, ok := obj["instructions"].(string); ok && inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch in := obj["input"].(type) {
	case string:
		if in != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	case []any:
		msgs = append(msgs, convertInputItems(in)...)
	}
	out["messages"] = msgs
	return json.Marshal(out)
}

func convertTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		if _, ok := m["function"].(map[string]any); ok {
			out = append(out, m)
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" || typ == "function" {
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := m[k]; ok {
					fn[k] = v
				}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		out = append(out, m)
	}
	return out
}

func convertInputItems(items []any) []any {
	var out []any
	var pending []any
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "assistant", "content": "", "tool_calls": pending})
		pending = nil
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "reasoning", "item_reference":
			flush()
			continue
		case "function_call":
			pending = append(pending, toToolCall(m))
			continue
		case "function_call_output":
			flush()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": asString(m["call_id"]),
				"content":      asContentString(m["output"]),
			})
			continue
		}
		flush()
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{"role": role, "content": convertContent(m["content"])})
	}
	flush()
	return out
}

func toToolCall(m map[string]any) map[string]any {
	callID := asString(m["call_id"])
	if callID == "" {
		callID = asString(m["id"])
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      asString(m["name"]),
			"arguments": asString(m["arguments"]),
		},
	}
}

func convertContent(c any) any {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var texts []string
		var parts []any
		hasNonText := false
		for _, p := range v {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "":
				t := asString(m["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "input_image":
				hasNonText = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURLFrom(m)},
				})
			}
		}
		if !hasNonText {
			return strings.Join(texts, "")
		}
		return parts
	default:
		return asContentString(c)
	}
}

func imageURLFrom(m map[string]any) string {
	switch u := m["image_url"].(type) {
	case string:
		return u
	case map[string]any:
		return asString(u["url"])
	}
	return asString(m["url"])
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func asContentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m, ok := p.(map[string]any); ok {
				b.WriteString(asString(m["text"]))
			}
		}
		return b.String()
	default:
		return asString(v)
	}
}

func chatCompletionToResponse(obj map[string]any) map[string]any {
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	var created int64
	switch v := obj["created"].(type) {
	case float64:
		created = int64(v)
	case int64:
		created = v
	case int:
		created = int64(v)
	}
	var msg map[string]any
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
		}
	}
	output := []any{}
	if msg != nil {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			output = append(output, map[string]any{
				"id":      "rs_" + id,
				"type":    "reasoning",
				"summary": []any{},
				"content": []any{map[string]any{"type": "reasoning_text", "text": rc}},
			})
		}
		content, _ := msg["content"].(string)
		tcs, _ := msg["tool_calls"].([]any)
		if content != "" || len(tcs) == 0 {
			output = append(output, map[string]any{
				"id":      "msg_" + id,
				"type":    "message",
				"status":  "completed",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": content}},
			})
		}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name, args := "", ""
			if fn != nil {
				name = asString(fn["name"])
				args = asString(fn["arguments"])
			}
			callID := asString(tc["id"])
			output = append(output, map[string]any{
				"id":        callID,
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": args,
				"status":    "completed",
			})
		}
	}
	out := map[string]any{
		"id":         respID(id),
		"object":     "response",
		"created_at": created,
		"status":     "completed",
		"model":      model,
		"output":     output,
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		out["usage"] = convertUsage(u)
	}
	return out
}

func convertUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = v
	}
	if v, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = v
	}
	if v, ok := u["total_tokens"]; ok {
		out["total_tokens"] = v
	}
	// ponytail: Responses 客户端的缓存读数在 input_tokens_details.cached_tokens
	// （Codex 据此显示 cached），上游已在 prompt_tokens_details.cached_tokens 给出。
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if c, ok := d["cached_tokens"]; ok {
			out["input_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	return out
}

func respID(chatID string) string {
	if chatID == "" {
		chatID = "wb2api"
	}
	if strings.HasPrefix(chatID, "resp_") {
		return chatID
	}
	return "resp_" + chatID
}

type responsesWriter struct {
	http.ResponseWriter
	stream  bool
	status  int
	hdrSent bool
	rest    []byte
	body    []byte
	x       streamState
}

type streamState struct {
	created, failed, completed bool
	id, model                  string
	outN                       int
	reasoningOpen              bool
	reasoningIdx               int
	reasoningID                string
	reasoningText              strings.Builder
	messageOpen, textOpen      bool
	messageIdx                 int
	msgID                      string
	messageText                strings.Builder
	tools                      map[int]*toolAcc
	toolOrder                  []int
	usage                      map[string]any
	output                     []any
}

type toolAcc struct {
	idx, outIdx    int
	id, name       string
	args           strings.Builder
	opened, closed bool
}

func (w *responsesWriter) Header() http.Header { return w.ResponseWriter.Header() }

func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.stream && strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		w.rest = append(w.rest, p...)
		for {
			i := bytes.Index(w.rest, []byte("\n\n"))
			if i < 0 {
				break
			}
			frame := string(bytes.TrimSpace(w.rest[:i]))
			w.rest = w.rest[i+2:]
			if err := w.handleFrame(frame); err != nil {
				return len(p), err
			}
		}
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *responsesWriter) finish() {
	if w.stream {
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") || w.hdrSent {
			if w.x.created && !w.x.completed && !w.x.failed {
				_ = w.closeOpenItems()
				_ = w.emitCompleted()
			}
			return
		}
		_ = w.emitJSONErrorAsFailed()
		return
	}
	w.flushJSON()
}

func (w *responsesWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	if len(w.body) == 0 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(w.body, &obj) != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	raw, err := json.Marshal(chatCompletionToResponse(obj))
	if err != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(raw)
}

func (w *responsesWriter) emitJSONErrorAsFailed() error {
	var obj map[string]any
	_ = json.Unmarshal(w.body, &obj)
	return w.emitFailed(obj["error"])
}

func (w *responsesWriter) handleFrame(frame string) error {
	if w.x.failed || w.x.completed {
		return nil
	}
	if strings.HasPrefix(frame, "data: [DONE]") {
		_ = w.closeOpenItems()
		return w.emitCompleted()
	}
	payload, ok := strings.CutPrefix(frame, "data: ")
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	if errObj, ok := obj["error"]; ok {
		return w.emitFailed(errObj)
	}
	return w.handleChunk(obj)
}

func (w *responsesWriter) handleChunk(obj map[string]any) error {
	if !w.x.created {
		id, _ := obj["id"].(string)
		model, _ := obj["model"].(string)
		w.x.id = id
		w.x.model = model
		w.x.created = true
		if err := w.emit("response.created", map[string]any{
			"response": map[string]any{
				"id":     respID(id),
				"object": "response",
				"status": "in_progress",
				"model":  model,
				"output": []any{},
			},
		}); err != nil {
			return err
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		w.x.usage = u
	}
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return nil
	}
	if delta, _ := c["delta"].(map[string]any); delta != nil {
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			if err := w.ensureReasoning(); err != nil {
				return err
			}
			w.x.reasoningText.WriteString(rc)
			if err := w.emit("response.reasoning_text.delta", map[string]any{
				"item_id":       w.x.reasoningID,
				"output_index":  w.x.reasoningIdx,
				"content_index": 0,
				"delta":         rc,
			}); err != nil {
				return err
			}
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if err := w.ensureMessageText(); err != nil {
				return err
			}
			w.x.messageText.WriteString(txt)
			if err := w.emit("response.output_text.delta", map[string]any{
				"item_id":       w.x.msgID,
				"output_index":  w.x.messageIdx,
				"content_index": 0,
				"delta":         txt,
			}); err != nil {
				return err
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if err := w.handleToolDeltas(tcs); err != nil {
				return err
			}
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		return w.closeOpenItems()
	}
	return nil
}

func (w *responsesWriter) nextIdx() int {
	i := w.x.outN
	w.x.outN++
	return i
}

func (w *responsesWriter) ensureReasoning() error {
	if w.x.reasoningOpen {
		return nil
	}
	w.x.reasoningIdx = w.nextIdx()
	w.x.reasoningID = "rs_" + w.x.id
	w.x.reasoningOpen = true
	return w.emit("response.output_item.added", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item": map[string]any{
			"id":      w.x.reasoningID,
			"type":    "reasoning",
			"summary": []any{},
			"content": []any{},
		},
	})
}

func (w *responsesWriter) ensureMessageText() error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if w.x.textOpen {
		return nil
	}
	if !w.x.messageOpen {
		w.x.messageIdx = w.nextIdx()
		w.x.msgID = "msg_" + w.x.id
		w.x.messageOpen = true
		if err := w.emit("response.output_item.added", map[string]any{
			"output_index": w.x.messageIdx,
			"item": map[string]any{
				"id":      w.x.msgID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}); err != nil {
			return err
		}
	}
	w.x.textOpen = true
	return w.emit("response.content_part.added", map[string]any{
		"item_id":       w.x.msgID,
		"output_index":  w.x.messageIdx,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	})
}

func (w *responsesWriter) handleToolDeltas(tcs []any) error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	if w.x.tools == nil {
		w.x.tools = map[int]*toolAcc{}
	}
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		acc, ok := w.x.tools[idx]
		if !ok {
			acc = &toolAcc{idx: idx}
			w.x.tools[idx] = acc
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if id := asString(tc["id"]); id != "" {
			acc.id = id
		}
		if fn, _ := tc["function"].(map[string]any); fn != nil {
			if n := asString(fn["name"]); n != "" {
				acc.name = n
			}
			if a, ok := fn["arguments"].(string); ok && a != "" {
				acc.args.WriteString(a)
				if !acc.opened {
					if err := w.openTool(acc); err != nil {
						return err
					}
				}
				if err := w.emit("response.function_call_arguments.delta", map[string]any{
					"item_id":      acc.id,
					"output_index": acc.outIdx,
					"delta":        a,
				}); err != nil {
					return err
				}
				continue
			}
		}
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *responsesWriter) openTool(acc *toolAcc) error {
	if acc.opened {
		return nil
	}
	acc.outIdx = w.nextIdx()
	if acc.id == "" {
		acc.id = "call_" + asString(acc.idx)
	}
	acc.opened = true
	return w.emit("response.output_item.added", map[string]any{
		"output_index": acc.outIdx,
		"item": map[string]any{
			"id":        acc.id,
			"type":      "function_call",
			"status":    "in_progress",
			"call_id":   acc.id,
			"name":      acc.name,
			"arguments": "",
		},
	})
}

func (w *responsesWriter) closeOpenItems() error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	for _, idx := range w.x.toolOrder {
		acc := w.x.tools[idx]
		if acc == nil || acc.closed {
			continue
		}
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
		if err := w.emit("response.function_call_arguments.done", map[string]any{
			"item_id":      acc.id,
			"output_index": acc.outIdx,
			"arguments":    acc.args.String(),
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id":        acc.id,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   acc.id,
			"name":      acc.name,
			"arguments": acc.args.String(),
		}
		if err := w.emit("response.output_item.done", map[string]any{
			"output_index": acc.outIdx,
			"item":         item,
		}); err != nil {
			return err
		}
		w.x.output = append(w.x.output, item)
		acc.closed = true
	}
	return nil
}

func (w *responsesWriter) closeReasoning() error {
	if !w.x.reasoningOpen {
		return nil
	}
	if err := w.emit("response.reasoning_text.done", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"content_index": 0,
		"text":          w.x.reasoningText.String(),
	}); err != nil {
		return err
	}
	item := map[string]any{
		"id":      w.x.reasoningID,
		"type":    "reasoning",
		"summary": []any{},
		"content": []any{map[string]any{"type": "reasoning_text", "text": w.x.reasoningText.String()}},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.reasoningOpen = false
	return nil
}

func (w *responsesWriter) closeMessage() error {
	if !w.x.messageOpen {
		return nil
	}
	if w.x.textOpen {
		if err := w.emit("response.output_text.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
			"text":          w.x.messageText.String(),
		}); err != nil {
			return err
		}
		if err := w.emit("response.content_part.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
		}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	item := map[string]any{
		"id":     w.x.msgID,
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": w.x.messageText.String()},
		},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.messageIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.messageOpen = false
	return nil
}

func (w *responsesWriter) emitCompleted() error {
	if w.x.completed || w.x.failed {
		return nil
	}
	w.x.completed = true
	resp := map[string]any{
		"id":     respID(w.x.id),
		"object": "response",
		"status": "completed",
		"model":  w.x.model,
		"output": w.x.output,
	}
	if w.x.usage != nil {
		resp["usage"] = convertUsage(w.x.usage)
	}
	return w.emit("response.completed", map[string]any{"response": resp})
}

func (w *responsesWriter) emitFailed(errObj any) error {
	if w.x.failed {
		return nil
	}
	w.x.failed = true
	if errObj == nil {
		errObj = map[string]any{"message": "upstream error", "type": "api_error"}
	}
	return w.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id":     respID(w.x.id),
			"object": "response",
			"status": "failed",
			"error":  errObj,
		},
	})
}

func (w *responsesWriter) emit(typ string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// ponytail: 不显式 WriteHeader——下面的 io.WriteString 会隐式提交 200。
		// 流式路径的 Flush（StreamHint 逐帧 flush）可能已先提交，再显式调用就是
		// net/http 的 "superfluous response.WriteHeader call" 噪音。
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, "event: "+typ+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	w.Flush()
	return nil
}
