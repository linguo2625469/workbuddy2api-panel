// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// accountFaultMarkers 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控，
//     需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（下方 softRateMarkers
// 后判定）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案）。
// 14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultMarkers = []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// contentBlockedClientMsg 内容拦截返回给调用方的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴力 等），绝不填业务 code、账号、冷却、upstream 前缀。
const contentBlockedClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const contentBlockedFallbackKeyword = "违禁词"

// contentBlockedKeywords 审核分类词，按优先级扫描上游文案（大小写不敏感）。
// 只收录可直接展示给调用方的分类标签，不收录错误码（如 11128）。
var contentBlockedKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// ContentBlockedClientMessage 把上游内容拦截改写成网关防火墙口径，不含账号/错误码。
func ContentBlockedClientMessage(body string) string {
	return fmt.Sprintf(contentBlockedClientMsg, contentBlockedKeyword(body))
}

// contentBlockedKeyword 从审核文案抽出分类关键词；抽不到则回「违禁词」。
func contentBlockedKeyword(body string) string {
	text := body
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && strings.TrimSpace(env.Msg) != "" {
		text = env.Msg
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockedKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return contentBlockedFallbackKeyword
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultMarkers —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 softRate/status429 判定：14017 常带 429 状态码，若落到 status==429 兜底
//     会误归 soft_rate（"限流"语义不符：限流可指数退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     落到 softRateMarkers 层，不受影响。
//  4. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  5. status==429 —— body 无文案时的兜底识别。
//  6. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range accountFaultMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrAccountFault
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
		// 模型权限，值得再试一次）。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按 realm 分层桶（cn/global）：同模型名跨域探测的 effort 集合可能不同，
	// 混桶会互相污染（C-2）。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空）。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = 内置默认（对齐官方 5.5.4 分发包）。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认（官方内置 CLI 2.137.1）。
	CliVersion string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		WebBaseCN:            "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// 路径常量：CN 现状路径（chatCompletionsPath）与 global 双候选路径。
const (
	chatCompletionsPath   = "/v2/chat/completions"
	globalChatConsolePath = "/console/chat/completions"
)

// chatPaths 按 realm 返回 chat 端点路径候选序列：
// global → [console, v2]（404/405 时 fallback）；cn → [v2]（现状逐字，零回归）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{globalChatConsolePath, chatCompletionsPath}
	}
	return []string{chatCompletionsPath}
}

func chatFallbackHTTPStatus(status int) bool { return status == 404 || status == 405 }

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染）。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints,
		c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm))
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（两段式，缩小持锁窗口）：
//   - 锁内仅做「读 refreshToken 快照」与「校验未变后写回新 token」两小段内存操作；
//   - 网络 I/O（doJSON）在**锁外**执行，带 30s ctx 超时——避免上游 hang 时长时间
//     独占 a.mu，阻塞同账号的 SaveAtomic / 其他刷新（issue:持锁 120s I/O）。
//   - 写回前重新校验快照一致性：若锁外期间另一 goroutine 已完成刷新（refreshToken
//     已变），本次结果直接采用（新 token 已生效），不再重复写回。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （两个并发刷新拿到的新 token 都有效，后写会覆盖先写，但二者等价可用；
		// 提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 需要客户端断开联动的调用方用 ChatStreamContext 传入请求 ctx。
