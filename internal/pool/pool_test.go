package pool

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// withNoPickGap 临时关闭防并发撞号窗口（minPickGap=0），让纯加权分布测试不受影响。
func withNoPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })
}

func TestPickHighestCredits(t *testing.T) {
	withNoPickGap(t)
	// 三因子加权（credits 比例×10 + 闲置 + 成功率）：积分悬殊时高积分账号应被多数选中，
	// 但不再像纯 credits 加权那样接近 99%（闲置补偿 + 成功率中性 1.5 拉平了基线）。
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50000, 0)
	p.SetCredits("u3", 300, 0)
	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u2"] <= counts["u1"] || counts["u2"] <= counts["u3"] {
		t.Errorf("u2 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100, 0)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllDisabled(t *testing.T) {
	// 全禁用 → 兜底不参与（禁用账号永不参与兜底）→ 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil (all disabled), got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	withNoPickGap(t)
	// 加权随机不能选出冷却/禁用账号。
	p := New("")
	p.Add(&auth.Auth{UID: "u-cold"})
	p.Add(&auth.Auth{UID: "u-hot"})
	p.SetCredits("u-cold", 9999, 0)
	p.SetCredits("u-hot", 1, 0)
	p.Cooldown("u-cold", CoolHard, time.Hour, "x")
	for i := 0; i < 20; i++ {
		got := p.PickExcluding(nil)
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	withNoPickGap(t)
	// Top5 三因子加权：单账号 credits 占比足够高时，多数挑中它。
	p := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 1, 0)
	}
	p.SetCredits("w1", 1000, 0)
	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		counts[p.Pick().UID]++
	}
	mx, mxUID := 0, ""
	for uid, n := range counts {
		if n > mx {
			mx, mxUID = n, uid
		}
	}
	if mxUID != "w1" {
		t.Errorf("w1 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	withNoPickGap(t)
	// credits 全为 0 → 退化为均匀随机，不能只挑固定一个。
	p := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		p.Add(&auth.Auth{UID: u})
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.Pick().UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	withNoPickGap(t)
	// 第 6 高 credits 的账号在 Top5 之外，权重抽签永远轮不到它。
	p := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a1", 1000, 0)
	p.SetCredits("a2", 1000, 0)
	p.SetCredits("a3", 1000, 0)
	p.SetCredits("a4", 1000, 0)
	p.SetCredits("a5", 1000, 0)
	p.SetCredits("a6", 5, 0) // Top5 之外
	for i := 0; i < 2000; i++ {
		if got := p.Pick(); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickTopFiveByIdleNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 回归：闲置补偿同样影响短名单。a1..a5 credits=100 但刚被用过（闲置 0），
	// a6 credits=90 但从未使用（闲置满分）。纯 credits 排序时 a6 进不了 top5；
	// 三因子权重下 a6 权重最高，首轮必被选中。仅断言首轮。
	p := New("")
	now := time.Now()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// a1..a5 全部"刚被用过"，闲置补偿归零；a6 从未使用 → 闲置满分。
	p.mu.Lock()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.byUID[u].lastUsed = now
	}
	p.mu.Unlock()

	if got := p.Pick(); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (idle low-credit must enter top5 by weight)", got)
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	// r=0 ∈ [0,50) → 命中 u1。注入源应使选号完全确定。
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestPickAntiThunderingHerd(t *testing.T) {
	// 100 goroutine 同时 Pick：防并发撞号窗口内同一账号不应被重复选中。
	// credits 相同 → 无注入源时加权随机应天然打散；为保证稳定，全部置 0 走均匀随机。
	//
	// minPickGap 置 0（源码注释标注的测试开关）：Windows 时钟粒度粗，整轮并发
	// Pick 可落在同一时钟刻度内——所有 lastUsed 时间戳相等，eligible 恒空走 LRU
	// 兜底，而 LRU 对相等时间戳按 UID 稳定 tie-break，结果 100 次全命中 c00。
	// 关闭窗口后本用例回归其真实断言口径：加权随机自身的打散性（跨平台稳定）。
	oldGap := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = oldGap })

	p := New("")
	for i := 0; i < 10; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%02d", i)})
	}
	// 关键：验证并发中任意瞬间不会全选同一账号。
	const N = 100
	var wg sync.WaitGroup
	picked := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if a := p.Pick(); a != nil {
				picked[idx] = a.UID
			}
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, uid := range picked {
		if uid != "" {
			counts[uid]++
		}
	}
	// 选号必须覆盖多个账号，且最热门的账号不超过一半。
	if len(counts) < 2 {
		t.Fatalf("anti-thundering-herd failed: all %d picks hit %d account(s) %v", N, len(counts), counts)
	}
	for uid, n := range counts {
		if n > N/2 {
			t.Errorf("account %s picked %d/%d (>50%%): thundering herd", uid, n, N)
		}
	}
}

