package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// apiKeyTestScheduler 构造只含一个账号的调度器；apiKey=true 时该号是静态 API Key
// （AccessToken 以 ck_ 开头、无 refreshToken、expiresAt=0），否则是个人号（对照组）。
// 两个账号形态与真实落盘一致：静态 Key 本就没有 refresh token。
func apiKeyTestScheduler(t *testing.T, apiKey bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	if apiKey {
		a = &auth.Auth{UID: "u1", AccessToken: "ck_9f3a2b1c.deadbeef"}
	}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestAPIKeyGateSkipsGrowthTasks 静态 API Key 账号无个人成长体系：五类任务必须一个
// 上游请求都不发（企业版的同构门控见 enterprise_gate_test）。每个子用例配个人号
// 对照，证明"没发请求"是门控生效而不是构造有误。
func TestAPIKeyGateSkipsGrowthTasks(t *testing.T) {
	// 夜猫子窗口守卫先于账号循环：钉住时钟，避免该子用例的判别力依赖运行环境墙钟。
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 7, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name string
		run  func(*Scheduler)
		// endpoint 是该任务在个人号上必然发起的上游路径（对照组用）。
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sKey, rKey := apiKeyTestScheduler(t, true)
			c.run(sKey)
			if rKey.has(c.endpoint) {
				t.Errorf("API Key 账号不应发起 %s，实际请求=%v", c.endpoint, rKey.snapshot())
			}

			sPer, rPer := apiKeyTestScheduler(t, false)
			c.run(sPer)
			if !rPer.has(c.endpoint) {
				t.Errorf("个人号对照应发起 %s，实际请求=%v", c.endpoint, rPer.snapshot())
			}
		})
	}
}

// TestAPIKeyCheckinStillRefreshesCredits API Key 账号虽跳过签到，但**必须继续刷新
// 额度**（企业口径是 credit_floor 与面板额度的唯一数据源）。这正是 RunCheckinNow
// 的早退守卫放过无 refreshToken 的 API Key、且签到门控只 skip 不 continue 的原因。
func TestAPIKeyCheckinStillRefreshesCredits(t *testing.T) {
	s, r := apiKeyTestScheduler(t, true)
	s.RunCheckinNow()

	if r.has("/daily-checkin") {
		t.Errorf("API Key 账号不应发起签到，实际请求=%v", r.snapshot())
	}
	if !r.has("/get-enterprise-user-usage") {
		t.Fatalf("API Key 账号应走企业口径额度查询，实际请求=%v", r.snapshot())
	}
	st, ok := s.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	// limitNum - round(credit) = 2000 - 812 = 1188（recorderUpstream 企业响应）。
	if st.Credits != 1188 || st.CreditsTotal != 2000 {
		t.Errorf("企业额度映射错误：credits=%d total=%d want 1188/2000", st.Credits, st.CreditsTotal)
	}
}

// TestAPIKeyKeepaliveSkipped API Key 无法 refresh：保活路径不发任何上游请求。
// 对照个人号照常刷新（/token/refresh）。
func TestAPIKeyKeepaliveSkipped(t *testing.T) {
	sKey, rKey := apiKeyTestScheduler(t, true)
	sKey.RunKeepaliveNow()
	if rKey.has("/token/refresh") {
		t.Errorf("API Key 账号不应参与保活，实际请求=%v", rKey.snapshot())
	}

	sPer, rPer := apiKeyTestScheduler(t, false)
	sPer.RunKeepaliveNow()
	if !rPer.has("/token/refresh") {
		t.Errorf("个人号对照应发起保活，实际请求=%v", rPer.snapshot())
	}
}
