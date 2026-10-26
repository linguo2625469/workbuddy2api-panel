package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeKeepaliveOnly 签到已过点时按保活整点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll 签到与保活配到同一整点时两类任务都要执行。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // 禁用旅行时点干扰（仅测签到+保活同整点）
		ActivityHours:    []int{}, // 禁用活跃时点干扰
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
		BlackcatDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（同一时刻两任务）", kinds)
	}

	// 22 点过后下一次是次日 09:00，且只含签到（旅行/活跃已禁用）。
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled 两类任务全空时返回零值，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled 显式禁用签到后，排程里不再有签到时点（保活照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 21 点签到）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled 显式禁用保活后，排程里不再有保活时点（签到照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 22 点保活）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled 五类任务都显式禁用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls 四类任务全禁用：Run 不空转（只等退出信号），
// 且不能触发任何上游请求。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（四类全禁用）", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	travelCalls    atomic.Int32
	resourceRemain int64
	resourceEnd    string
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			// remain 需 <= size（取数钳 [0,size]：脏数据 remain>size 会被钳到 size
			// ——上游真实数据恒一致，实测 Cycle{17,482,500}）。
			end := ""
			if f.resourceEnd != "" {
				end = `,"CycleEndTime":` + jsonString(f.resourceEnd)
			}
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0` + end + `}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/info"):
			// 已领养（data.buddy 非空）：跳过领养前置，直接进旅行状态查询。
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"cat"}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/travel/status"):
			// daily_limit_reached=true：状态查询计一次调用即止，不发 depart/claim。
			f.travelCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1：12153 连续 N 次才禁用。前 2 次刷新失败不应杀号（误判防护）。
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	// 第 3 次连续 12153 → 禁用。
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess 两次 12153 后刷新成功 → 计数清零，
// 再来的 12153 从第 1 次重新计（不会因历史失败被继续追杀）。
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 第 3 次（本次调度循环的第二轮）刷新成功
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: 前 2 次不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 刷新成功 → 清计数
	// 接下来连续 2 次 12153：从新计数重新算，仍不应禁用（历史计数已清）。
	s.RunKeepaliveNow() // 12153 #1（新计数）
	s.RunKeepaliveNow() // 12153 #2（新计数）
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("刷新成功清计数后连续 2 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3（新计数）→ 禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("新计数第 3 次 12153 应禁用: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// TestRunBalanceRefreshNowUpdatesCreditsAndRevives 只查余额（不签到）即可更新 credits
// 并解冻余额恢复的冷却账号——面板手动刷新与后台周期任务共用该语义。
func TestRunBalanceRefreshNowUpdatesCreditsAndRevives(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	p.Disable("u2", "manual")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBalanceRefreshNow()

	st, _ := p.Status("u1")
	if st.Cooling || st.Credits != 777 {
		t.Errorf("u1 want revived with credits=777: cooling=%v credits=%d", st.Cooling, st.Credits)
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("balance refresh must not checkin, got %d calls", f.checkinCalls.Load())
	}
	// 禁用账号不参与：其 credits 保持 0（未被 UserResource 覆盖解冻）。
	if st2, _ := p.Status("u2"); !st2.Disabled {
		t.Errorf("u2 must stay disabled")
	}
}

// TestNextWakeGrowthSlot growth 排程进候选 + 禁用退场（每日自动执行成长任务队列）。
func TestNextWakeGrowthSlot(t *testing.T) {
	s := New(Config{GrowthHours: []int{1}})
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	hasGrowth := false
	for _, k := range kinds {
		if k == taskGrowth {
			hasGrowth = true
		}
	}
	if !hasGrowth || at.Hour() != 1 || at.Day() != 27 {
		t.Fatalf("growth 槽位: at=%v kinds=%v（期望 09-27 01:00 含 taskGrowth）", at, kinds)
	}
	// 禁用后不进候选（其余 kind 为空 → nextWake 零值返回）
	s2 := New(Config{GrowthHours: []int{1}, GrowthDisabled: true})
	_, kinds2 := s2.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	for _, k := range kinds2 {
		if k == taskGrowth {
			t.Fatal("禁用后 growth 仍在候选")
		}
	}
}

func TestRunBalanceRefreshReplacesExpirySnapshot(t *testing.T) {
	end := time.Now().In(time.FixedZone("CST", 8*3600)).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	f := &fakeUpstream{resourceRemain: 100, resourceEnd: end}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.SetCreditsDetailed("u1", 999, 999, 999, time.Now().Add(time.Hour), 999)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	s.RunBalanceRefreshNow()

	st, _ := p.Status("u1")
	if st.Credits != 100 || st.CreditsExpiring != 100 || st.CreditsEarliestRemaining != 100 || st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("snapshot=%+v", st)
	}

	f.resourceEnd = ""
	s.RunBalanceRefreshNow()
	st, _ = p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero expiry refresh did not clear snapshot=%+v", st)
	}
}

func TestSetExpiringSoonWindowClearsSnapshot(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 100, 50, time.Now().Add(time.Hour), 50)
	s := New(Config{Pool: p, ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.SetExpiringSoonWindow(24 * time.Hour)
	if got := s.ExpiringSoonWindow(); got != 24*time.Hour {
		t.Fatalf("window=%v want 24h", got)
	}
	st, _ := p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 {
		t.Fatalf("window change did not clear snapshot=%+v", st)
	}
}

// ---------------------------------------------------------------------------
// schedule.include_disabled_in_tasks：保号类四任务是否覆盖「已禁用」账号
// ---------------------------------------------------------------------------

// poolWithDisabledAccount 建一个两账号池：u1 可用、u2 被人工禁用（终态）。
func poolWithDisabledAccount() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u2", "manual disable (test)")
	return p
}

