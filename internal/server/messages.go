// messages.go Anthropic Messages 兼容的搜索端点 POST /v1/search/messages。
//
// 存在理由：DSH 内置的搜索 provider（`@deepseek-ai/dsh-web-search-deepseek`）把
// 搜索请求硬编码发到 `${baseURL}/messages`，且要求响应是 Anthropic Messages 形态
// （含 `web_search_tool_result` block）。把该协议壳做在网关侧，客户端只需把搜索
// baseURL 指向 `<网关>/v1/search`，零代码改动即可用上本网关的搜索能力。
//
// 为什么不占用 `/v1/messages`：那是 Anthropic **模型对话**的标准路径，将来网关若要
// 支持 Anthropic 协议的 chat 转发会与搜索冲突。搜索挂在 `/v1/search/messages`，
// 与 OpenAI 风格的 `/v1/search` 同前缀，语义清晰且互不干扰。
//
// 与真实 Anthropic 行为的差异（有意为之）：本端点**不做模型调用**。真实 Anthropic
// 的 web_search 是 server tool，由模型决定何时搜、并生成带引用的自然语言回答；
// 这里直接执行一次搜索并返回结构化结果块——因为网关没有模型，客户端（DSH）只从
// 结果块里取 sources，不消费自然语言回答。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// messagesBodyLimit Messages 请求体上限：只含 query 与工具声明，1MB 富余。
const messagesBodyLimit = 1 << 20

// messagesQueryPrefix 辅助搜索模型收到的固定指令前缀（DSH/官方 CLI 的约定）。
// 它是给模型看的指令，不是搜索词的一部分，必须剥掉后再送去搜索。
const messagesQueryPrefix = "Perform a web search for the query:"

// messagesRequest Anthropic Messages 请求里本端点关心的字段。
type messagesRequest struct {
	Model    string             `json:"model"`
	Tools    []messagesTool     `json:"tools"`
	Messages []messagesTurnItem `json:"messages"`
}

// messagesTool 工具声明。搜索工具两种形态都认：`{type:"web_search_20250305"}`
// 与 `{name:"web_search"}`（后者是部分客户端的简化形态）。
type messagesTool struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// messagesTurnItem 单条消息。content 既可能是字符串，也可能是 block 数组，
// 用 RawMessage 保留原始形态交给 extractMessagesQuery 处理。
type messagesTurnItem struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messagesContentBlock Messages content 数组里的文本块。
type messagesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// declaresWebSearch 判断请求是否声明了 web_search server tool。
// 判定口径与 DSH 内置 provider 的客户端实现一致：type 以 "web_search" 开头，
// 或 name 恰为 "web_search"。
func declaresWebSearch(tools []messagesTool) bool {
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") || t.Name == "web_search" {
			return true
		}
	}
	return false
}

// extractMessagesQuery 从 Messages 请求里还原真实搜索词。
//
// 约定（DSH / 官方 CLI 相同）：请求里恰好一条 user 消息，文本为
// `Perform a web search for the query: <query>`。前缀必须剥掉，否则会把指令本身
// 当成搜索词送去上游。
//
// **只取最后一条带文本的消息**，不拼接整段历史。拼接会在客户端带多轮上下文时把
// 历史问句一起当搜索词送上游（且前缀因不在开头而剥不掉，最终搜的是一段垃圾）；
// 「最后一次提问」才是此处唯一正确的语义。
func extractMessagesQuery(items []messagesTurnItem) string {
	joined := ""
	for _, m := range items {
		if len(m.Content) == 0 {
			continue
		}
		// content 为纯字符串（Anthropic 允许的简写形态）。
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			if t := strings.TrimSpace(plain); t != "" {
				joined = t
			}
			continue
		}
		var blocks []messagesContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		var text []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		}
		// 同一条消息内的多个 text block 拼接（一条消息被拆块是合法的）。
		if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
			joined = t
		}
	}
	// 前缀大小写不敏感地剥离：不同客户端的模板大小写可能有差异。
	if len(joined) >= len(messagesQueryPrefix) &&
		strings.EqualFold(joined[:len(messagesQueryPrefix)], messagesQueryPrefix) {
		joined = strings.TrimSpace(joined[len(messagesQueryPrefix):])
	}
	return joined
}

// messagesSearchResultItem 响应里 `web_search_tool_result.content[]` 的单项。
// 注意：Anthropic 原生不带 snippet，摘要走 text 块的 citations，故这里只有 url/title。
type messagesSearchResultItem struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// messagesToolResultBlock 搜索结果块。
type messagesToolResultBlock struct {
	Type      string                     `json:"type"`
	ToolUseID string                     `json:"tool_use_id"`
	Content   []messagesSearchResultItem `json:"content"`
}

// messagesCitation 引用条目（snippet 的唯一承载处）。
type messagesCitation struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	CitedText string `json:"cited_text"`
}

