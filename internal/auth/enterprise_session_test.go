package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// IsEnterpriseSession 不校验签名，只解 payload，故签名段可为任意占位。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestIsEnterpriseSession 企业切换会话判定：JWT token_source=enterprise_switch
// 视为企业上下文；无该声明 / 非 JWT / 空 token 一律否（老凭证零回归）；
// enterpriseId 非空短路为真（与 IsEnterprise 同口径）。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"enterprise_switch", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), true},
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), false},
		{"带填充 base64url", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`)) + ".sig", true},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", false},
		{"非 JWT 形态", "opaque-token", false},
		{"空 token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("enterpriseId 非空短路为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"sub":"u1"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("IsEnterprise()=true 时 IsEnterpriseSession() 应恒为 true")
		}
	})

	t.Run("企业切换会话不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if a.IsEnterprise() {
			t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
