// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start            time.Time
	model            string
	mode             string // "stream" | "sync"
	uid              string // 完整 uid，展示时只取前 8 位
	nick             string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb             time.Duration
	toks             int // <0 表示 usage 缺失 → 显示 "-"
	status           int
	requestID        string
	outcome          string
	attempts         int
	credit           float64
	hasCredit        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// 缓存命中观测（usage.prompt_cache_hit_tokens / miss）：上游给到才有效。
	// 供 usage 桶命中率维度与 reqlog 逐次记录；缺失时 hasCache=false 不参与统计。
	cacheHit  int64
	cacheMiss int64
	hasCache  bool

	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.clientIP, s.userAgent)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit  bool
	credit     float64
	errorFrame bool
	// cacheHit/cacheMiss 上游末帧 usage.prompt_cache_hit_tokens / miss_tokens，
	// 供用量桶的命中率维度与 reqlog 逐次记录（issue #92）。
	hasCacheHit  bool
	cacheHit     int
	cacheMiss    int
	hasCacheMiss bool
	pend         []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		Usage *struct {
			PromptTokens         *int            `json:"prompt_tokens"`
			CompletionTokens     *int            `json:"completion_tokens"`
			TotalTokens          json.RawMessage `json:"total_tokens"`
			Credit               json.RawMessage `json:"credit"`
			PromptCacheHitTokens *int            `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTok   *int            `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Error) > 0 {
			s.errorFrame = true
		}
		return
	}
	if len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if len(chunk.Usage.TotalTokens) > 0 {
		var total float64
		if json.Unmarshal(chunk.Usage.TotalTokens, &total) == nil {
			parsedTotal, valid := parseUsageTotalTokens(total)
			if !valid {
				s.hasTotalTokens = false
				s.totalTokens = 0
			} else {
				s.hasTotalTokens = true
				s.totalTokens = parsedTotal
			}
		} else {
			s.hasTotalTokens = false
			s.totalTokens = 0
		}
	}
	if len(chunk.Usage.Credit) > 0 {
		var credit float64
		if json.Unmarshal(chunk.Usage.Credit, &credit) == nil && validUsageCredit(credit, true) {
			s.hasCredit = true
			s.credit = credit
		} else {
			// An invalid later usage frame supersedes an earlier value; do not
			// archive the previous frame's fee as if it belonged to this response.
			s.hasCredit = false
			s.credit = 0
		}
	}
	if chunk.Usage.PromptCacheHitTokens != nil {
		s.hasCacheHit = true
		s.cacheHit = *chunk.Usage.PromptCacheHitTokens
	}
	if chunk.Usage.PromptCacheMissTok != nil {
		s.hasCacheMiss = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTok
	}
}

// CacheTokens 返回末帧 usage 的缓存命中 / 未命中 token 数。miss 缺失时按
// prompt - hit 推导；hit 与 miss 均不可得时 ok=false（不参与命中率统计）。
func (s *chatStatsReader) CacheTokens() (hit, miss int64, ok bool) {
	if !s.hasCacheHit {
		return 0, 0, false
	}
	hit = int64(s.cacheHit)
	miss = int64(s.cacheMiss)
	if !s.hasCacheMiss {
		if !s.hasPromptTokens || s.promptTokens < s.cacheHit {
			return hit, 0, true
		}
		miss = int64(s.promptTokens - s.cacheHit)
	}
	return hit, miss, true
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 实现委托 logfmt.UID8，避免 "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	start time.Time
	stat  *chatStat
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与 upstream.ExtractClientIP 的差别：后者只认代理头（X-Forwarded-For 首段 →
// X-Real-IP），因为它的用途是把客户端 IP **透传给上游**，回落到网关自身地址会
// 污染上游风控判据；日志场景相反——直连（无反代）时 RemoteAddr 就是唯一线索，
// 必须回落，否则面板里所有来源都显示 "-"。代理头优先保证反代后拿到真实客户端。
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      "/v1/chat/completions",
		Status:    status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		e.CacheHitTokens = s.cacheHit
		e.CacheMissTokens = s.cacheMiss
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	if e.Outcome == "" {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	return e
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（输出 chatLogOut，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	logChatRowEx(ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "")
}

// logChatRowEx 是带请求 ID、结果、重试、积分与调用来源字段的扩展流水行。旧调用保持
// 原格式；requestID 非空时才追加扩展字段；来源两参均为空时不追加来源段。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = fmt.Sprintf("%.4f", credit)
		}
		extra = fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
	}
	// 调用来源：IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后的客户端标签
	// 并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只会取到 OpenAI/Python）。
	// 两者都未采集时不追加，旧行格式保持不变。
	src := ""
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		src = fmt.Sprintf(" src=%s ua=%s |", dashIfEmpty(clientIP), ua)
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
		extra,
		src,
	)
}

// cacheMissSignal 低命中率告警信号（issue #92 P1 轻量化）：同一模型此前观测到过
// 命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	everHit map[string]bool
	last    map[string]time.Time
}

var cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

// cacheMissWarnEvery 冷却间隔；变量供测试缩短。
var cacheMissWarnEvery = 10 * time.Minute

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit > 0 {
		c.everHit[model] = true
		return
	}
	// hit==0 才可能是整段重算。小前缀（<1000 tok）的 miss 无告警价值；上游未回
	// miss 字段时按 prompt 全量视为未命中。
	if prompt < 1000 {
		return
	}
	if miss <= 0 {
		miss = prompt
	}
	if !c.everHit[model] {
		return
	}
	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < cacheMissWarnEvery {
		return
	}
	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀未命中（prompt=%d miss=%d，此前观测到过命中）；上游前缀缓存重算，费用会显著升高", model, prompt, miss)
}
