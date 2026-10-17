// search.go 联网搜索接口：/agenttool/v1/search。
//
// 上游契约（对齐官方客户端 WebSearchTool，实测 2026-10）：
//   - POST {chatBase}/agenttool/v1/search，超时 20s
//   - 请求体 {"query":..., "type":"text2text", "max_results":N}
//   - 响应为**扁平结构**（非 {code,msg,data} 信封）：
//     {query, type, provider, results[{title,url,snippet,site?,favicon?}],
//     total_results, response_time_ms}
//   - 业务错误才带 code/msg（如 code=15003 unsupported search type/provider），
//     且通常同时伴随 HTTP 4xx
//
// 为什么不能复用 doJSON：doJSON 解的是 {code,msg,data} 统一信封并只返回 data；
// 搜索成功响应没有 data 包裹，直接套信封会拿到空 data。故单独实现 doSearch，
// 但错误分类与 doJSON 保持同一口径（复用 Classify/Error），不引入第二套判定。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// searchPath 搜索端点路径。官方 CLI 同名常量 WEB_SEARCH_API_PATH。
	// 与 chat 共用同一 endpoint（官方 product.endpoint 同时承载 /v2/chat/completions
	// 与 /agenttool/v1/search），故 base 复用 chatBase。
	searchPath = "/agenttool/v1/search"

	// searchTimeout 搜索请求整体超时，对齐官方 WEB_SEARCH_TIMEOUT_MS（2e4）。
	// 必须显式设置：搜索是同步 JSON 请求（非流式），上游 hang 时若无超时会一直占用
	// 在途租约，把账号池名额耗光。
	searchTimeout = 20 * time.Second

	// SearchDefaultMaxResults 默认返回条数 = 官方硬编码值（buildRequestBody 固定 5）。
	SearchDefaultMaxResults = 5

	// SearchMaxResultsCap 服务端实际返回上限（实测约 10）。
	// 请求值超过该上限不会报错、也不会多返回，只是被服务端截断——故在入口收敛，
	// 避免把无效配置透传给上游并给调用方「配了就该生效」的错觉。
	SearchMaxResultsCap = 10

	// SearchMinQueryLen 官方 zod schema 的 query.min(2)。
	SearchMinQueryLen = 2
)

// SearchResult 单条搜索结果。
// site / favicon 是上游可选字段（官方客户端同样按「有才带」处理）。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	Site    string `json:"site,omitempty"`
	Favicon string `json:"favicon,omitempty"`
}

// SearchOutcome 一次搜索的完整结果，字段名与上游响应一致，便于直接透出。
type SearchOutcome struct {
	Query          string         `json:"query"`
	Results        []SearchResult `json:"results"`
	TotalResults   int            `json:"total_results"`
	Provider       string         `json:"provider,omitempty"`
	ResponseTimeMs int64          `json:"response_time_ms,omitempty"`
}

// SearchHeaders 搜索请求头。
//
// 复用 ChatHeaders 的桌面端全套头族（UA 三段式 / Origin / Referer /
// X-CodeBuddy-Request / X-Machine-ID / X-Session-ID / Accept-Language /
// Authorization / X-User-Id / 企业与域头 / X-Agent-Purpose + X-IDE-* + X-Product /
// X-Device-Token / 会话头族 X-Conversation-* + X-Request-ID + X-Root-Request-ID + B3），
// 再按官方 WebSearchTool 的差异覆盖四处：
//   - Accept：搜索是非流式 JSON，官方为 application/json
//     （ChatHeaders 为 chat 流式设成 event-stream，必须覆盖）
//   - Content-Type：官方搜索为 application/json;charset=UTF-8
//     （CommonHeaders 的 chat 形态是不带 charset 的 application/json）
//   - X-Agent-Intent：官方固定 "craft"（搜索请求不携带会话 mode）
//   - X-Agent-Runtime：官方 2.166.0 起必发，无 runtime 时回落 "other"
//
// 为什么整体复用 ChatHeaders 而不是照抄官方 WebSearchTool 的精简头清单：
// 官方 CLI 的搜索头就是它**同一个客户端**的通用头（CLI 的 chat 与搜索共用
// `CLI/<ver>` UA、同一套会话头族），搜索并没有独立的头族。本项目出站身份是
// WorkBuddy 桌面端，chat 一直用 ChatHeaders 那套；若搜索另发一套精简头，上游会看到
// 「同一账号同一时段存在两种不同客户端指纹」，反而更像异常。故搜索 = 桌面端头族
// + 搜索特有的三项覆盖。
func (c *Client) SearchHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.ChatHeaders(req, a, clientIP, meta)
	req.Header.Set("Accept", "application/json")
	// 官方搜索 Content-Type 带 charset（MediaType.APPLICATION_JSON_UTF8）。
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "other")
}

