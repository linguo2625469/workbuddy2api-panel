package httpauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// KeySpec 一条 API Key 的配置（多 Key 场景）。
//
// 存在的意义：单密钥部署下所有客户端共用一把钥匙，任一客户端（尤其带后台请求
// 的桌面客户端）都能调用任意模型并消耗额度。多 Key 让每个客户端/用途各拿一把，
// 各自绑定允许的模型与出口区域——越权请求在网关本地就被 400 挡掉，不送上上游、
// 不消耗任何额度。
type KeySpec struct {
	Key     string   // 密钥原文（仅内存持有，落盘在 config.json）
	Name    string   // 备注名（日志与报错用）
	Models  []string // 允许的模型（支持 * ? 通配；空 = 不限制）
	Realm   string   // 出口绑定："cn" / "global"；空 = 跟随请求
	Enabled bool
}

// Keyring 多条 API Key 的匹配表。
//
// 匹配用 SHA-256 摘要 + subtle.ConstantTimeCompare（与 VerifyBearer 同口径），
// 逐条比较不提前返回，避免以时序区分"密钥存在"与"密钥不存在"。
type Keyring struct {
	specs []KeySpec
}

// NewKeyring 构建匹配表；没有任何启用中的有效密钥时返回 nil（调用方回落单密钥）。
func NewKeyring(specs []KeySpec) *Keyring {
	enabled := make([]KeySpec, 0, len(specs))
	for _, s := range specs {
		if strings.TrimSpace(s.Key) == "" || !s.Enabled {
			continue
		}
		s.Key = strings.TrimSpace(s.Key)
		s.Name = strings.TrimSpace(s.Name)
		s.Realm = strings.ToLower(strings.TrimSpace(s.Realm))
		enabled = append(enabled, s)
	}
	if len(enabled) == 0 {
		return nil
	}
	return &Keyring{specs: enabled}
}

// Len 返回启用中的密钥条数。
func (k *Keyring) Len() int {
	if k == nil {
		return 0
	}
	return len(k.specs)
}

// Match 解析请求头里的 Bearer 密钥并返回命中的配置。
func (k *Keyring) Match(r *http.Request) (KeySpec, bool) {
	if k == nil || len(k.specs) == 0 {
		return KeySpec{}, false
	}
	authz := r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(authz, bearerPrefix) {
		tok = authz[len(bearerPrefix):]
	}
	var found KeySpec
	hit := false
	for _, s := range k.specs {
		// 全部比较都执行（不提前 break），保持耗时与命中位置无关。
		if subtleCompare(tok, s.Key) {
			found = s
			hit = true
		}
	}
	return found, hit
}

// subtleCompare 常量时间比较两个字符串（先摘要再比较，长度差异被吸收）。
func subtleCompare(a, b string) bool {
	return subtle.ConstantTimeCompare(digest(a), digest(b)) == 1
}

// MatchModel 报告模型名是否命中白名单；patterns 为空表示不限制。
//
// 支持 * 与 ? 通配（大小写不敏感），例如 "gpt-6-*"、"deepseek*"。
func MatchModel(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return true
	}
	if model == "" {
		return false
	}
	m := strings.ToLower(model)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if globMatch(p, m) {
			return true
		}
	}
	return false
}

// globMatch 单模式 glob 匹配（* 任意长度、? 单字符）。
func globMatch(pattern, s string) bool {
	// 迭代式回溯：线性空间，避免递归在长输入上爆栈。
	var (
		pi, si         int
		star, matchEnd = -1, 0
	)
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			matchEnd = si
			pi++
		case star >= 0:
			pi = star + 1
			matchEnd++
			si = matchEnd
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