func TestPickLRUFallbackWhenTopAllRecentlyUsed(t *testing.T) {
	// top5 全部刚被选中 → LRU 兜底应挑最近最少使用的那个（= 最早 lastUsed）。
	old := minPickGap
	minPickGap = time.Hour // 超大窗口：任何 lastUsed 都在窗口内
	defer func() { minPickGap = old }()

	p := New("")
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("a%d", i)})
	}
	// 直接构造 lastUsed：不经过 Pick（避免 Pick 改写 lastUsed）。
	order := []string{"a4", "a3", "a2", "a1", "a0"}
	p.mu.Lock()
	for i, uid := range order {
		p.byUID[uid].lastUsed = time.Now().Add(-time.Duration(len(order)-i) * time.Second) // a4 最旧
	}
	p.mu.Unlock()

	got := p.Pick()
	if got == nil {
		t.Fatal("pick returned nil")
	}
	if got.UID != "a4" {
		t.Errorf("LRU fallback picked %s want a4 (oldest lastUsed)", got.UID)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush() // 状态变更走 dirty 标志，落盘由 Flush / 后台 goroutine 负责
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling || st.Reason != "余额不足" {
		t.Fatalf("cooldown lost after reload: %+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500, 0)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorAccumulatesErrTotal(t *testing.T) {
	// NoteError 语义变更：不再有独立的 err 冷却（CoolErr 已并入熔断器），
	// 只累计 errTotal（不清零，供成功率权重）并喂熔断器 fails。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("NoteError alone must not set cooling (no CoolErr): %+v", st)
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err not set")
	}
}

func TestNoteSuccessResetsBreakerNotErrTotal(t *testing.T) {
	// NoteSuccess 清 fails/熔断（运行态），但不清 errTotal（累计值）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if p.internalHealthy("u1") {
		t.Fatal("breaker should be open (unhealthy) after 2 failures")
	}
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2 (cumulative, not cleared by success)", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("success should clear breaker: %+v", st)
	}
}

func TestNoteSuccessIncrementsAndRecords(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.LastSuccessTime.Before(before) {
		t.Errorf("last_success=%v before call", st.LastSuccessTime)
	}
	if !st.LastErrTime.IsZero() {
		t.Errorf("last_err should be zero for fresh success: %v", st.LastErrTime)
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	// C5：签到解冻只清冷却（until/coolKind/reason）+ 更新 credits，不清熔断
	// （fails/retryCount/breakerUntil）。签到成功只证明余额与 billing 通道恢复，
	// 不证明 chat 通道健康——熔断仍按 breakerUntil 退避到期或 NoteSuccess 恢复。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足") // 硬冷却（喂 fails，但此时阈值默认 3，不熔断）
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断（fails→阈值1→fails=0, retryCount=1, breakerUntil 非零）
	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.Reason != "" || st.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", st)
	}
	if st.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", st.Until)
	}
	if st.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// 熔断仍在 → 账号仍不可选（直至 breakerUntil 到期）。
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	// C5 回归锁定新语义：仅熔断（无冷却）的账号，签到解冻不得清熔断。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	p.ReenableIfCredits("u1", 500, 0)
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestCoolKindPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush()

	// 旧文件缺新字段时零值 → 冷却应仍工作（向后兼容）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling {
		t.Fatalf("cooldown state lost after reload: %+v ok=%v", st, ok)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind after reload=%q want hard_credit", st.CoolKind)
	}
}

func TestStateRoundTripExtendedFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.NoteSuccess("u1") // successCount=1，last_success 非零
	p.NoteSuccess("u1") // successCount=2
	p.NoteError("u1")   // errTotal=1（累计），last_err 非零
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// JSON tag 全小写下划线；err_total 落盘，err_count 不再落盘。
	for _, want := range []string{`"cool_kind"`, `"success_count"`, `"err_total"`, `"last_success"`, `"last_err"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"err_count"`) {
		t.Errorf("state.json should not write legacy err_count:\n%s", raw)
	}

	// 重载后字段保留
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.SuccessCount != 2 || st.CoolKind != "hard_credit" {
		t.Errorf("reloaded portrait=%+v", st)
	}
	if st.ErrTotal != 1 {
		t.Errorf("reloaded err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() || st.LastErrTime.IsZero() {
		t.Error("last_success/last_err lost after reload")
	}
}