// messagesTextBlock 承载 citations 的文本块。
type messagesTextBlock struct {
	Type      string            `json:"type"`
	Text      string            `json:"text"`
	Citations []messagesCitation `json:"citations,omitempty"`
}

// messagesUsage Anthropic 响应必带的 usage。搜索不消耗模型 token，故为 0。
type messagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// messagesResponse Anthropic Messages 响应。
type messagesResponse struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	Content      []any  `json:"content"`
	StopReason   string `json:"stop_reason"`
	StopSequence any    `json:"stop_sequence"`
	Usage        messagesUsage `json:"usage"`
}

// buildMessagesSearchResponse 把上游搜索结果渲染成 Anthropic Messages 响应。
//
// 结构契约（DSH 内置 provider 的 mapAnthropicResponse 逐条要求）：
//   - 必须至少有一个 type="web_search_tool_result" 的 block，否则客户端报
//     WEB_PROVIDER_ERROR；
//   - 该 block 的 content[] 每项 type="web_search_result"，url 非空；
//   - snippet 只能从 text block 的 citations[].cited_text 取（web_search_result
//     本身不带摘要），故有摘要时追加一个 text block 承载引用。
func buildMessagesSearchResponse(query, model string, out *upstream.SearchOutcome) messagesResponse {
	items := make([]messagesSearchResultItem, 0, len(out.Results))
	citations := make([]messagesCitation, 0, len(out.Results))
	for _, r := range out.Results {
		title := r.Title
		if title == "" {
			title = r.URL // 无标题时用 URL 兜底，客户端渲染 title ?? hostname 才不会空
		}
		items = append(items, messagesSearchResultItem{
			Type:  "web_search_result",
			URL:   r.URL,
			Title: title,
		})
		if r.Snippet != "" {
			citations = append(citations, messagesCitation{
				Type:      "web_search_result_location",
				URL:       r.URL,
				CitedText: r.Snippet,
			})
		}
	}
	if model == "" {
		model = "web-search"
	}
	// tool_use_id 与 msg id 每次调用都必须唯一：Anthropic 协议里它们标识的是
	// 「一次工具调用」，不是「一次查询的内容」。用 query 哈希会让同一 query 的
	// 重复搜索返回同一个 id（下游按 id 缓存/去重会互相覆盖），32 位哈希本身也可
	// 能碰撞。随机 16 hex 兼顾唯一性与可读性。
	callID := randomHex(16)
	content := []any{messagesToolResultBlock{
		Type:      "web_search_tool_result",
		ToolUseID: "srvtoolu_" + callID,
		Content:   items,
	}}
	if len(citations) > 0 {
		content = append(content, messagesTextBlock{Type: "text", Text: "", Citations: citations})
	}
	return messagesResponse{
		ID:           "msg_wb2api_" + callID,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      content,
		StopReason:   "end_turn",
		StopSequence: nil,
		Usage:        messagesUsage{InputTokens: 0, OutputTokens: 0},
	}
}

// randomHex 返回 n 字节随机数的 hex 串。熵源失败时回落时间+计数派生值：
// 仍保证非空且每次调用都不同（id 唯一性是这里的硬要求）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, uint64(time.Now().UnixNano())^atomic.AddUint64(&idSeq, 1))
	}
	return hex.EncodeToString(b)
}

// idSeq 熵源故障时的兜底计数器（正常路径不使用）。
var idSeq uint64

// writeMessagesError 按 Anthropic 错误形态写出（与 OpenAI 形态分开，客户端各自解析）。
func writeMessagesError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// messages 处理 POST /v1/search/messages（Anthropic Messages 兼容搜索端点）。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, messagesBodyLimit))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	// 只服务搜索：没有声明 web_search 的 Messages 请求属于模型对话，不该打到本端点
	// （打错端点时给明确错误，而不是静默把用户消息当搜索词）。
	if !declaresWebSearch(req.Tools) {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error",
			"request does not declare the web_search tool; this endpoint only serves web search, not chat")
		return
	}
	query := extractMessagesQuery(req.Messages)
	if len(query) < upstream.SearchMinQueryLen {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("could not read a search query from the request (need at least %d characters)", upstream.SearchMinQueryLen))
		return
	}

	out, fail := h.runSearch(r.Context(), query, 0, searchMetaFromRequest(r))
	if fail != nil {
		// Anthropic 错误类型映射：限流用 rate_limit_error，其余用 api_error，
		// 与真实 Anthropic 的错误词表保持可识别。
		errType := "api_error"
		if fail.status == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else if fail.status == http.StatusBadRequest {
			errType = "invalid_request_error"
		}
		msg := fail.msg
		if fail.hint != "" {
			msg = msg + " (" + fail.hint + ")"
		}
		writeMessagesError(w, fail.status, errType, msg)
		return
	}
	writeJSON(w, http.StatusOK, buildMessagesSearchResponse(query, req.Model, out))
}
