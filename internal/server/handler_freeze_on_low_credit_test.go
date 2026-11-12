// handler_freeze_on_low_credit_test.go 低积分冻结闸门：开关 + 国际版豁免。
//
// 大部分用例直接调 applyErrorPolicy（未导出，同包可用）测闸门，避开 realm 选号/
// 轮转的复杂度：冻结的唯一生产写入点就是 ErrHardCredit 分支，断言它写/不写 CoolHard。
// 末尾补一条端到端（真实走 HTTP 选号）佐证 global 号 402 后不被冻结。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// enableGlobalRealm 打开 global 逃生门（进程级全局开关），用例结束复位。
func enableGlobalRealm(t *testing.T) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
}

// noopUpstream 直接调用 applyErrorPolicy 的用例不触网，但 newFakeUpstream 会无条件
// 调用 behavior，故给一个非 nil 的占位实现。
func noopUpstream(t *testing.T) *upstream.Client {
	return newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
}

// TestFreezeOnLowCreditDomesticDefaultFrozen 国内账号 + 默认开关 → 冻结（保持现状）。
func TestFreezeOnLowCreditDomesticDefaultFrozen(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: noopUpstream(t)})
	if !p.FreezeOnLowCredit() {
		t.Fatal("前置失效：Pool.New 默认应为 true")
	}
	h.applyErrorPolicy("cn", upstream.ErrHardCredit, `{"code":1,"msg":"余额不足"}`, "m", nil)
	st, _ := p.Status("cn")
	if !st.Cooling || st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("国内账号默认应被冻结到次日 04:00，got %+v", st)
	}
}

// TestFreezeOnLowCreditGlobalExempt 国际版账号无条件豁免（开关默认 true 也不冻）。
func TestFreezeOnLowCreditGlobalExempt(t *testing.T) {
	enableGlobalRealm(t)
	p := testPoolWith(&auth.Auth{UID: "g", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	h := NewHandler(Config{Pool: p, Upstream: noopUpstream(t)})
	h.applyErrorPolicy("g", upstream.ErrHardCredit, `{"code":1,"msg":"余额不足"}`, "m", nil)
	st, _ := p.Status("g")
	if st.Cooling || st.Disabled || st.CoolKind == "hard_credit" {
		t.Fatalf("国际版账号不应被冻结，got %+v", st)
	}
}

// TestFreezeOnLowCreditSwitchOff 开关关闭时国内账号也只换号、不冻结。
func TestFreezeOnLowCreditSwitchOff(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetFreezeOnLowCredit(false)
	h := NewHandler(Config{Pool: p, Upstream: noopUpstream(t)})
	h.applyErrorPolicy("cn", upstream.ErrHardCredit, `{"code":1,"msg":"余额不足"}`, "m", nil)
	st, _ := p.Status("cn")
	if st.Cooling || st.Disabled || st.CoolKind == "hard_credit" {
		t.Fatalf("开关关闭不应写冻结状态，got %+v", st)
	}
}

// TestFreezeOnLowCreditHotToggle 面板热改开关即时生效（同一池先后两次判定）。
func TestFreezeOnLowCreditHotToggle(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: noopUpstream(t)})
	h.applyErrorPolicy("a", upstream.ErrHardCredit, "余额不足", "m", nil)
	sta, _ := p.Status("a")
	if !sta.Cooling {
		t.Fatal("热改前 a 应冻结")
	}
	p.SetFreezeOnLowCredit(false)
	h.applyErrorPolicy("b", upstream.ErrHardCredit, "余额不足", "m", nil)
	stb, _ := p.Status("b")
	if stb.Cooling {
		t.Fatalf("热改关后 b 不应冻结，got %+v", stb)
	}
}

// TestFreezeOnLowCreditGlobalEndToEnd 端到端佐证：global 号 402 后不被冻结，
// 轮转到另一个 global 号拿到 200（真实走 HTTP 选号 + applyErrorPolicy）。
func TestFreezeOnLowCreditGlobalEndToEnd(t *testing.T) {
	enableGlobalRealm(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:m","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.CoolKind == "hard_credit" {
		t.Fatalf("402 的 global 号不应被冻结，got %+v", st)
	}
}
