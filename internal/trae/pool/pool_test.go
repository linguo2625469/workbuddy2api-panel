package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
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

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "plan limit" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
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

func TestSyncToDirRemovesMissing(t *testing.T) {
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

func TestRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Remove("u1") {
		t.Fatal("remove existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("u1 should be gone after remove")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 status should not exist")
	}
	if p.Remove("u1") {
		t.Fatal("remove missing should return false")
	}
}

// TestRemoveClearsStateEntry Remove 后 state.json 不再含该条目；
// 模拟「auths 文件也已删」场景：reload（不再 Add 该 uid）后池中无该账号。
func TestRemoveClearsStateEntry(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100) // 触发 saveLocked 落盘
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 50)
	p.Remove("u1")
	// reload：不再 Add u1（等价于 auths/trae-u1.json 已被 API 层删除）
	p2 := New(fp)
	if _, ok := p2.Status("u1"); ok {
		t.Fatal("u1 should not be in state after remove + reload")
	}
	if _, ok := p2.Status("u2"); !ok {
		t.Fatal("u2 should remain in state")
	}
	// u2 从 state reload，credits 应保留
	st, _ := p2.Status("u2")
	if st.Credits != 50 {
		t.Errorf("u2 credits lost on reload: %+v", st)
	}
}

func TestSetEnabledSoftSwitch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	if !p.SetEnabled("u1", false, "user paused") {
		t.Fatal("set enabled on existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("disabled-by-user account should be skipped by Pick")
	}
	st, _ := p.Status("u1")
	if st.Enabled {
		t.Errorf("Enabled flag should be false, got %+v", st)
	}
	if st.Disabled {
		t.Errorf("soft switch must not set Disabled (session dead), got %+v", st)
	}
	if st.Reason != "user paused" {
		t.Errorf("reason should be recorded, got %+v", st)
	}
	// 重新启用
	p.SetEnabled("u1", true, "")
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("re-enabled account should be picked, got %+v", got)
	}
	st2, _ := p.Status("u1")
	if !st2.Enabled {
		t.Errorf("Enabled should be true after re-enable, got %+v", st2)
	}
}

func TestSetEnabledDoesNotAffectDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	// session-dead 硬禁用时切软开关不应当作恢复
	p.SetEnabled("u1", true, "")
	if p.Pick() != nil {
		t.Fatal("disabled (session dead) must stay un-pickable even if enabled=true")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("Disabled should remain true, got %+v", st)
	}
}

func TestSetEnabledOnMissing(t *testing.T) {
	p := New("")
	if p.SetEnabled("nope", false, "x") {
		t.Fatal("set enabled on missing should return false")
	}
}

func TestSetEnabledPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetEnabled("u1", false, "paused")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("soft-disabled account should stay skipped after reload")
	}
	st, _ := p2.Status("u1")
	if st.Enabled || st.Reason != "paused" {
		t.Errorf("soft switch state lost: %+v", st)
	}
}

// TestStateFileBackwardCompat 旧 state.json 无 enabled 字段时，reload 默认按启用处理。
func TestStateFileBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手写一份无 enabled 字段的旧格式
	old := `{"accounts":{"u1":{"credits":100,"disabled":false,"reason":""}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 should load from old state")
	}
	if !st.Enabled {
		t.Errorf("missing enabled field should default to true, got %+v", st)
	}
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("old-format account should be pickable, got %+v", got)
	}
}