// TestRunCheckinSkipsDisabledByDefault 缺省（开关未开）下禁用账号不签到——锁定既有行为。
func TestRunCheckinSkipsDisabledByDefault(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}})

	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 1 {
		t.Errorf("checkin calls=%d want 1（仅 u1；禁用号默认跳过）", got)
	}
}

// TestRunCheckinIncludesDisabledWhenConfigured 开关打开后禁用账号也签到，
// 但**不被解冻**（ReenableIfCredits 对 disabled 是 no-op），且**仍不参与选号**。
func TestRunCheckinIncludesDisabledWhenConfigured(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool: p, Upstream: up,
		CheckinHours:           []int{9, 21},
		IncludeDisabledInTasks: true,
	})

	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（u1 + 禁用号 u2）", got)
	}
	if st, _ := p.Status("u2"); !st.Disabled {
		t.Errorf("禁用号签到后不应被自动解冻: %+v", st)
	}
	// 选号侧不受本开关影响：禁用号依旧不可选。
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("选号应仍只给 u1, got %+v", got)
	}
}

// TestRunKeepaliveIncludesDisabledRenewsToken 开关打开后禁用账号也续期 token
// （轮换用法下保持闲置号可用的关键）。
func TestRunKeepaliveIncludesDisabledRenewsToken(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, IncludeDisabledInTasks: true})

	s.RunKeepaliveNow()
	if got := f.refreshCalls.Load(); got != 2 {
		t.Errorf("refresh calls=%d want 2（含禁用号续期 token）", got)
	}
}

// TestRunBalanceRefreshIncludesDisabledUpdatesCredits 开关打开后禁用账号的积分也被刷新
// （面板据此判断下一个该启用谁），但不会被解冻。
func TestRunBalanceRefreshIncludesDisabledUpdatesCredits(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 700}
	srv := f.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, IncludeDisabledInTasks: true})

	s.RunBalanceRefreshNow()
	st, _ := p.Status("u2")
	if st.Credits != 700 {
		t.Errorf("禁用号 credits=%d want 700（余额刷新应覆盖禁用号）", st.Credits)
	}
	if !st.Disabled {
		t.Errorf("余额刷新不得解冻禁用号: %+v", st)
	}
}

// TestSetIncludeDisabledInTasksHot setter 热更新立即生效（面板保存配置走这条路径）。
func TestSetIncludeDisabledInTasksHot(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 1 {
		t.Fatalf("前置：缺省应只签 1 个，got %d", got)
	}

	s.SetIncludeDisabledInTasks(true)
	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 3 {
		t.Errorf("热更新后 checkin calls=%d want 3（1 + u1 + u2）", got)
	}
}

// TestPausedAccountStillRunsKeepaliveTasks 暂停选号的账号**照常参与**保号任务
// （签到 / 保活 / 余额刷新）——这是「暂停选号」与「禁用」的核心区别，也是本功能
// 的存在理由：轮换用法下让位的号仍需养着，否则积分断档、token 过期要重新登录。
// 注意：**不开** IncludeDisabledInTasks——paused 不依赖那个全局开关。
func TestPausedAccountStillRunsKeepaliveTasks(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 700}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	if !p.Pause("u2") {
		t.Fatal("Pause 失败")
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up}) // 有意不设 IncludeDisabledInTasks

	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（暂停号也签到，且无需全局开关）", got)
	}
	s.RunKeepaliveNow()
	if got := f.refreshCalls.Load(); got != 2 {
		t.Errorf("refresh calls=%d want 2（暂停号也续期 token）", got)
	}
	s.RunBalanceRefreshNow()
	if st, _ := p.Status("u2"); st.Credits != 700 {
		t.Errorf("暂停号 credits=%d want 700（余额刷新应覆盖）", st.Credits)
	}
	// 保号任务不得改变暂停状态（签到解冻的是冷却，不是 paused）
	if st, _ := p.Status("u2"); !st.Paused {
		t.Errorf("保号任务后暂停状态应保持: %+v", st)
	}
	// 选号侧始终排除暂停号
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("选号应只给 u1, got %+v", got)
	}
}

// TestPausedVsDisabledTaskParticipation 固化二者对比：都退出选号，但禁用号默认
// 跳过保号（除非开 include_disabled_in_tasks），暂停号**无条件**参与。
func TestPausedVsDisabledTaskParticipation(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 700}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Pause("u2")
	p.Disable("u3", "manual disable (test)")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up}) // 全局开关有意保持关闭

	s.RunCheckinNow()
	// u1（正常）+ u2（暂停）签到；u3（禁用）跳过 ⇒ 2 次
	if got := f.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（正常 + 暂停参与；禁用跳过）", got)
	}
}

// TestPausedStillTravels 暂停号照常跑旅行：旅行是纯 RPC（状态/派出/领奖 +
// 领养前置上报），不发模型对话，与「让位防风控」不冲突——唯一被跳过的
// 对话类任务只有夜猫子（RunNightChats 真实 ChatStream）。
func TestPausedStillTravels(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Pause("u2")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunTravelNow()
	if got := f.travelCalls.Load(); got != 2 {
		t.Errorf("travel status calls=%d want 2（u1 + 暂停号 u2 照常旅行）", got)
	}
}