func TestLoadLegacyErrCountMigratesToErrTotal(t *testing.T) {
	// 迁移测试：旧 state.json 只含 err_count（连续错误）→ 加载后 err_total 正确。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100,"err_count":7}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.ErrTotal != 7 {
		t.Errorf("err_total=%d want 7 (migrated from legacy err_count)", st.ErrTotal)
	}
	// 新字段优先：二者并存时取较大者。
	both := `{"accounts":{"u1":{"credits":100,"err_count":3,"err_total":9}}}`
	if err := os.WriteFile(fp, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st2, _ := p2.Status("u1"); st2.ErrTotal != 9 {
		t.Errorf("err_total=%d want 9 (new field wins over legacy)", st2.ErrTotal)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 下一个 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// 凌晨 00:00~04:00 触发硬冷却：当天 04:00 尚未到，冷却应落在当天（而非次日），
		// 否则多冷约一天（原 bug）。
		{"凌晨02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发时落在当天 04:00，其余时段落在次日 04:00，跨度恒 < 24h）。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// 全冷却时余额耗尽（hard）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "余额不足" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

// ---------------------------------------------------------------------------
// 软冷却指数退避（softStreak）
// ---------------------------------------------------------------------------

// wantCoolSec 断言账号当前冷却剩余秒数 ≈ want（±tol 秒，容忍测试内的 tick 漂移）。
func wantCoolSec(t *testing.T, p *Pool, uid string, want int64, tol int64) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("%s should be in soft_rate cooling: %+v", uid, st)
	}
	if got := st.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// expireCooldown 测试助手：把账号冷却截止回拨到过去，模拟冷却已到期
// （CooldownSoftRate 只在"不在有效软冷却中"时推进 streak——跨冷却期堆加）。
func expireCooldown(p *Pool, uid string) {
	p.mu.Lock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(-time.Second)
	}
	p.mu.Unlock()
}

func TestCooldownSoftExponentialBackoff(t *testing.T) {
	// 同一账号**跨冷却期**连续软限流 → 时长按 2 倍指数增长（吸收上游：冷却中的
	// 兜底探测不再堆加，堆加只发生在到期后的新一轮限流）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour) // 封顶 1h：本用例三步（600/1200/2400）都不触及

	for i, want := range []int64{600, 1200, 2400} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1") // 模拟冷却到期后再撞新一轮限流
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	// 注入封顶：streak 3 的 400s 被压到 250s（跨冷却期堆加）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(250 * time.Second)

	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 100, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 200, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	// 未注入 softRateMax → 按 2h 封顶（避免裸用池时退避无上限）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1")
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	// 非正值保留原值（风格同 SetBreaker）：0 不应把封顶清零导致无上限。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(0)
	p.SetSoftRateMax(-time.Second)
	for i := 0; i < 6; i++ {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		if i < 5 {
			expireCooldown(p, "u1") // 最后一轮不回拨：断言时须仍在冷却中
		}
	}
	wantCoolSec(t, p, "u1", 7200, 3) // 仍是 2h 封顶（第 6 步 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	// 成功即证明账号恢复 → streak 归零，下次软冷却回到基数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 1200, 3)

	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}
	expireCooldown(p, "u1") // 新一轮限流（上一轮冷却已过/已被成功重置）
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownSoftKeptByReenable(t *testing.T) {
	// 签到/余额刷新解冻（reviveCoolingLocked）只解冻余额耗尽冷却（CoolHard）；
	// 软限流冷却（CoolSoft）与 softStreak 保留——限流恢复证据是重置墙钟/退避到期，
	// 不是余额恢复（余额刷新每 5 分钟一次，若在此清冷却域，限流保护实际寿命被压到
	// 一个刷新周期内）。熔断域（fails/retryCount/breakerUntil）不动，与既有 C5 语义一致。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1") // 跨冷却期第二次限流，streak 累计到 2（冷却中重复触发不堆叠，#152）
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := p.breakerFails("u1")

	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("reenable 不得解除软限流冷却: %+v", st)
	}
	if st.SoftStreak != 2 {
		t.Errorf("reenable 不得清零软限流退避计数 soft_streak, got %d want 2", st.SoftStreak)
	}
	if failsAfter := p.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	// 退避延续：第 3 次触发从既有 streak=2 继续 → 600s<<2 = 2400s（而非归零后的 600s）。
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 2400, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	// 硬冷却（余额耗尽）时长由签到时点决定，不参与软退避指数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftStreakPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"soft_streak"`) {
		t.Fatalf("state.json missing soft_streak: %s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.SoftStreak != 2 {
		t.Fatalf("soft_streak after reload=%d want 2", st.SoftStreak)
	}
	// 退避从持久化的 streak 继续：第 3 次 → 2400s。
	expireCooldown(p2, "u1")
	p2.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p2, "u1", 2400, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	// 旧 state.json 无 soft_streak → 零值兼容，退避从基数重新开始。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// ---------------------------------------------------------------------------
// issue #31：429 6004 模型级限流 → 按上游重置时间收窄冷却 + 模型级豁免选号
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	// 6004 msg 带「将在 … 重置」→ 该模型的独立冷却截止精确等于解析时间（wall-clock 判断）。
	// 用未来 5 分钟的时间戳：解析后 Until ≈ now+5m，远短于固定 600s 基数的指数退避，
	// 证明"上游明说重置时间"优先于"600s 起指数退避"。
	// 新语义：只写 modelCooldowns（不写账号级 until）→ 账号不 cooling、台账单行。
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row for glm-5.3: %+v", st.RateLimitedModels)
	}
	until := st.RateLimitedModels[0].Until
	if d := until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	// 解析时间超出 soft_rate_max → 截断到 soft_rate_max（不无限期拉黑）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超过封顶 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("want model ledger row: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.Sub(before) > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", st.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	// 无解析时间（resetAt 零值）→ 有界退避（600s 起）。冷却中的兜底探测再撞 429
	// **不推进 streak、不延长**（吸收上游：旧「每次探测都翻倍」正是全池被推到
	// 2h 封顶的元凶）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3) // 仍在冷却中：不堆加
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel 冷却中账号（6004 带解析时间，
// 已记录模型）+ 同 model 请求 → 仍不可选（现状语义保持）。
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 冷却中的账号 + 不同 model
// → 视为可用，可选到该号（真·单模型限流，切模型立即可用）。
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestCooldownSoftWithoutModelRecordsNone 非 6004 的普通软冷却（resetAt 零值，
// 不记录 softRateModel）→ 不因模型切换而豁免（现状语义）。
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// 冷却中 + 不同 model 请求仍跳过 u1（无 softRateModel，不豁免）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks 模型豁免只豁免软冷却维度，
// 熔断（breakerUntil）仍拦截：6004 冷却 + 熔断中的账号，切模型也不可选。
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // u1 熔断
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// TestSoftRateModelClearedByPlainCooldown 回归：6004 模型冷却后，若账号又经历一次
// **非模型级**软冷却（plain Cooldown），softRateModel 必须被清空——否则上次 6004 的
// 模型豁免会泄漏到本次账号级限流上，导致"换模型请求"错误绕过本次冷却。
func TestSoftRateModelClearedByPlainCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 带解析时间 → 记录模型 glm-5.3。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) 账号恢复后经历普通账号级软冷却（无模型语义）。
	p.NoteSuccess("u1") // 还原 fresh 状态（Cooldown 会重设 until）
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) 换模型请求不得再豁免（softRateModel 已清空）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear softRateModel (no bypass), got %+v", got)
	}
}

