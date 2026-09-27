// messages.go Anthropic Messages 入站：POST /v1/messages 与 /messages ↔ 现有 chatCompletions。
// ponytail: 不存会话；客户端每轮带全量 messages。thinking 无 signature，不回传。
// top_k / cache_control / context_management / betas / count_tokens 上游 chat 没有对应物，丢掉。
// 内置服务端工具（web_search / code_execution / bash_* …）没有本地执行器，丢掉并在 system 里写明。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

func (h *Handler) withAnthropicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 官方 SDK 用 x-api-key，不是 Bearer。空 Authorization 时提升，再走同一套比较。
		if r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("x-api-key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	chat, err := messagesToChat(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chat, &peek)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := &messagesWriter{ResponseWriter: w, stream: peek.Stream}
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func messagesToChat(src []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range []string{"model", "stream", "temperature", "top_p", "max_tokens"} {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	if v, ok := obj["stop_sequences"]; ok {
		out["stop"] = v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if u, ok := md["user_id"]; ok {
			out["user"] = u
		}
	}
	switch tc := obj["tool_choice"].(type) {
	case string:
		out["tool_choice"] = tc
	case map[string]any:
		if c, ok := mapAnthropicToolChoice(tc); ok {
			out["tool_choice"] = c
		}
		if dis, ok := tc["disable_parallel_tool_use"].(bool); ok && dis {
			out["parallel_tool_calls"] = false
		}
	}
	var tools []any
	var skipped []string
	if raw, ok := obj["tools"].([]any); ok {
		tools, skipped = anthropicTools(raw)
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if e := effortFromMessages(obj); e != "" {
		out["reasoning_effort"] = e
	}
	msgs := []any{}
	if sys := systemText(obj["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	if note := skippedToolsNote(skipped); note != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": note})
	}
	if in, ok := obj["messages"].([]any); ok {
		msgs = append(msgs, convertAnthropicMessages(in)...)
	}
	out["messages"] = msgs
	return json.Marshal(out)
}

// effortFromMessages 把 Messages 的思考档收成上游已有的 reasoning_effort。
// output_config.effort 优先；旧式 budget_tokens 按 Hermes 的档位表折回。
// adaptive / enabled 没带档位时不发明档位，交给上游默认。disabled 显式 none。
func effortFromMessages(obj map[string]any) string {
	if oc, ok := obj["output_config"].(map[string]any); ok {
		if e := asString(oc["effort"]); e != "" {
			return e
		}
	}
	th, _ := obj["thinking"].(map[string]any)
	if th == nil {
		return ""
	}
	switch strings.ToLower(asString(th["type"])) {
	case "disabled":
		return "none"
	case "enabled":
		n, ok := th["budget_tokens"].(float64)
		if !ok || n <= 0 {
			return ""
		}
		switch {
		case n >= 32000:
			return "xhigh"
		case n >= 16000:
			return "high"
		case n >= 8000:
			return "medium"
		default:
			return "low"
		}
	default:
		return ""
	}
}

func skippedToolsNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "These Anthropic server tools are not available here: " + strings.Join(names, ", ") + ". Do not call them."
}

func systemText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for i, p := range t {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text := asString(m["text"])
			if text == "" {
				continue
			}
			if i > 0 && b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
		return b.String()
	default:
		return ""
	}
}

func mapAnthropicToolChoice(m map[string]any) (any, bool) {
	switch asString(m["type"]) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": asString(m["name"])}}, true
	default:
		return nil, false
	}
}

// anthropicTools 只保留客户端自己执行的工具。
// 带 input_schema 的（含 type=custom / 缺 type）原样变成 function。
// 没有 schema 的内置服务端工具没有本地执行器，返回名字供 system 说明，不发给上游。
func anthropicTools(tools []any) (out []any, skipped []string) {
	out = make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["function"].(map[string]any); ok {
			out = append(out, m)
			continue
		}
		if _, ok := m["input_schema"]; !ok {
			if n := toolLabel(m); n != "" {
				skipped = append(skipped, n)
			}
			continue
		}
		fn := map[string]any{"name": asString(m["name"])}
		if d, ok := m["description"]; ok {
			fn["description"] = d
		}
		fn["parameters"] = m["input_schema"]
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, skipped
}

