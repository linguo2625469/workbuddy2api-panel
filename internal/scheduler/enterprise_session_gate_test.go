package scheduler

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sessionTestScheduler 与 enterpriseTestScheduler 同构：switched=true 时账号的
// AccessToken 为带 token_source=enterprise_switch 的 JWT（企业**切换会话**，
// enterpriseId 为空——网关无法凭 enterpriseId 识别的形态）；false 为个人号对照组。
func sessionTestScheduler(t *testing.T, switched bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	token := "at"
	if switched {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch"}`))
		token = "hdr." + payload + ".sig"
	}
	a := &auth.Auth{UID: "u1", AccessToken: token, RefreshToken: "rt", ExpiresAt: 9999999999}
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

// TestEnterpriseSessionGateSkipsBehaviorTasks 企业切换会话的行为类任务门控：
// /v2/report 等行为事件对该形态返回 200 却被上游静默丢弃（实测 2026-10-12，
// 15 个成长任务全部 0/x 且 90s 内进度从不跳动），执行即零收益白跑——
// 活跃上报 / 连登管家 / 猫猫旅行 / 夜猫子必须一个上游调用都不发；
// 签到不在此列（对该形态未观测到静默失败，且失败是显式报错）。
// 每个子用例配个人号对照，证明"没发请求"是门控生效而非构造有误。
func TestEnterpriseSessionGateSkipsBehaviorTasks(t *testing.T) {
	// 与 TestEnterpriseGateSkipsGrowthTasks 同款：把夜猫子窗口钉在窗口内。
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name string
		run  func(*Scheduler)
		// endpoint 是该任务在**个人号**上必然发起的上游路径（对照组判据）。
		endpoint string
		// gated=true 期望切换会话不发该请求；false 期望照常发（不门控）。
		gated bool
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin", false},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report", true},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info", true},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks", true},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sSess, rSess := sessionTestScheduler(t, true)
			c.run(sSess)
			if c.gated && rSess.has(c.endpoint) {
				t.Errorf("企业切换会话不应发起 %s，实际请求=%v", c.endpoint, rSess.snapshot())
			}
			if !c.gated && !rSess.has(c.endpoint) {
				t.Errorf("企业切换会话应照常发起 %s（不门控），实际请求=%v", c.endpoint, rSess.snapshot())
			}

			sPer, rPer := sessionTestScheduler(t, false)
			c.run(sPer)
			if !rPer.has(c.endpoint) {
				t.Errorf("个人号对照应发起 %s，实际请求=%v", c.endpoint, rPer.snapshot())
			}
		})
	}
}

// TestEnterpriseSessionKeepsKeepalive 切换会话照常保活（token 会过期，会话
// 形态不影响选号/刷新能力）。
func TestEnterpriseSessionKeepsKeepalive(t *testing.T) {
	s, r := sessionTestScheduler(t, true)
	s.RunKeepaliveNow()
	if !r.has("/token/refresh") {
		t.Errorf("企业切换会话必须照常保活，实际请求=%v", r.snapshot())
	}
}