// TestSoftRateModelNotPersistedToState 模型级独立冷却（modelCooldowns）是运行态：
// 落盘不引入该字段，重启清零（退化为仅账号级 until 冷却的现状）。
// 新语义：6004 带重置时间只写 modelCooldowns、不写 until → 重载后账号不冷却、
// 台账为空。
func TestSoftRateModelPersistsToState(t *testing.T) {
	// 6004 重置墙钟可长达数小时，跨重启是常态：model_cooldowns 持久化，
	// 恢复后 healthyForModel 不失忆（吸收上游 2f4c77b）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// resetAt 必须显著晚于 now：传 time.Now() 会走"重置时间已过"分支把冷却压到 1ms。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json 应持久化 model_cooldowns: %s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("account missing after reload")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset 不写账号级 until，重载后不应 cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("modelCooldowns should survive reload, got %+v", st.RateLimitedModels)
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42, 0)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestAutoFlush(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = old }()

	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77, 0)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(fp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // 无 dirty，不应写盘
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}

func TestSaveFailureRecordedAndRecovers(t *testing.T) {
	// stateFp 的父路径是一个普通文件（非目录）→ MkdirAll/WriteFile 必失败，
	// root 也不可绕过，可靠地触发落盘失败路径。
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("persist failure should be recorded (visible), got 0")
	}

	// 换回可写目录 → 成功后 persistFails 归零（恢复日志由零值门槛触发）。
	good := filepath.Join(t.TempDir(), "state.json")
	p2 := New(good)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 42, 0)
	p2.Flush()
	if p2.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", p2.persistFails)
	}
	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on success: %v %s", err, raw)
	}
}

// ---------------------------------------------------------------------------
// T2 熔断器 + 全冷却兜底 + 指数退避
// ---------------------------------------------------------------------------

// breakerUntil 曝露内部运行态供测试断言（包内私有 helper）。
func (p *Pool) breakerUntil(uid string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, false
	}
	return e.breakerUntil, true
}

// breakerFails 曝露 entry.fails 供测试断言（包内私有 helper）。
func (p *Pool) breakerFails(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].fails
}

// internalHealthy 曝露 entry.healthy 供测试断言（包内私有 helper）。
func (p *Pool) internalHealthy(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthy(time.Now())
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	for i := 0; i < 2; i++ {
		p.NoteError("u1") // NoteError 只驱动熔断（不再有单独 err 冷却）
		if bt, ok := p.breakerUntil("u1"); ok && !bt.IsZero() {
			t.Fatalf("breaker tripped too early at %d: %v", i+1, bt)
		}
	}
	p.NoteError("u1")
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatalf("breaker should trip at threshold: until=%v ok=%v", bt, ok)
	}
}

func TestBreakerSuccessClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	p.NoteSuccess("u1")
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Fatalf("success should clear breaker, until=%v", bt)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("account should be healthy after success clears breaker")
	}
}

func TestBreakerExponentialBackoffCapped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Minute, 4*time.Minute) // threshold=3：连续 3 次失败熔断一次
	// 连续 9 次失败（无成功）→ 熔断 3 次，retryCount 1→2→3，退避 1m→2m→4m(封顶)。
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			p.NoteError("u1") // 连续失败只驱动熔断
		}
	}
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	d := time.Until(bt)
	// 第 3 次熔断：d = min(1m * 2^2, 4m) = 4m
	if d < 4*time.Minute-time.Second || d > 4*time.Minute+time.Second {
		t.Errorf("backoff should cap at max=4m, got %v", d)
	}

	// 对比第 1 次熔断（新账号重新来）：退避应更短。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetBreaker(3, time.Minute, 4*time.Minute)
	for j := 0; j < 3; j++ {
		p2.NoteError("u1")
	}
	bt1, _ := p2.breakerUntil("u1")
	if d1 := time.Until(bt1); d1 > time.Minute+time.Second {
		t.Errorf("first trip should be ~1m, got %v", d1)
	}
}

func TestFallbackPicksEarliestExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却；early 更早到期 → 兜底选 early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")
	got := p.Pick()
	if got == nil || got.UID != "early" {
		t.Fatalf("fallback should pick earliest expiry (early), got %+v", got)
	}
}

func TestFallbackSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "dead"})
	p.Cooldown("cooled", CoolSoft, time.Hour, "x")
	p.Disable("dead", "session dead") // 禁用不参与兜底
	got := p.Pick()
	if got == nil || got.UID != "cooled" {
		t.Fatalf("fallback should skip disabled, got %+v", got)
	}
}

func TestFallbackSkipsHardCooldown(t *testing.T) {
	// D3：余额耗尽（CoolHard）号不参与兜底——调了必 402，浪费轮换并产生噪音日志。
	p := New("")
	p.Add(&auth.Auth{UID: "hard"})
	p.Cooldown("hard", CoolHard, time.Hour, "余额不足")
	if got := p.Pick(); got != nil {
		t.Fatalf("hard-cooled account must not be fallback-picked, got %+v", got)
	}
}