func toolLabel(m map[string]any) string {
	if n := asString(m["name"]); n != "" {
		return n
	}
	return asString(m["type"])
}

func convertAnthropicMessages(in []any) []any {
	var out []any
	for _, it := range in {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			role = "user"
		}
		switch c := m["content"].(type) {
		case string:
			out = append(out, map[string]any{"role": role, "content": c})
		case []any:
			out = append(out, blocksToMessages(role, c)...)
		default:
			if s := asString(m["content"]); s != "" {
				out = append(out, map[string]any{"role": role, "content": s})
			}
		}
	}
	return out
}

func blocksToMessages(role string, blocks []any) []any {
	var toolCalls, toolMsgs, parts []any
	var texts []string
	hasNonText := false
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch asString(m["type"]) {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use", "server_tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   asString(m["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      asString(m["name"]),
					"arguments": mustJSON(m["input"]),
				},
			})
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": asString(m["tool_use_id"]),
				"content":      toolResultContent(m["content"], m["is_error"]),
			})
		case "image":
			hasNonText = true
			parts = append(parts, imagePart(m))
		case "document":
			hasNonText = true
			parts = append(parts, map[string]any{"type": "text", "text": documentText(m)})
		default:
			t := blockText(m)
			texts = append(texts, t)
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
	}
	out := append([]any{}, toolMsgs...)
	if len(toolCalls) == 0 && len(texts) == 0 && !hasNonText {
		return out
	}
	msg := map[string]any{"role": role}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if hasNonText {
		msg["content"] = parts
	} else if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	} else {
		msg["content"] = ""
	}
	return append(out, msg)
}

func imagePart(m map[string]any) map[string]any {
	url := ""
	if src, ok := m["source"].(map[string]any); ok {
		switch asString(src["type"]) {
		case "base64":
			url = "data:" + asString(src["media_type"]) + ";base64," + asString(src["data"])
		default:
			url = asString(src["url"])
		}
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func blockText(m map[string]any) string {
	if t := asString(m["text"]); t != "" || m["text"] != nil {
		return t
	}
	if t := asString(m["thinking"]); t != "" {
		return t
	}
	if c, ok := m["content"]; ok {
		return toolResultContent(c, nil)
	}
	return ""
}

func documentText(m map[string]any) string {
	title := asString(m["title"])
	src, _ := m["source"].(map[string]any)
	var body string
	if src != nil {
		switch asString(src["type"]) {
		case "text":
			body = asString(src["data"])
		case "content":
			body = toolResultContent(src["content"], nil)
		case "base64":
			body = "[document " + asString(src["media_type"]) + " omitted]"
		case "url":
			body = asString(src["url"])
		}
	}
	if title != "" {
		return title + "\n" + body
	}
	return body
}

func toolResultContent(v any, isError any) string {
	s := contentToString(v)
	if b, ok := isError.(bool); ok && b {
		return "error: " + s
	}
	return s
}

func contentToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch asString(m["type"]) {
			case "image":
				img := imagePart(m)
				if u, ok := img["image_url"].(map[string]any); ok {
					b.WriteString(asString(u["url"]))
				}
			case "document":
				b.WriteString(documentText(m))
			default:
				b.WriteString(blockText(m))
			}
		}
		return b.String()
	case map[string]any:
		if _, ok := t["type"]; ok {
			return blockText(t)
		}
		return mustJSON(t)
	default:
		return asString(v)
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func chatCompletionToMessage(obj map[string]any) map[string]any {
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	var msg map[string]any
	fr := ""
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			fr, _ = c["finish_reason"].(string)
		}
	}
	content := []any{}
	if msg != nil {
		// ponytail: reasoning_content 没有 signature，回传 thinking 会让下一轮 400。
		if text := messageText(msg["content"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				if tc == nil {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				name, args := "", any(nil)
				if fn != nil {
					name = asString(fn["name"])
					args = fn["arguments"]
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    asString(tc["id"]),
					"name":  name,
					"input": parseToolInput(args),
				})
			}
		}
	}
	return map[string]any{
		"id":            msgID(id),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStop(fr),
		"stop_sequence": nil,
		"usage":         anthropicUsage(obj["usage"]),
	}
}

