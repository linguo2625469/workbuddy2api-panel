// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Panel struct {
		// PackageDetailLimit 积分构成页单账号默认展示的最近到期包数；<=0 回落 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7；<=0 回落默认。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100；<=0 回落默认。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo 是否在请求日志（归档事件 + stdout 流水行 + 面板运行
		// 日志）里记录调用来源：客户端 IP 与 User-Agent。缺省 true。
		//
		// 为什么做成开关而不是恒开：来源信息是排查"谁在打网关"的第一手线索，
		// 但它比 token 计数敏感（IP 属个人信息），共享部署/多租户场景可能需要
		// 关掉。关闭后 Event.ClientIP/UserAgent 保持为空，归档里不出现该字段。
		// 热生效（经 livecfg 快照），无需重启。
		RequestClientInfo bool `json:"request_client_info"`
	} `json:"logging"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		GrowthHours    []int `json:"growth_hours"`    // [1] 成长任务队列（Sequential 族每日零点解锁，01:00 自动扫描执行）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子
		GrowthEnabled    bool `json:"growth_enabled"`    // 缺省 true；false = 关成长任务自动排程

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5
	} `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 内置默认（对齐官方 5.5.4 分发包）。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（官方内置 CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "passthrough" / "custom" / "append"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// PreferExpiring 最早到期优先路由开关，默认 true。开启且 expiring_soon 窗口内
		// 存在有效批次时，按最早到期时间排序；关闭后完全不使用到期信息选号。
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的积分进入优先集，再按最早到期排序。空/0 = 禁用该路由门槛。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型（tier 2）不再
		// 参与选号——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到。
		// tier 0（免费）/ tier 1（无观测）不受限；签到回血越过 floor 自动恢复。
		// 默认 0 = 关闭；负值钳 0。
		CreditFloor int64 `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 缺省 true 靠显式赋值实现（同 Schedule 开关）：JSON 里键缺席时字段保留此值，
	// 只有显式 false 才关闭来源记录。
	c.Logging.RequestClientInfo = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_PREFER_EXPIRING"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PreferExpiring = b
		}
	}
}

// EnvironmentManagedFields names fields whose startup environment overrides the file.
// Values are never returned: the panel uses these names to prevent ineffective edits.
func (c *Config) EnvironmentManagedFields() []string {
	fields := []struct{ env, path, kind string }{
		{"WB2A_LISTEN", "listen", ""}, {"WB2A_API_KEY", "api_key", ""},
		{"WB2A_AUTH_DIR", "auth_dir", ""}, {"WB2A_STATE_FILE", "state_file", ""},
		{"WB2A_SOFT_RATE", "cooldown.soft_rate", ""}, {"WB2A_SOFT_RATE_MAX", "cooldown.soft_rate_max", ""},
		{"WB2A_TIMEOUT_SECONDS", "upstream.timeout_seconds", "int"}, {"WB2A_HEADER_TIMEOUT_SECONDS", "upstream.header_timeout_seconds", "int"},
		{"WB2A_IDLE_TIMEOUT_SECONDS", "upstream.idle_timeout_seconds", "int"}, {"WB2A_USER_AGENT", "upstream.user_agent", ""},
		{"WB2A_CLIENT_VERSION", "upstream.client_version", ""}, {"WB2A_CLI_VERSION", "upstream.cli_version", ""},
		{"WB2A_CLIENT_NAME", "upstream.client_name", ""}, {"WB2A_DEVICE_TOKEN", "upstream.device_token", ""},
		{"WB2A_DEVICE_TOKEN_FILE", "upstream.device_token_file", ""}, {"WB2A_PASSTHROUGH_IP", "upstream.passthrough_ip", "bool"},
		{"WB2A_SANITIZE_FINGERPRINTS", "features.sanitize_blacklist_fingerprints", "bool"},
		{"WB2A_PROMPT_MODE", "prompt.mode", ""}, {"WB2A_PROMPT_FILE", "prompt.file", ""},
		{"WB2A_EXPIRING_SOON", "pool.expiring_soon", ""}, {"WB2A_PREFER_EXPIRING", "pool.prefer_expiring", "bool"},
	}
	var out []string
	for _, field := range fields {
		value := os.Getenv(field.env)
		if value == "" {
			continue
		}
		if field.kind == "int" {
			if _, err := strconv.Atoi(value); err != nil {
				continue
			}
		}
		if field.kind == "bool" {
			if _, err := strconv.ParseBool(value); err != nil {
				continue
			}
		}
		out = append(out, field.path)
	}
	return out
}

func (c *Config) normalize() error {
	var err error
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	//（"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	// 积分保底：负值钳 0（= 关闭）。0 是合法默认（关闭），无需空值回落。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 passthrough/custom/append）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