func TestFallbackAllHardReturnsNil(t *testing.T) {
	// 全 hard 冷却 → 无软冷却/熔断号可兜底 → 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "h1"})
	p.Add(&auth.Auth{UID: "h2"})
	p.Cooldown("h1", CoolHard, time.Hour, "x")
	p.Cooldown("h2", CoolHard, 2*time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard should return nil, got %+v", got)
	}
}

func TestFallbackSoftAndBreakerParticipate(t *testing.T) {
	// D3：soft 与 breaker 冷却号允许参与兜底，取最早到期者。
	p := New("")
	p.Add(&auth.Auth{UID: "soft"})
	p.Add(&auth.Auth{UID: "brk"})
	p.Cooldown("soft", CoolSoft, 10*time.Minute, "429") // soft: until=10m, fails=1
	p.SetBreaker(2, 5*time.Minute, 5*time.Minute)       // 阈值 2：soft 的 1 次失败不熔断
	p.NoteError("brk")                                  // brk: fails=1
	p.NoteError("brk")                                  // brk: 熔断，breakerUntil=5m
	got := p.Pick()
	if got == nil {
		t.Fatal("fallback should pick breaker (earliest) account")
	}
	if got.UID != "brk" {
		t.Fatalf("fallback should pick earliest expiry brk (5m < soft 10m), got %+v", got)
	}
}

func TestFallbackNilWhenAllDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil when all disabled, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T3 三因子加权选取
// ---------------------------------------------------------------------------

// idleWeightOf 曝露 weightOf 的单因子拆解不便，改用完整权重断言（包内私有 helper）。
func (p *Pool) entryWeight(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}
	return p.weightOf(e, maxCredits, time.Now())
}

func TestWeightHighCreditsDominates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetCredits("hi", 1000, 0)
	p.SetCredits("lo", 10, 0)
	wHi, wLo := p.entryWeight("hi"), p.entryWeight("lo")
	if wHi <= wLo {
		t.Errorf("high credits should weigh more: hi=%v lo=%v", wHi, wLo)
	}
}

func TestWeightIdleCompensation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "used"})
	p.Add(&auth.Auth{UID: "idle"})
	p.SetCredits("used", 100, 0)
	p.SetCredits("idle", 100, 0)
	// used 1 小时前被选中过、idle 从未使用 → idle 权重更高（闲置补偿）。
	p.mu.Lock()
	p.byUID["used"].lastUsed = time.Now().Add(-1 * time.Hour)
	p.mu.Unlock()
	wUsed, wIdle := p.entryWeight("used"), p.entryWeight("idle")
	if wIdle <= wUsed {
		t.Errorf("idle should weigh more: used=%v idle=%v", wUsed, wIdle)
	}
}

func TestWeightAllZeroCreditsStillWeighted(t *testing.T) {
	// credits 全 0：权重完全由 idle+successRate 决定，不退化均匀随机（仍可选出更高分者）。
	p := New("")
	p.Add(&auth.Auth{UID: "idle"})
	p.Add(&auth.Auth{UID: "bursty"})
	// idle 从未使用、bursty 半分钟前刚用过 → idle 权重更高。
	p.mu.Lock()
	p.byUID["bursty"].lastUsed = time.Now().Add(-30 * time.Second)
	p.mu.Unlock()
	wIdle, wBursty := p.entryWeight("idle"), p.entryWeight("bursty")
	if wIdle <= wBursty {
		t.Errorf("idle should outweigh recently-used when credits all zero: idle=%v bursty=%v", wIdle, wBursty)
	}
}

func TestWeightTopFiveSelectionChanges(t *testing.T) {
	withNoPickGap(t)
	// credits 相差不大时，闲置补偿可让"低分但久置"的账号权重反超"高分但刚用"的账号，
	// 即使 credits 排序里 b 在前（Top5 内权重排序可与 credits 排序不同）。
	p := New("")
	for _, u := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a", 90, 0) // a credits 略低，但久置
	p.SetCredits("b", 100, 0)
	p.mu.Lock()
	p.byUID["b"].lastUsed = time.Now()
	p.byUID["a"].lastUsed = time.Now().Add(-48 * time.Hour)
	p.mu.Unlock()
	if wA, wB := p.entryWeight("a"), p.entryWeight("b"); wA <= wB {
		t.Errorf("idle a should outweigh busy higher-credit b: a=%v b=%v", wA, wB)
	}
}

// ---------------------------------------------------------------------------
// T4 在途租约（单账号并发上限）
// ---------------------------------------------------------------------------

func TestAcquireReleaseLifecycle(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	if !p.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}
	if !p.Acquire("u1") {
		t.Fatal("second acquire should succeed")
	}
	if p.Acquire("u1") {
		t.Fatal("third acquire should fail (limit 2)")
	}
	p.Release("u1")
	if !p.Acquire("u1") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestAcquireUnlimited(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// max=0 不限：连续 acquire 永不拒绝。
	for i := 0; i < 100; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
}

func TestAcquireUnknownUID(t *testing.T) {
	p := New("")
	if p.Acquire("nope") {
		t.Fatal("acquire unknown uid should fail")
	}
	p.Release("nope") // 不 panic
}

