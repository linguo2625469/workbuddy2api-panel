// proxyusage.go 代理池路由（zcode / trae / autoclaw）的用量记账。
//
// 背景：主链路 chatCompletions 自带逐次记账（recordAttempt → usage.Recorder），
// 而三条代理路径直接把请求交给上游 http.Handler 透传，完全绕过用量记录器——
// glm-5.3 这类落 zcode/autoclaw 的请求在面板「用量」页永远不出现。
//
// 方案：不干预透传，在 ResponseWriter 外包一层尾缓冲（usageTailWriter），请求
// 结束后从响应末尾 64KB 提取上游 usage 对象（SSE 末帧与非流式 JSON 两种形态
// 通吃——OpenAI 格式里 usage 恒在响应尾部），再补记一条到用量时序。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// usageTailMax 尾缓冲字节上限。usage 对象恒在响应尾部（SSE 末帧 / JSON 尾部），
// 64KB 足以覆盖任何形态的 usage 帧，同时把内存上界钉死。
const usageTailMax = 64 << 10

// usageTailWriter 包装 ResponseWriter：原始字节原样透传给客户端的同时，保留
// 末尾 usageTailMax 字节的副本，供请求结束后提取上游 usage。
type usageTailWriter struct {
	http.ResponseWriter
	tail   []byte
	total  int64
	status int
}

func (w *usageTailWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *usageTailWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.total += int64(len(p))
	w.tail = append(w.tail, p...)
	if overflow := len(w.tail) - usageTailMax; overflow > 0 {
		w.tail = append(w.tail[:0], w.tail[overflow:]...)
	}
	return w.ResponseWriter.Write(p)
}

// Flush 透传 Flusher：ReverseProxy 逐帧刷 SSE 依赖它，包装层不能吞掉。
func (w *usageTailWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 逐层解包（ReverseProxy 错误处理路径会用到）。
func (w *usageTailWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// proxiedUsage 上游 usage 对象中网关关心的字段。指针语义区分「缺字段」与「真实 0」。
type proxiedUsage struct {
	PromptTokens     *int64   `json:"prompt_tokens"`
	CompletionTokens *int64   `json:"completion_tokens"`
	TotalTokens      *int64   `json:"total_tokens"`
	Credit           *float64 `json:"credit"`
}

// extractUsageTail 从响应尾缓冲提取最后一个可解析的 usage 对象。
// 从后往前扫 `"usage"` 键并做花括号配对（带字符串字面量转义感知），取第一个
// 能解出至少一个已知字段的样本——SSE 流的 usage 在末帧，非流式 JSON 的 usage
// 在 choices 之后，两者都天然落在尾缓冲里。
func extractUsageTail(tail []byte) (proxiedUsage, bool) {
	key := []byte(`"usage"`)
	for pos := bytes.LastIndex(tail, key); pos >= 0; pos = bytes.LastIndex(tail[:pos], key) {
		obj, _, found := matchJSONObject(tail, pos+len(key))
		if !found {
			continue
		}
		var cand proxiedUsage
		if json.Unmarshal(obj, &cand) == nil &&
			(cand.PromptTokens != nil || cand.CompletionTokens != nil || cand.TotalTokens != nil || cand.Credit != nil) {
			return cand, true
		}
	}
	return proxiedUsage{}, false
}

// matchJSONObject 从 off 起跳过空白与冒号，提取下一个完整 JSON 对象（含花括号）。
// 返回对象字节切片与结束下标；形态不符或括号未闭合时 ok=false。
func matchJSONObject(buf []byte, off int) (obj []byte, end int, ok bool) {
	i := off
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t' || buf[i] == '\r' || buf[i] == '\n') {
		i++
	}
	if i >= len(buf) || buf[i] != ':' {
		return nil, i, false
	}
	i++
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t' || buf[i] == '\r' || buf[i] == '\n') {
		i++
	}
	if i >= len(buf) || buf[i] != '{' {
		return nil, i, false
	}
	depth, inStr, esc := 0, false, false
	for j := i; j < len(buf); j++ {
		c := buf[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return buf[i : j+1], j + 1, true
			}
		}
	}
	return nil, len(buf), false
}

func int64Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// serveProxiedChat 代理池路由的统一出口：透传上游响应，结束后补记一条用量。
//
// realm 用代理池名（zcode/trae/autoclaw），uid 同样取池名——上游内部选号对网关
// 不可见，账号维度只能按池聚合；模型取分流后的裸名。成败口径按 HTTP 状态判定
// （>=400 计失败），不能按「有没有 usage」判定——部分上游（如 autoclaw2api 非
// 流式）合法地不返回 usage，那不是失败。
func (h *Handler) serveProxiedChat(w http.ResponseWriter, r *http.Request, realm, model string, upstream http.Handler) {
	if h.cfg.Usage == nil || upstream == nil {
		upstream.ServeHTTP(w, r)
		return
	}
	started := time.Now()
	tw := &usageTailWriter{ResponseWriter: w}
	upstream.ServeHTTP(tw, r)

	var u proxiedUsage
	hasUsage := false
	ok := tw.status >= 200 && tw.status < 400
	if ok {
		u, hasUsage = extractUsageTail(tw.tail)
	}
	latencyMs := time.Since(started).Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	var tps float64
	hasTPS := hasUsage && u.CompletionTokens != nil
	if hasTPS {
		tps = float64(*u.CompletionTokens) * 1000 / float64(latencyMs)
	}
	credit := 0.0
	if u.Credit != nil {
		credit = *u.Credit
	}
	if model == "" {
		model = "(unknown)"
	}
	h.cfg.Usage.Add(time.Now(), realm, realm, model, usage.Delta{
		PromptTokens:     int64Val(u.PromptTokens),
		HasPromptTokens:  u.PromptTokens != nil,
		CompletionTokens: int64Val(u.CompletionTokens),
		HasCompletion:    u.CompletionTokens != nil,
		TotalTokens:      int64Val(u.TotalTokens),
		HasTotal:         u.TotalTokens != nil,
		Credit:           credit,
		HasCredit:        u.Credit != nil,
		LatencyMs:        latencyMs,
		HasLatency:       true,
		TokensPerSecond:  tps,
		HasTPS:           hasTPS,
	}, ok)
}
