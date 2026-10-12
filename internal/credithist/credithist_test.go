package credithist

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 首次见到账号只建基线：把历史余额误报成「刚获得」是最刺眼的错误，
// 也是参考实现明确写下的一条约束。
func TestFirstObserveOnlyBaselines(t *testing.T) {
	l := New("", 100)
	l.Observe("u1", 1000)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("首次观测应只建基线，得到 %d 条流水", got)
	}
	// 基线生效：下一次同值不记、变化才记。
	l.Observe("u1", 1000)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("余额未变不应记流水，得到 %d 条", got)
	}
	l.Observe("u1", 1100)
	got := l.Read(0)
	if len(got) != 1 {
		t.Fatalf("余额 +100 应记 1 条，得到 %d 条", len(got))
	}
	if got[0].Delta != 100 || got[0].Before != 1000 || got[0].After != 1100 || got[0].UID != "u1" {
		t.Fatalf("流水字段不符: %+v", got[0])
	}
}

// 消耗（余额下降）同样留痕，Delta 为负：面板缺的是逐账号余额时间线，
// 只记增加会让"什么时候变少"永远无从查证。
func TestDecreaseIsRecorded(t *testing.T) {
	l := New("", 100)
	l.Observe("u1", 1000)
	l.Observe("u1", 940)
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != -60 {
		t.Fatalf("余额 -60 应记一条 delta=-60 的流水，得到 %+v", got)
	}
}

// |Delta| 超过 maxDelta 视为上游脏数据：只更新基线、不留痕（对齐参考实现
// _MAX_DELTA），且不得把基线留在旧值上导致下一轮继续误判。
func TestDirtyDeltaOnlyUpdatesBaseline(t *testing.T) {
	l := New("", 100)
	l.Observe("u1", 100)
	l.Observe("u1", 100+maxDelta+1) // 超过上限 → 不留痕
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("超限变动不应留痕，得到 %d 条", got)
	}
	// 基线已更新到新值：回落 100 应记一条 -1000001 之外的正常流水。
	l.Observe("u1", 200)
	got := l.Read(0)
	if len(got) != 1 || got[0].Before != 100+maxDelta+1 || got[0].After != 200 {
		t.Fatalf("基线未更新，得到 %+v", got)
	}
}

// maxEntries 截断：只保留最新的 N 条，丢的是最旧的。
func TestMaxEntriesTrimsOldest(t *testing.T) {
	l := New("", 3)
	l.Observe("u1", 0)
	for i := int64(1); i <= 5; i++ {
		l.Observe("u1", i*10)
	}
	got := l.Read(0)
	if len(got) != 3 {
		t.Fatalf("上限 3 应只保留 3 条，得到 %d 条", len(got))
	}
	// 新的在前：10→20→30→40→50 里应剩 30/40/50。
	if got[0].After != 50 || got[1].After != 40 || got[2].After != 30 {
		t.Fatalf("截断应保留最新 3 条，得到 %+v", got)
	}
}

// 落盘后重建实例仍能续接基线：快照与流水同文件，重启不会重复记基线、
// 也不会把重启当成一次变动。
func TestSnapshotSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-history.json")
	l := New(path, 100)
	l.Observe("u1", 1000) // 基线落盘
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Observe 后应已落盘: %v", err)
	}

	l2 := New(path, 100)
	l2.Observe("u1", 1000) // 同值：不记
	if got := len(l2.Read(0)); got != 0 {
		t.Fatalf("重启后同值不应记流水，得到 %d 条", got)
	}
	l2.Observe("u1", 1250)
	got := l2.Read(0)
	if len(got) != 1 || got[0].Delta != 250 || got[0].Before != 1000 {
		t.Fatalf("重启后应能续接基线并记 +250，得到 %+v", got)
	}
}

// Read 的倒序与 limit 语义：新的在前，limit<=0 = 全部。
func TestReadOrderAndLimit(t *testing.T) {
	l := New("", 100)
	l.Observe("u1", 0)
	l.Observe("u1", 10)
	l.Observe("u1", 20)
	l.Observe("u1", 30)
	all := l.Read(0)
	if len(all) != 3 || all[0].After != 30 || all[2].After != 10 {
		t.Fatalf("全量应新的在前: %+v", all)
	}
	one := l.Read(1)
	if len(one) != 1 || one[0].After != 30 {
		t.Fatalf("limit=1 应取最新一条: %+v", one)
	}
}

// 空 uid 不建基线：面板存在 uid 为空的异常条目，不能污染账本。
func TestEmptyUIDIgnored(t *testing.T) {
	l := New("", 100)
	l.Observe("", 100)
	l.Observe("", 200)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("空 uid 不应记账，得到 %d 条", got)
	}
}

// 损坏文件不能拖垮启动：从空账本开始，后续观测照常工作。
func TestCorruptFileFallsBackToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-history.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path, 100)
	l.Observe("u1", 500)
	l.Observe("u1", 600)
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != 100 {
		t.Fatalf("损坏文件后应能正常建基线并记账，得到 %+v", got)
	}
}

// 并发 Observe：调度器全量余额刷新是多账号并行的，观测不能丢记录，
// 落盘也不能被慢写覆盖（内存与磁盘最终必须一致）。
func TestConcurrentObserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-history.json")
	l := New(path, 500)
	const accounts = 8
	const rounds = 20
	for i := 0; i < accounts; i++ {
		l.Observe(fmt.Sprintf("u%d", i), 100) // 基线
	}
	var wg sync.WaitGroup
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("u%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); n <= rounds; n++ {
				l.Observe(uid, 100+n*10)
			}
		}()
	}
	wg.Wait()

	want := accounts * rounds
	if got := len(l.Read(0)); got != want {
		t.Fatalf("并发观测应产生 %d 条流水，得到 %d", want, got)
	}
	// 重启读取必须与内存一致：慢写覆盖会让磁盘少条、快照回退。
	l2 := New(path, 500)
	if got := len(l2.Read(0)); got != want {
		t.Fatalf("重启后应保留 %d 条，得到 %d（落盘被覆盖）", want, got)
	}
	for i := 0; i < accounts; i++ {
		if v := l2.snapshot[fmt.Sprintf("u%d", i)]; v != 300 {
			t.Fatalf("u%d 快照应为 300，得到 %d", i, v)
		}
	}
}