func TestPickSkipsInFlightFull(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "free"})
	p.SetCredits("full", 1000, 0)
	p.SetCredits("free", 1, 0)
	p.SetMaxInFlight(1)
	// full 占满唯一名额 → Pick 应跳过它，选 free（即使 credits 更低）。
	p.Acquire("full")
	got := p.Pick()
	if got == nil || got.UID != "free" {
		t.Fatalf("pick should skip in-flight-full account, got %+v", got)
	}
	p.Release("full")
	// 释放后可重新被选中（确定性随机源 r=0 → 选 credits 最高的 full）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(); got == nil || got.UID != "full" {
		t.Fatalf("after release full should be pickable, got %+v", got)
	}
	p.Release("full")
}

func TestInFlightCountNotExceedLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)

	// 并发 50 次 acquire：CAS 保证任一时刻在途数不超上限；每次成功后立即 release。
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire("u1") {
				// 峰值检查：acquire 成功后立即读计数，应 ≤ 2。
				p.mu.RLock()
				if n := p.byUID["u1"].inFlight.Load(); n > 2 {
					t.Errorf("in-flight exceeded limit: %d", n)
				}
				p.mu.RUnlock()
				p.Release("u1")
			}
		}()
	}
	wg.Wait()

	// 全部释放后计数必须为 0。
	p.mu.RLock()
	n := p.byUID["u1"].inFlight.Load()
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("in-flight should be 0 after all releases, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T6 向后兼容 + 运行态 Status 扩展
// ---------------------------------------------------------------------------

func TestLoadLegacyStateFile(t *testing.T) {
	// 旧 state.json 只含 credits/until/disabled 等老字段，缺熔断/在途/成功率新字段。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"legacy":{"credits":123,"until":"2027-01-01T04:00:00+08:00","cool_kind":1,"reason":"余额不足"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	st, ok := p.Status("legacy")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.Credits != 123 || !st.Cooling || st.Reason != "余额不足" {
		t.Errorf("legacy state misloaded: %+v", st)
	}
	// 运行态新字段默认零值。
	if st.InFlight != 0 || st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("runtime fields should be zero for legacy load: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// T7 D5: Redis 状态快照镜像 + 择新恢复
// ---------------------------------------------------------------------------

// memStore 内存假 Store：记录 SaveState（模拟 Redis 快照）并可按需返回 LoadState。
type memStore struct {
	mu       sync.Mutex
	saved    []byte
	loadData []byte
	loadOK   bool
}

func (m *memStore) SaveState(data []byte) {
	m.mu.Lock()
	m.saved = append([]byte(nil), data...)
	m.mu.Unlock()
}
func (m *memStore) LoadState() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loadOK {
		return nil, false
	}
	return append([]byte(nil), m.loadData...), true
}

func TestSaveMirrorsSnapshot(t *testing.T) {
	// Flush 落盘时同步 fire-and-forget SaveState（带 saved_at）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	ms := &memStore{}
	p.SetStore(ms)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	ms.mu.Lock()
	raw := string(ms.saved)
	ms.mu.Unlock()
	if !strings.Contains(raw, `"saved_at"`) {
		t.Fatalf("snapshot should carry saved_at: %s", raw)
	}
	if !strings.Contains(raw, `"credits":42`) {
		t.Fatalf("snapshot should carry account state: %s", raw)
	}
}

func TestRestoreUsesRedisWhenNewer(t *testing.T) {
	// Redis 快照比本地 state.json 新 → 采用 Redis。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 本地较旧
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 把本地 mtime 设到过去
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fp, old, old); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesLocalWhenNewer(t *testing.T) {
	// 本地 state.json 比 Redis 快照新 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":77}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now().Add(-time.Hour)}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("should keep local (newer): %+v ok=%v", st, ok)
	}
}

func TestRestoreNoRedisUsesLocal(t *testing.T) {
	// 无 Redis 快照 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":55}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: false}
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 55 {
		t.Fatalf("no redis → use local: %+v ok=%v", st, ok)
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	p.Acquire("u1") // in_flight=1
	st, _ := p.Status("u1")
	if st.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", st.InFlight)
	}
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1") // breaker_fails=1
	st, _ = p.Status("u1")
	if st.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", st.BreakerFails)
	}
	p.Release("u1")
}

