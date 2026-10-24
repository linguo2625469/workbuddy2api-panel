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

// TestIsEnterpriseSession 企业形态判定：**只看 enterpriseId**，不看 JWT 的
// token_source。
//
// 口径变更（issue #240 实测修正，2026-10-24）：旧实现把
// token_source=enterprise_switch 也判为企业上下文，理由是「该形态下 /v2/report
// 行为事件返回 200 却被上游静默丢弃，成长任务全 0/x」。该结论已被实测推翻：
// 两个 enterpriseId 为空、仅带 token_source=enterprise_switch 的**个人版**账号，
// 向 /v2/report 发一条 chat_request_send 事件后回读 /activity/growth/heatmap：
// today.score 与 is_active 均随之变化（0→14 / False→True 与 0→2 /
// False→True）——行为事件**照常计分**，成长体系可用。旧门控把这类可用账号的
// 连登/活跃上报/旅行/夜猫子/成长任务五类全挡掉了（白跑）。
//
// 现在：enterpriseId 非空 = 真企业版（成长端点上游 400 code 10001 / 403
// growth-only，确实无个人成长体系）；enterpriseId 为空 = 个人号，即便本次授权
// 签自企业切换会话也不门控。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		// token_source=enterprise_switch 但 enterpriseId 为空 → 个人号，不门控。
		{"enterprise_switch 无 enterpriseId", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), false},
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), false},
		{"带填充 base64url", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`)) + ".sig", false},
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

	t.Run("enterpriseId 非空 + enterprise_switch 仍为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("真企业版（enterpriseId 非空）应恒为企业上下文")
		}
	})

	t.Run("企业切换会话不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if a.IsEnterprise() {
			t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
