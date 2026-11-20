package pool

import (
	"reflect"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// realmPool 构造一个含 cn/global 账号的池，并确保 globalEnabled 开关开启（缺省）。
func realmPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	// CN 账号（显式 cn realm 或空 realm+cn domain 均可）→ Realm()=="cn"
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn
	// global 账号 → Realm()=="global"
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})
	return p
}

// realmOf 直接读账号 realm（等价 e.a.Realm()）。
func realmOf(a *auth.Auth) string { return a.Realm() }

func TestAvailableUIDsForRealm(t *testing.T) {
	p := realmPool(t)

	// 全部 healthy → cn 集合只有 cn1/cn2，global 集合只有 g1/g2。
	got := p.AvailableUIDsForRealm("cn")
	want := []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}
	got = p.AvailableUIDsForRealm("global")
	want = []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}

	// realm=="" 退化为现状（等价 AvailableUIDs：全部 healthy）。
	got = p.AvailableUIDsForRealm("")
	want = []string{"cn1", "cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm()=%v want %v", got, want)
	}
}

func TestAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	// 6004 模型冷却只落在 cn-1 上 → AvailableUIDsForModelRealm("glm-5.2","cn") 排除它，
	// 但 cn 集合至少还保底 cn-2；global 集合不受 cn 冷却影响。
	p.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	got := p.AvailableUIDsForModelRealm("glm-5.2", "cn")
	if len(got) != 1 || got[0] != "cn2" {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,cn)=%v want [cn2]", got)
	}
	got = p.AvailableUIDsForModelRealm("glm-5.2", "global")
	want := []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,global)=%v want %v", got, want)
	}
	// realm=="" → 等价 AvailableUIDsForModel（模型豁免照常：cn1 仍被该模型冷却排除）。
	got = p.AvailableUIDsForModelRealm("glm-5.2", "")
	want = []string{"cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2)=%v want %v", got, want)
	}
}

func TestWeightedAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	now := time.Now()
	p.SetCreditsDetailed("cn2", 100, 100, 100, now.Add(time.Hour), 100)

	got := p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want := []string{"cn1", "cn2", "cn2", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted cn candidates=%v want %v", got, want)
	}

	// 在途满载账号不进入候选，快过期权重不得突破现有并发上限。
	p.SetMaxInFlight(1)
	if !p.Acquire("cn2") {
		t.Fatal("failed to acquire cn2")
	}
	got = p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want = []string{"cn1"}
	p.Release("cn2")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight-full weighted candidates=%v want %v", got, want)
	}

	// 关闭总开关后逐账号只出现一次，旧行为完全恢复。
	p.SetPreferExpiring(false)
	got = p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want = []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled weighted cn candidates=%v want %v", got, want)
	}
}

// TestWeightedAvailableUIDsProportionalSlots 快过期虚拟实例数按剩余量成比例：
// 剩余最多者取满 expiringVirtualSlots 次、次之按比例（1+2*0.6 四舍五入 = 2）、
// 无快过期者 1 次——不再是「有快过期就都 3 次」。
func TestWeightedAvailableUIDsProportionalSlots(t *testing.T) {
	p := realmPool(t)
	now := time.Now()
	at := now.Add(time.Hour)
	// 仅 cn 域两号设快过期，剩余量 100:60。
	p.SetCreditsDetailed("cn1", 100, 100, 100, at, 100)
	p.SetCreditsDetailed("cn2", 100, 100, 60, at, 60)

	got := p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want := []string{"cn1", "cn1", "cn1", "cn2", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proportional slots=%v want %v", got, want)
	}
}

func TestPickExcludingForRealm(t *testing.T) {
	p := realmPool(t)
	// realm=cn → 只从 cn 集合选。
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "cn")
		if a == nil {
			t.Fatal("PickExcludingForRealm(cn) returned nil")
		}
		seen[a.UID] = true
		if realmOf(a) != "cn" {
			t.Fatalf("PickExcludingForRealm(cn) returned global account %s", a.UID)
		}
	}
	// global 同样只从 global 集合。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "global")
		if a == nil {
			t.Fatal("PickExcludingForRealm(global) returned nil")
		}
		if realmOf(a) != "global" {
			t.Fatalf("PickExcludingForRealm(global) returned cn account %s", a.UID)
		}
	}
	// realm=="" → 退化现状：所有 healthy 都能选。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "")
		if a == nil {
			t.Fatal("PickExcludingForRealm() returned nil")
		}
	}
}

func TestPickExcludingForRealmTried(t *testing.T) {
	p := realmPool(t)
	// tried 排除在 realm 过滤之后（维持请求级轮换语义）。
	a := p.PickExcludingForRealm(map[string]bool{"cn1": true}, "", "cn")
	if a == nil {
		t.Fatal("tried cn1 → nil")
	}
	if a.UID == "cn1" {
		t.Fatalf("tried cn1 still picked: %s", a.UID)
	}
	if realmOf(a) != "cn" {
		t.Fatalf("picked non-cn %s", a.UID)
	}
}