func TestRecordTokenUsage(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model:               "glm-5.2",
		HasPromptTokens:     true,
		PromptTokens:        5,
		HasCompletionTokens: true,
		CompletionTokens:    7,
		HasTotalTokens:      true,
		TotalTokens:         12,
		HasLatencyMs:        true,
		LatencyMs:           1250,
		HasTokensPerSecond:  true,
		TokensPerSecond:     9.6,
	})
	p.RecordTokenUsage("u1", TokenUsageDelta{Model: "glm-5.2", HasLatencyMs: true, LatencyMs: 300})
	st, _ := p.Status("u1")
	if st.TokenUsage.RequestCount != 2 {
		t.Errorf("request_count=%d want 2", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("usage_count=%d want 1", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.PromptTokens != 5 || st.TokenUsage.CompletionTokens != 7 || st.TokenUsage.TotalTokens != 12 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 300 || st.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("latest performance should replace speed with unknown: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastModel != "glm-5.2" || st.TokenUsage.LastUsedAt.Before(before) {
		t.Errorf("last usage=%+v", st.TokenUsage)
	}
}

func TestTokenUsagePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model:               "deepseek-v4",
		HasPromptTokens:     true,
		PromptTokens:        11,
		HasCompletionTokens: true,
		CompletionTokens:    13,
		HasTotalTokens:      true,
		TotalTokens:         24,
		HasLatencyMs:        true,
		LatencyMs:           2300,
		HasTokensPerSecond:  true,
		TokensPerSecond:     5.65,
	})
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("account missing after reload")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.TotalTokens != 24 || st.TokenUsage.LastModel != "deepseek-v4" {
		t.Errorf("token usage lost after reload: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 2300 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond != 5.65 {
		t.Errorf("latest performance lost after reload: %+v", st.TokenUsage)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"token_usage"`) {
		t.Fatalf("state.json missing token_usage: %s", raw)
	}
	if strings.Contains(string(raw), "AccessToken") || strings.Contains(string(raw), "RefreshToken") {
		t.Fatalf("state.json contains credential field: %s", raw)
	}
}

func TestPickPrefersExpiringByVirtualWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "later"})
	p.Add(&auth.Auth{UID: "soon"})
	p.Add(&auth.Auth{UID: "none"})

	now := time.Now()
	p.SetCreditsDetailed("later", 100, 100, 100, now.Add(48*time.Hour), 100)
	p.SetCreditsDetailed("soon", 100, 100, 100, now.Add(2*time.Hour), 100)
	p.SetCreditsDetailed("none", 100, 100, 0, time.Time{}, 0)

	// 3:1 虚拟实例是软偏好而非硬优先。用确定性随机源统计长期分布：两个快过期
	// 账号的合计份额应显著高于普通账号，同时普通账号仍保留少量流量。
	rng := rand.New(rand.NewPCG(1, 2))
	p.SetRandomSource(func(n int64) int64 { return rng.Int64N(n) })
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("pick returned nil")
		}
		counts[got.UID]++
	}
	expiring := counts["soon"] + counts["later"]
	if expiring < 1500 || counts["none"] == 0 {
		t.Fatalf("virtual weight distribution=%v, want expiring majority and regular non-zero", counts)
	}
}

func TestPickExpiringTieUsesExistingWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "small"})
	p.Add(&auth.Auth{UID: "large"})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	at := time.Now().Add(time.Hour)
	p.SetCreditsDetailed("small", 10, 10, 10, at, 10)
	p.SetCreditsDetailed("large", 50, 50, 50, at, 50)

	got := p.Pick()
	if got == nil || got.UID != "large" {
		t.Fatalf("pick=%v want large", got)
	}
}

func TestPreferExpiringDisabledRestoresWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	now := time.Now()
	p.SetCreditsDetailed("a", 100, 100, 50, now.Add(time.Hour), 50)
	p.SetCreditsDetailed("b", 100, 100, 0, time.Time{}, 0)

	p.mu.Lock()
	we := p.routingWeightOf(p.byUID["a"], 100, now)
	wn := p.routingWeightOf(p.byUID["b"], 100, now)
	p.mu.Unlock()
	if we != wn*expiringVirtualSlots {
		t.Fatalf("enabled expiring weight=%v want %v", we, wn*expiringVirtualSlots)
	}

	p.SetPreferExpiring(false)

	p.mu.Lock()
	wa := p.routingWeightOf(p.byUID["a"], 100, now)
	wb := p.routingWeightOf(p.byUID["b"], 100, now)
	p.mu.Unlock()
	if wa != wb {
		t.Fatalf("disabled expiring weights differ: %v/%v", wa, wb)
	}
}

func TestCreditExpirySnapshotConsumptionAndClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.SetCreditsDetailed("u1", 100, 100, 50, now.Add(time.Hour), 40)

	st, _ := p.Status("u1")
	if st.CreditsExpiring != 50 || st.CreditsEarliestRemaining != 40 || st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("initial snapshot=%+v", st)
	}

	p.NoteModelCost("u1", "m", 10, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 90 || st.CreditsExpiring != 40 || st.CreditsEarliestRemaining != 30 {
		t.Fatalf("after consume=%+v", st)
	}

	p.NoteModelCost("u1", "m", 40, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 50 || st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("after exhaustion=%+v", st)
	}

	p.SetCreditsDetailed("u1", 50, 50, 10, now.Add(time.Hour), 10)
	p.SetCreditsDetailed("u1", 50, 50, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero refresh did not clear snapshot=%+v", st)
	}
}

// TestExpiryBucketsFEFOAndPersistence 钉住按日到期批次的三件事：
//  1. 最早批次以 buckets 为准（同日多包合计已在写入前完成），而不是入参单批值；
//  2. 两次余额刷新之间按 FEFO 扣减，扣穿第一桶后最早批次顺延到下一桶；
//  3. state.json / Redis 快照往返不丢批次，重启后首页快照仍与「检查」同口径。
func TestExpiryBucketsFEFOAndPersistence(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	at1 := now.Add(24 * time.Hour)
	at2 := now.Add(48 * time.Hour)
	p.SetCreditsSnapshot("u1", CreditsSnapshot{
		Remain: 300, Total: 300, Expiring: 100,
		// 故意给一个与 buckets 不一致的最早值：buckets 必须优先。
		EarliestAt: at1, EarliestRemaining: 999,
		ExpiryBuckets: []ExpiryBucket{
			{Date: at1.Format("2006-01-02"), ExpireAt: at1, Remain: 100},
			{Date: at2.Format("2006-01-02"), ExpireAt: at2, Remain: 200},
		},
	})
	st, _ := p.Status("u1")
	if len(st.CreditsExpiryBuckets) != 2 || st.CreditsEarliestRemaining != 100 || !st.CreditsEarliestExpiry.Equal(at1) {
		t.Fatalf("initial buckets=%+v earliest=%v/%d", st.CreditsExpiryBuckets, st.CreditsEarliestExpiry, st.CreditsEarliestRemaining)
	}

	// 扣 150：第一桶扣穿（100），第二桶剩 150，最早批次顺延到 at2。
	p.NoteModelCost("u1", "m", 150, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 150 || len(st.CreditsExpiryBuckets) != 1 || st.CreditsExpiryBuckets[0].Remain != 150 {
		t.Fatalf("after consume buckets=%+v credits=%d", st.CreditsExpiryBuckets, st.Credits)
	}
	if !st.CreditsEarliestExpiry.Equal(at2) || st.CreditsEarliestRemaining != 150 {
		t.Fatalf("earliest after consume=%v/%d want %v/150", st.CreditsEarliestExpiry, st.CreditsEarliestRemaining, at2)
	}

	// 持久化往返。
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	if got := sf.Accounts["u1"].CreditsExpiryBuckets; len(got) != 1 || got[0].Remain != 150 {
		t.Fatalf("persisted buckets=%+v", got)
	}
	p2 := New("")
	p2.mu.Lock()
	p2.applyAccountsLocked(sf.Accounts)
	p2.mu.Unlock()
	st2, ok := p2.Status("u1")
	if !ok || len(st2.CreditsExpiryBuckets) != 1 || st2.CreditsExpiryBuckets[0].Remain != 150 {
		t.Fatalf("restored buckets=%+v", st2.CreditsExpiryBuckets)
	}
	if !st2.CreditsEarliestExpiry.Equal(at2) || st2.CreditsEarliestRemaining != 150 {
		t.Fatalf("restored earliest=%v/%d", st2.CreditsEarliestExpiry, st2.CreditsEarliestRemaining)
	}
}

// ---------- 暂停选号（paused）----------
//
// paused 与 disabled 正交：两者都退出选号候选，但 paused 是「临时让位」——
// 不清冷却域、不写 reason，且**保号任务照常参与**（见 scheduler 侧测试）。

// TestPauseExitsRouting 暂停号退出选号（Pick 含全冷却兜底都不参与）。
func TestPauseExitsRouting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Pause("u1") {
		t.Fatal("Pause 对存在的账号应返回 true")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("暂停号不应被选中, got %+v", got)
	}
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Errorf("Status.Paused 应为 true: %+v", st)
	}
	if st.Disabled {
		t.Errorf("暂停不是禁用：Disabled 必须为 false: %+v", st)
	}
}

// TestPauseExcludesFromFallback 全冷却兜底同样不捞回暂停号（与 disabled 同口径）。
func TestPauseExcludesFromFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited") // 迫使命中兜底路径
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil（暂停号不入兜底）, got %+v", got)
	}
}

// TestResumeRestoresRouting 解除暂停后立刻回到池子（无需重登或解冻）。
func TestResumeRestoresRouting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	if !p.Resume("u1") {
		t.Fatal("Resume 对存在的账号应返回 true")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("Resume 后 Paused 应为 false: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Resume 后应可选, got %+v", got)
	}
}

// TestPauseDoesNotTouchCoolingDomain 暂停**不**清冷却域——与 disable 的关键区别：
// disable 是终态故清冷却，pause 是临时态故保留观测（恢复后仍有效）。
func TestPauseDoesNotTouchCoolingDomain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited")
	p.Pause("u1")
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "429 rate limited" {
		t.Errorf("Pause 不应清冷却/原因: %+v", st)
	}
}

// TestDisableClearsPaused 禁用是比暂停更强的终态：二者不叠加。
func TestDisableClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if st.Paused {
		t.Errorf("disable 后 paused 应被清（不叠加）: %+v", st)
	}
	if !st.Disabled {
		t.Errorf("应为 disabled: %+v", st)
	}
}

// TestReviveClearsPaused 运维「解冻」是全清：paused 一并解除。
func TestReviveClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("Revive 应清 paused: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Revive 后应可选, got %+v", got)
	}
}

// TestPausePersists 暂停状态落盘，跨重启不丢（轮换用法要求）。
func TestPausePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); !st.Paused {
		t.Errorf("重载后 Paused 应保持: %+v", st)
	}
	if got := p2.Pick(); got != nil {
		t.Fatalf("重载后暂停号仍不应可选, got %+v", got)
	}
}

// TestPauseUnknownUIDReturnsFalse 不存在的 uid 返回 false（供面板区分 404）。
func TestPauseUnknownUIDReturnsFalse(t *testing.T) {
	p := New("")
	if p.Pause("nope") {
		t.Error("Pause 未知 uid 应返回 false")
	}
	if p.Resume("nope") {
		t.Error("Resume 未知 uid 应返回 false")
	}
}

// TestResumeIdempotent 对未暂停账号 Resume 是空操作（幂等，不改变状态）。
func TestResumeIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Resume("u1") {
		t.Fatal("Resume 幂等应返回 true")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("Paused 应仍为 false: %+v", st)
	}
}