//
// global realm：先打 /console/chat/completions，404/405 时同一 base 二次换 /v2/chat/completions
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但出站请求挂在调用方的 reqCtx 上：
// handler 传 r.Context() → 客户端断开时上游请求随之取消（不再白白消耗账号积分
// 与上游连接继续生成无人消费的流）。成功流的 cancel 仍由 monitorBody 的 Close
// 接管（reqCtx 取消与显式 Close 任一触发即断）。
// global 首次路径 404/405 时换 fallback 路径重试；ensureConsoleSystem 在 prepareBody 后统一套用
// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	// global 首次路径 404/405 时换 fallback 路径重试。
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	for attempt, path := range c.chatPaths(a) {
		url := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
		// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
				a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
			// global 首次路径 404/405 → 换 fallback 路径重试；其余状态码直接返回。
			if attempt < len(c.chatPaths(a))-1 && chatFallbackHTTPStatus(resp.StatusCode) {
				continue
			}
			return nil, resp.StatusCode, raw, nil
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	cancel()
	return nil, 0, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID                 string
	Name               string
	ContextWindow      int64    // = maxInputTokens
	MaxTokens          int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	MaxAllowedSize     int64    // = maxAllowedSize（单请求体大小上限，通常等于 maxInputTokens）
	Efforts            []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort      string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回
	CanDisableThinking bool     // reasoning.canDisableThinking：思考可关（off 档可用）
	SupportsReasoning  bool     // supportsReasoning：模型支持思考
	SupportsImages     bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	Credits            string   // credits：积分倍率（如 "x0.79"）
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含 text-to-image：图片生成模型，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// CLI 三段式 WorkBuddy UA 会拿到精简目录（flash 输出 128K、无 supportedEfforts）；
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 才返回完整能力
// （flash：393216 + low/high/max）。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
// CLI 目录（/console/enterprises/personal/models）决定「能调哪些模型」；
// IDE /v3/config 覆盖同名模型的窗口 / 思考档（失败则静默保留 CLI 字段）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string   `json:"id"`
				Name              string   `json:"name"`
				MaxInputTokens    int64    `json:"maxInputTokens"`
				MaxOutputTokens   int64    `json:"maxOutputTokens"`
				MaxAllowedSize    int64    `json:"maxAllowedSize"`
				Disabled          bool     `json:"disabled"`
				Credits           string   `json:"credits"`
				SupportsReasoning bool     `json:"supportsReasoning"`
				SupportsImages    bool     `json:"supportsImages"`
				Tags              []string `json:"tags"`
				Reasoning         struct {
					Effort             string   `json:"effort"`        // 老模型键（auto/hy3/glm-5.2 系）
					DefaultEffort      string   `json:"defaultEffort"` // 新模型键（glm-5.3 系只返回这个）
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	type parsed struct {
		mi       ModelInfo
		disabled bool
	}
	dynMap := make(map[string]parsed, len(env.Data.Models))
	for _, m := range env.Data.Models {
		// 非对话模型（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
		// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		def := m.Reasoning.Effort
		if def == "" {
			def = m.Reasoning.DefaultEffort // 新旧双键兼容：glm-5.3 系只返回 defaultEffort
		}
		dynMap[m.ID] = parsed{ModelInfo{
			ID:                 m.ID,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			Credits:            m.Credits,
		}, m.Disabled}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if pm, ok := dynMap[id]; ok && !pm.disabled {
			out = append(out, pm.mi)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	if overlay, err := c.fetchV3ConfigModelMap(a); err == nil && len(overlay) > 0 {
		out = mergeModelCapabilities(out, overlay)
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	c.efforts[realmKey(a.Realm())] = cache
	c.defaultEfforts[realmKey(a.Realm())] = defCache
	c.effortsMu.Unlock()
	return out, nil
}

// v3ConfigDomain /v3/config 的 X-Domain：优先账号落盘 domain，否则 chatBase host。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		if d := strings.TrimSpace(a.Domain); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// fetchV3ConfigModelMap 拉官方 IDE 配置目录，按模型 id 建能力表。
// 该端点对 UA 敏感：必须带 CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403。
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", codeBuddyIDEUA)
	c.injectCodeBuddyRequest(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string `json:"id"`
				Name              string `json:"name"`
				MaxInputTokens    int64  `json:"maxInputTokens"`
				MaxOutputTokens   int64  `json:"maxOutputTokens"`
				MaxAllowedSize    int64  `json:"maxAllowedSize"`
				Credits           string `json:"credits"`
				SupportsReasoning bool   `json:"supportsReasoning"`
				SupportsImages    bool   `json:"supportsImages"`
				Reasoning         struct {
					Effort             string   `json:"effort"`
					DefaultEffort      string   `json:"defaultEffort"`
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code=%d", env.Code)
	}
	out := make(map[string]ModelInfo, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		def := m.Reasoning.DefaultEffort
		if def == "" {
			def = m.Reasoning.Effort
		}
		out[m.ID] = ModelInfo{
			ID:                 m.ID,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			Credits:            m.Credits,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// mergeModelCapabilities 用 IDE /v3/config 覆盖 CLI 目录里同 id 的窗口与思考档。
// 只填 overlay 里有值的字段，避免空配置把 CLI 已解析结果抹掉。
func mergeModelCapabilities(base []ModelInfo, overlay map[string]ModelInfo) []ModelInfo {
	if len(overlay) == 0 {
		return base
	}
	for i, mi := range base {
		ov, ok := overlay[mi.ID]
		if !ok {
			continue
		}
		if ov.ContextWindow > 0 {
			mi.ContextWindow = ov.ContextWindow
		}
		if ov.MaxTokens > 0 {
			mi.MaxTokens = ov.MaxTokens
		}
		if ov.MaxAllowedSize > 0 {
			mi.MaxAllowedSize = ov.MaxAllowedSize
		}
		if len(ov.Efforts) > 0 {
			mi.Efforts = ov.Efforts
		}
		if ov.DefaultEffort != "" {
			mi.DefaultEffort = ov.DefaultEffort
		}
		if ov.CanDisableThinking {
			mi.CanDisableThinking = true
		}
		if ov.SupportsReasoning {
			mi.SupportsReasoning = true
		}
		if ov.SupportsImages {
			mi.SupportsImages = true
		}
		if ov.Credits != "" {
			mi.Credits = ov.Credits
		}
		if ov.Name != "" {
			mi.Name = ov.Name
		}
		base[i] = mi
	}
	return base
}

// UserResource 查询账号积分余额与总额度（所有套餐聚合）。remain 负值钳 0；
// total 取与 remain 同源的额度字段（CycleCapacitySize 优先，无周期额度退
// CapacitySize），上游缺 size 的套餐按 remain 兜底，保证百分比不超 100%。
func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// packageEndLayout 上游套餐到期时间的墙钟格式（UTC+8，与 softRateResetLoc 同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 PackageEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					PackageEndTime      string `json:"PackageEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r, size int64
		switch {
		case acct.CycleCapacitySize > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		default:
			r, size = acct.CapacityRemain, acct.CapacitySize
		}
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		// 分桶：仅 soon>0 且能解析出有效到期时间、且确实在窗口内 → expiring。
		if soon > 0 && r > 0 && acct.PackageEndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, acct.PackageEndTime, softRateResetLoc); perr == nil {
				if !end.After(now.Add(soon)) {
					expiring += r
				}
			}
		}
	}
	return remain, total, expiring, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
