package auth

import (
	"testing"
	"time"
)

// TestIsAPIKeyMatrix 静态 API Key 判定矩阵：只看 AccessToken 前缀（ck_/pt_），
// JWT 与空 token 都不是 API Key。该判定驱动额度口径分流与成长体系门控。
func TestIsAPIKeyMatrix(t *testing.T) {
	cases := []struct {
		name string
		tok  string
		want bool
	}{
		{"ck_ 前缀", "ck_9f3a2b1c.deadbeef", true},
		{"pt_ 前缀", "pt_abc123.secret", true},
		{"JWT 形态", "eyJhbGciOiJIUzI1NiJ9.payload.sig", false},
		{"空 token", "", false},
		{"裸前缀无分隔", "ck_", true}, // 官方判据是前缀，不做形态完整性校验
		{"大小写敏感（CK_ 不算）", "CK_abc.def", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.tok}
			if got := a.IsAPIKey(); got != c.want {
				t.Errorf("token=%q IsAPIKey()=%v want %v", c.tok, got, c.want)
			}
		})
	}
}

// TestAPIKeyNeverNeedsRefresh 静态 Key 是永久凭证、无法 refresh（refresh 端点只认
// OAuth 设备流），故无论 expiry 形态都恒 false——否则 server 路径会先 refresh、失败
// 后把好号当坏号冷却换号。
func TestAPIKeyNeverNeedsRefresh(t *testing.T) {
	now := time.Now().Unix()
	for _, exp := range []int64{0, now - 10000, now + 60, now + 10*24*3600} {
		a := &Auth{UID: "u1", AccessToken: "ck_abc.def", ExpiresAt: exp}
		if a.NeedsRefresh(time.Hour) {
			t.Errorf("API Key（ExpiresAt=%d）不应需要 refresh", exp)
		}
	}
	// 对照：普通 OAuth 账号照旧按 expiry 判定（零回归）。
	expired := &Auth{UID: "u2", AccessToken: "at", ExpiresAt: now - 10}
	if !expired.NeedsRefresh(time.Hour) {
		t.Error("已过期的 OAuth 账号应需要 refresh")
	}
	noExpiry := &Auth{UID: "u3", AccessToken: "at", ExpiresAt: 0}
	if !noExpiry.NeedsRefresh(time.Hour) {
		t.Error("无 expiry 的 OAuth 账号应需要 refresh")
	}
	valid := &Auth{UID: "u4", AccessToken: "at", ExpiresAt: now + 10*3600}
	if valid.NeedsRefresh(time.Hour) {
		t.Error("未到期的 OAuth 账号不应需要 refresh")
	}
}

// TestUsesEnterpriseBilling 额度口径分流：企业版与静态 API Key 走企业口径端点，
// 个人号走个人资源包端点。
func TestUsesEnterpriseBilling(t *testing.T) {
	cases := []struct {
		name string
		ent  string
		tok  string
		want bool
	}{
		{"企业版", "ent-1", "at", true},
		{"静态 API Key", "", "ck_abc.def", true},
		{"个人号（对照）", "", "at", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", EnterpriseID: c.ent, AccessToken: c.tok}
			if got := a.UsesEnterpriseBilling(); got != c.want {
				t.Errorf("UsesEnterpriseBilling()=%v want %v", got, c.want)
			}
		})
	}
}

// TestSupportsPersonalGrowth 个人成长体系门控：企业版与静态 API Key 均无该体系，
// 只有个人号支持（对照）。
func TestSupportsPersonalGrowth(t *testing.T) {
	cases := []struct {
		name string
		ent  string
		tok  string
		want bool
	}{
		{"个人号", "", "at", true},
		{"企业版", "ent-1", "at", false},
		{"静态 API Key", "", "ck_abc.def", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", EnterpriseID: c.ent, AccessToken: c.tok}
			if got := a.SupportsPersonalGrowth(); got != c.want {
				t.Errorf("SupportsPersonalGrowth()=%v want %v", got, c.want)
			}
		})
	}
}