// ClampSearchMaxResults 把调用方给的条数收敛到 [1, SearchMaxResultsCap]；
// <=0 视为未指定，回落官方默认值。
func ClampSearchMaxResults(n int) int {
	if n <= 0 {
		return SearchDefaultMaxResults
	}
	if n > SearchMaxResultsCap {
		return SearchMaxResultsCap
	}
	return n
}

// searchRequestBody 出站搜索请求体。
//
// 用 struct 而不是 map：Go 的 map 序列化会按键名排序（max_results, query, type），
// 而官方 buildRequestBody 的字段顺序是 query → type → max_results。虽然 JSON 语义上
// 顺序无关，但保持与真实客户端逐字节同形，便于抓包比对与可能的 body 指纹风控。
type searchRequestBody struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	MaxResults int    `json:"max_results"`
}

// Search 对指定账号执行一次联网搜索。
//
// 只做「一次上游调用 + 结果解析」，不涉及选号/轮转/冷却——那些属于 handler 的职责
// （与 chat 一致：upstream 层不感知账号池）。
//
// 返回的 error 在 HTTP 非 2xx 或业务 code != 0 时是 *Error（带 Kind，供 handler
// 决定是否罚号），其余（序列化/读体失败）是普通 error（传输层故障不罚号）。
func (c *Client) Search(ctx context.Context, a *auth.Auth, query string, maxResults int, meta ChatMeta) (*SearchOutcome, error) {
	query = strings.TrimSpace(query)
	if len(query) < SearchMinQueryLen {
		return nil, &Error{Kind: ErrClient, Msg: fmt.Sprintf("search query must be at least %d characters", SearchMinQueryLen)}
	}
	body, err := json.Marshal(searchRequestBody{
		Query:      query,
		Type:       "text2text", // 官方硬编码；服务端只接受小写该值
		MaxResults: ClampSearchMaxResults(maxResults),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+searchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.SearchHeaders(req, a, "", meta)
	return c.doSearch(req)
}

// doSearch 发搜索请求并解析扁平响应。
//
// 错误处理与 doJSON 同口径：
//   - HTTP >= 400 → Classify 出 Kind，包成 *Error
//   - 2xx 但 code != 0 → 业务错误（ErrNone 时兜底 ErrClient），同样包成 *Error
//   - 读体失败 → 普通 error（半截 body 不参与账号惩罚）
func (c *Client) doSearch(req *http.Request) (*SearchOutcome, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	// 成功响应没有 data 包裹：code/msg 与结果字段同级，用嵌入结构一次性解出。
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		SearchOutcome
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if parsed.Code != 0 {
		kind := Classify(resp.StatusCode, parsed.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", parsed.Code, truncate(parsed.Msg, 160))}
	}
	out := parsed.SearchOutcome
	// results 恒输出为数组（nil 会让 JSON 变成 null，调用方还要判空）。
	if out.Results == nil {
		out.Results = []SearchResult{}
	}
	// 上游个别情况下 total_results 缺省，用实际条数兜底，避免对外出现 0 条却有条目的矛盾。
	if out.TotalResults == 0 && len(out.Results) > 0 {
		out.TotalResults = len(out.Results)
	}
	return &out, nil
}
