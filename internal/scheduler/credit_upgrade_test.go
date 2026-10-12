package scheduler

// T4 升级场景集成测试（调度器侧）：并发全量余额刷新下的积分留痕一致性。
//
// RunBalanceRefreshNow 对每个账号各起一个 goroutine，观察者在各自调用栈上同步回调；
// 账本必须做到「流水条数 = 各账号真实变化次数之和」，且重启读盘与内存一致。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// upgBalanceFake 按 accessToken 识别账号，逐号返回可变的个人版余额。
type upgBalanceFake struct {
	mu     sync.Mutex
	tokens map[string]string // accessToken -> uid
	remain map[string]int64
	calls  map[string]int
	srv    *httptest.Server
}

func upgNewBalanceFake(t *testing.T) *upgBalanceFake {
	t.Helper()
	f := &upgBalanceFake{
		tokens: map[string]string{},
		remain: map[string]int64{},
		calls:  map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		uid := f.tokens[tok]
		f.calls[uid]++
		remain := f.remain[uid]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			http.Error(w, "not found", 404)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":%d,"CycleCapacityRemain":%d,"CycleCapacityUsed":0}]}}}}`, int64(1)<<40, remain)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *upgBalanceFake) add(uid string, remain int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens["at-"+uid] = uid
	f.remain[uid] = remain
}

func (f *upgBalanceFake) set(uid string, remain int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remain[uid] = remain
}

func (f *upgBalanceFake) callCount(uid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[uid]
}

// TestUpgConcurrentFullRefreshLedgerConsistent 覆盖 H：
// 多账号并发全量刷新 → 流水条数 = 变化账号数之和；重启读盘与内存一致。
func TestUpgConcurrentFullRefreshLedgerConsistent(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "data", "credit-history.json")
	fake := upgNewBalanceFake(t)

	const total = 8
	const changed = 5
	uids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		uids = append(uids, fmt.Sprintf("u%02d", i))
	}

	p := pool.New(filepath.Join(dir, "data", "state.json"))
	t.Cleanup(p.Close)
	for _, uid := range uids {
		p.Add(&auth.Auth{
			UID:          uid,
			AccessToken:  "at-" + uid,
			RefreshToken: "rt-" + uid,
			ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
		})
		fake.add(uid, 1000)
	}

	ledger := credithist.New(ledgerPath, 2000)
	up := &upstream.Client{HTTP: fake.srv.Client(), ChatBaseCN: fake.srv.URL, BillingBaseCN: fake.srv.URL}
	up.SetCreditObserver(ledger.Observe)
	s := New(Config{Pool: p, Upstream: up})

	// 首轮：全部建立基线，0 条流水
	s.RunBalanceRefreshNow()
	if got := len(ledger.Read(0)); got != 0 {
		t.Fatalf("首轮只应建基线，得到 %d 条流水", got)
	}
	for _, uid := range uids {
		st, ok := p.Status(uid)
		if !ok || st.Credits != 1000 {
			t.Fatalf("首轮后 %s credits=%d ok=%v want 1000", uid, st.Credits, ok)
		}
		if fake.callCount(uid) != 1 {
			t.Fatalf("%s 首轮应查一次余额，实际 %d 次", uid, fake.callCount(uid))
		}
	}

	// 第二轮：前 5 个变化，其余不变 → 恰好 5 条
	for i := 0; i < changed; i++ {
		fake.set(uids[i], 1100)
	}
	s.RunBalanceRefreshNow()

	entries := ledger.Read(0)
	if len(entries) != changed {
		t.Fatalf("流水条数应等于真实变化次数之和 %d，得到 %d: %+v", changed, len(entries), entries)
	}
	seen := map[string]bool{}
	for _, en := range entries {
		if en.Before != 1000 || en.After != 1100 || en.Delta != 100 {
			t.Fatalf("流水字段不符: %+v", en)
		}
		if seen[en.UID] {
			t.Fatalf("%s 重复留痕: %+v", en.UID, entries)
		}
		seen[en.UID] = true
	}
	for i := 0; i < changed; i++ {
		if !seen[uids[i]] {
			t.Fatalf("变化的账号 %s 未留痕: %+v", uids[i], entries)
		}
		if st, _ := p.Status(uids[i]); st.Credits != 1100 {
			t.Fatalf("%s credits=%d want 1100", uids[i], st.Credits)
		}
	}
	for i := changed; i < total; i++ {
		if st, _ := p.Status(uids[i]); st.Credits != 1000 {
			t.Fatalf("未变化账号 %s credits=%d want 1000", uids[i], st.Credits)
		}
	}

	// 第三轮：同值 → 不新增
	s.RunBalanceRefreshNow()
	if got := len(ledger.Read(0)); got != changed {
		t.Fatalf("同值轮不应新增流水，want %d got %d", changed, got)
	}

	// 重启读盘：与内存一致（跨账号顺序非确定，按条目键比较）
	l2 := credithist.New(ledgerPath, 2000)
	disk := l2.Read(0)
	if len(disk) != len(entries) {
		t.Fatalf("重启读盘条数 %d != 内存 %d", len(disk), len(entries))
	}
	key := func(e credithist.Entry) string {
		return fmt.Sprintf("%s|%d|%d|%d", e.UID, e.Delta, e.Before, e.After)
	}
	diskSet := map[string]int{}
	for _, e := range disk {
		diskSet[key(e)]++
	}
	for _, e := range entries {
		if diskSet[key(e)] != 1 {
			t.Fatalf("重启读盘与内存不一致，缺 %s（磁盘=%+v）", key(e), disk)
		}
	}
	// 磁盘快照：8 个账号齐全，变化账号为新值、未变化账号保持旧值
	rawFile, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var diskFile struct {
		Snapshot map[string]int64 `json:"snapshot"`
	}
	if err := json.Unmarshal(rawFile, &diskFile); err != nil {
		t.Fatalf("credit-history.json 不可解析: %v", err)
	}
	if len(diskFile.Snapshot) != total {
		t.Fatalf("磁盘快照应含 %d 个账号，得到 %d: %+v", total, len(diskFile.Snapshot), diskFile.Snapshot)
	}
	for i, uid := range uids {
		want := int64(1000)
		if i < changed {
			want = 1100
		}
		if diskFile.Snapshot[uid] != want {
			t.Fatalf("磁盘快照 %s=%d want %d", uid, diskFile.Snapshot[uid], want)
		}
	}
}