func messageText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return asContentString(v)
}

func parseToolInput(v any) any {
	switch t := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return t
	case string:
		if t == "" {
			return map[string]any{}
		}
		var parsed any
		if json.Unmarshal([]byte(t), &parsed) != nil || parsed == nil {
			return map[string]any{"raw": t}
		}
		if _, ok := parsed.(map[string]any); ok {
			return parsed
		}
		return map[string]any{"value": parsed}
	default:
		return map[string]any{"value": t}
	}
}

func anthropicStop(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func anthropicUsage(v any) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	u, _ := v.(map[string]any)
	if u == nil {
		return out
	}
	if n, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = n
	}
	if n, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = n
	}
	// ponytail: 上游的 cache_read_/cache_creation_input_tokens 恒 0，真实命中数在
	// prompt_cache_hit_tokens（prompt_tokens_details.cached_tokens 同值）。不映射
	// 的话 Claude 系客户端一律显示缓存读取 0。
	if n, ok := u["prompt_cache_hit_tokens"]; ok {
		out["cache_read_input_tokens"] = n
	}
	if n, ok := u["prompt_cache_write_tokens"]; ok {
		out["cache_creation_input_tokens"] = n
	}
	return out
}

func msgID(id string) string {
	if id == "" {
		id = "wb2api"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func anthropicErrorBody(status int, obj map[string]any) []byte {
	msg := "upstream error"
	if e, ok := obj["error"].(map[string]any); ok {
		if m := asString(e["message"]); m != "" {
			msg = m
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrType(status), "message": msg},
	})
	return raw
}

type messagesWriter struct {
	http.ResponseWriter
	stream  bool
	status  int
	hdrSent bool
	rest    []byte
	body    []byte
	x       anthState
}

type anthState struct {
	started, failed, done bool
	id, model, stop       string
	textOpen              bool
	textIdx, next         int
	tools                 map[int]*anthTool
	toolOrder             []int
	usage                 map[string]any
}

type anthTool struct {
	idx            int
	block          int
	id, name       string
	opened, closed bool
}

func (w *messagesWriter) Header() http.Header { return w.ResponseWriter.Header() }

func (w *messagesWriter) WriteHeader(code int) { w.status = code }

func (w *messagesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *messagesWriter) Write(p []byte) (int, error) {
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

func (w *messagesWriter) finish() {
	if w.stream {
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") || w.hdrSent {
			if w.x.started && !w.x.done && !w.x.failed {
				_ = w.closeAndStop()
			}
			return
		}
		_ = w.emitJSONError()
		return
	}
	w.flushJSON()
}

func (w *messagesWriter) flushJSON() {
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
	if _, ok := obj["error"]; ok {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(anthropicErrorBody(status, obj))
		return
	}
	raw, err := json.Marshal(chatCompletionToMessage(obj))
	if err != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(raw)
}

func (w *messagesWriter) emitJSONError() error {
	var obj map[string]any
	_ = json.Unmarshal(w.body, &obj)
	msg := "upstream error"
	if e, ok := obj["error"].(map[string]any); ok {
		if m := asString(e["message"]); m != "" {
			msg = m
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return w.emitError(msg, anthropicErrType(status))
}

func (w *messagesWriter) handleFrame(frame string) error {
	if w.x.failed || w.x.done {
		return nil
	}
	if strings.HasPrefix(frame, "data: [DONE]") {
		return w.closeAndStop()
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
		msg := "upstream error"
		if e, ok := errObj.(map[string]any); ok {
			if m := asString(e["message"]); m != "" {
				msg = m
			}
		}
		return w.emitError(msg, "api_error")
	}
	return w.handleChunk(obj)
}

func (w *messagesWriter) handleChunk(obj map[string]any) error {
	if err := w.ensureStart(obj); err != nil {
		return err
	}
	if u, ok := obj["usage"].(map[string]any); ok {
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
	if d, _ := c["delta"].(map[string]any); d != nil {
		if s, ok := d["content"].(string); ok && s != "" {
			if err := w.ensureText(); err != nil {
				return err
			}
			if err := w.emit("content_block_delta", map[string]any{
				"index": w.x.textIdx,
				"delta": map[string]any{"type": "text_delta", "text": s},
			}); err != nil {
				return err
			}
		}
		if tcs, ok := d["tool_calls"].([]any); ok {
			if err := w.onTools(tcs); err != nil {
				return err
			}
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.stop = fr
	}
	return nil
}

func (w *messagesWriter) ensureStart(obj map[string]any) error {
	if w.x.started {
		return nil
	}
	w.x.started = true
	if obj != nil {
		w.x.id, _ = obj["id"].(string)
		w.x.model, _ = obj["model"].(string)
	}
	// ponytail: usage 在最后一帧才到，message_start 的 input_tokens 固定 0；真实数在 message_delta。
	return w.emit("message_start", map[string]any{
		"message": map[string]any{
			"id":            msgID(w.x.id),
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         w.x.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (w *messagesWriter) ensureText() error {
	if w.x.textOpen {
		return nil
	}
	w.x.textIdx = w.x.next
	w.x.next++
	w.x.textOpen = true
	return w.emit("content_block_start", map[string]any{
		"index":         w.x.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (w *messagesWriter) onTools(tcs []any) error {
	if w.x.tools == nil {
		w.x.tools = map[int]*anthTool{}
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
			acc = &anthTool{idx: idx}
			w.x.tools[idx] = acc
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if id := asString(tc["id"]); id != "" {
			acc.id = id
		}
		fn, _ := tc["function"].(map[string]any)
		args := ""
		if fn != nil {
			if n := asString(fn["name"]); n != "" {
				acc.name = n
			}
			args = asString(fn["arguments"])
		}
		if !acc.opened && (acc.id != "" || acc.name != "" || args != "") {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
		if args != "" && acc.opened {
			if err := w.emit("content_block_delta", map[string]any{
				"index": acc.block,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *messagesWriter) openTool(acc *anthTool) error {
	if acc.opened {
		return nil
	}
	if w.x.textOpen {
		if err := w.emit("content_block_stop", map[string]any{"index": w.x.textIdx}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	acc.opened = true
	acc.block = w.x.next
	w.x.next++
	if acc.id == "" {
		acc.id = "toolu_" + asString(acc.idx)
	}
	return w.emit("content_block_start", map[string]any{
		"index": acc.block,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    acc.id,
			"name":  acc.name,
			"input": map[string]any{},
		},
	})
}

func (w *messagesWriter) closeAndStop() error {
	if w.x.done || w.x.failed {
		return nil
	}
	w.x.done = true
	if !w.x.started {
		if err := w.ensureStart(nil); err != nil {
			return err
		}
	}
	if w.x.textOpen {
		if err := w.emit("content_block_stop", map[string]any{"index": w.x.textIdx}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	for _, idx := range w.x.toolOrder {
		acc := w.x.tools[idx]
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
		if !acc.closed {
			if err := w.emit("content_block_stop", map[string]any{"index": acc.block}); err != nil {
				return err
			}
			acc.closed = true
		}
	}
	if err := w.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": anthropicStop(w.x.stop), "stop_sequence": nil},
		"usage": anthropicUsage(w.x.usage),
	}); err != nil {
		return err
	}
	return w.emit("message_stop", map[string]any{})
}

func (w *messagesWriter) emitError(msg, typ string) error {
	if w.x.failed || w.x.done {
		return nil
	}
	w.x.failed = true
	w.x.done = true
	return w.emit("error", map[string]any{
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func (w *messagesWriter) emit(event string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = event
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// ponytail: 不显式 WriteHeader——下面的 io.WriteString 会隐式提交 200
		//（Flush 先提交时显式调用会变成 superfluous 警告，同 responses.go emit）。
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, "event: "+event+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	w.Flush()
	return nil
}
