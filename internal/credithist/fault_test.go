package credithist

// T5 故障注入与边界测试（账本层）。只新增测试，不改业务代码。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// ── 1. 落盘目标不可写 ──────────────────────────────────────────────────────

// 目标路径任何一段不可写（这里用「父路径是普通文件」构造，Windows / POSIX 都成立）
// 时，Observe 只能记日志：不得 panic、不得影响内存账本；故障排除后必须恢复落盘。
func TestFaultUnwritableTargetKeepsMemoryAndRecovers(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "credit-history.json") // 父路径是普通文件 → MkdirAll 与写盘必失败

	l := New(path, 100)
	l.Observe("u1", 100) // 基线：落盘失败
	l.Observe("u1", 250) // +150：落盘失败
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != 150 || got[0].After != 250 {
		t.Fatalf("落盘失败不应影响内存账本，得到 %+v", got)
	}
	if l.snapshot["u1"] != 250 {
		t.Fatalf("内存基线应为 250，得到 %d", l.snapshot["u1"])
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("父路径是普通文件时不应写出账本文件")
	}

	// 排除故障：删掉阻塞文件、建同名目录，再观察必须恢复落盘。
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	l.Observe("u1", 300) // +50
	l2 := New(path, 100)
	got = l2.Read(0)
	if len(got) != 2 || got[0].Delta != 50 || got[1].Delta != 150 {
		t.Fatalf("恢复可写后应落盘并保留两条流水，得到 %+v", got)
	}
	if l2.snapshot["u1"] != 300 {
		t.Fatalf("恢复后快照应为 300，得到 %d", l2.snapshot["u1"])
	}
}

// POSIX 目录只读路径。Windows 的 os.Chmod 只改文件只读属性，没有目录写保护语义，
// 故在 Windows 上跳过（不可写路径已由上面的父路径为文件用例覆盖）。
func TestFaultReadOnlyDirPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX 目录权限语义")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "credit-history.json")
	l := New(path, 100)
	l.Observe("u1", 10) // 正常落盘
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	l.Observe("u1", 20) // 写临时文件失败 → 只记日志
	if got := l.Read(0); len(got) != 1 || got[0].Delta != 10 {
		t.Fatalf("只读目录下内存账本仍应正确，得到 %+v", got)
	}
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	l.Observe("u1", 30)
	l2 := New(path, 100)
	if got := l2.Read(0); len(got) != 2 || got[1].Delta != 10 {
		t.Fatalf("恢复可写后应落盘两条，得到 %+v", got)
	}
}

// ── 2. 退化/损坏文件 ──────────────────────────────────────────────────────

// 损坏 JSON、null 快照/流水、空文件、类型不符、顶层数组、路径本身是目录——
// 一律从空账本安全启动，后续 Observe 正常，不得 panic。
func TestFaultDegenerateFilesStartEmpty(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"invalid_json", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty_file", func(t *testing.T, p string) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"null_snapshot_and_entries", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(`{"version":1,"snapshot":null,"entries":null}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"entries_null", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(`{"version":1,"snapshot":{"u1":5},"entries":null}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong_types", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(`{"version":1,"snapshot":"x","entries":42}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"top_level_array", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"path_is_directory", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credit-history.json")
			c.setup(t, path)
			l := New(path, 100) // 必须不 panic
			base := len(l.Read(0))
			l.Observe("fresh", 100) // 基线
			l.Observe("fresh", 130) // +30
			got := l.Read(0)
			if len(got) != base+1 {
				t.Fatalf("退化输入后应能正常观测：base=%d 期望 %d 条，得到 %d（%+v）", base, base+1, len(got), got)
			}
			if got[0].UID != "fresh" || got[0].Delta != 30 || got[0].Before != 100 || got[0].After != 130 {
				t.Fatalf("新增流水字段不符: %+v", got[0])
			}
		})
	}
}

// ── 3. maxEntries 回落与截断 ──────────────────────────────────────────────

// maxEntries <= 0 回落默认上限，不产生无界增长；=1 只留最新一条。
func TestFaultMaxEntriesFallbackAndTrim(t *testing.T) {
	for _, m := range []int{0, -1, -1000} {
		l := New("", m)
		if l.max != defaultMaxEntries {
			t.Fatalf("maxEntries=%d 应回落默认 %d，得到 %d", m, defaultMaxEntries, l.max)
		}
	}

	// 不无界增长：写 defaultMaxEntries+300 次，条数必须恰为默认上限。
	l := New("", 0)
	l.Observe("u1", 0)
	for i := int64(1); i <= int64(defaultMaxEntries)+300; i++ {
		l.Observe("u1", i)
	}
	got := l.Read(0)
	if len(got) != defaultMaxEntries {
		t.Fatalf("默认上限 %d 应截断，得到 %d", defaultMaxEntries, len(got))
	}
	if got[0].After != int64(defaultMaxEntries)+300 {
		t.Fatalf("截断后最新一条应保留，得到 %+v", got[0])
	}

	// maxEntries=1：只留最新一条，且丢的是最旧。
	l1 := New("", 1)
	l1.Observe("u1", 0)
	l1.Observe("u1", 10)
	l1.Observe("u1", 20)
	one := l1.Read(0)
	if len(one) != 1 || one[0].After != 20 || one[0].Delta != 10 {
		t.Fatalf("上限 1 应只留最新一条，得到 %+v", one)
	}
}

// ── 4. Read 边界 ──────────────────────────────────────────────────────────

// limit<=0 = 全部；大于总数 = 全部；1 = 最新；顺序严格新的在前。
func TestFaultReadBoundaries(t *testing.T) {
	l := New("", 100)
	l.Observe("u1", 0)
	for i := int64(1); i <= 3; i++ {
		l.Observe("u1", i*10)
	}
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero_is_all", 0, 3},
		{"negative_is_all", -7, 3},
		{"over_total_is_all", 99, 3},
		{"exact_total", 3, 3},
		{"two", 2, 2},
		{"one", 1, 1},
	}
	for _, c := range cases {
		if got := l.Read(c.limit); len(got) != c.want {
			t.Fatalf("%s: Read(%d) 得到 %d 条，期望 %d", c.name, c.limit, len(got), c.want)
		}
	}
	got := l.Read(0)
	if got[0].After != 30 || got[1].After != 20 || got[2].After != 10 {
		t.Fatalf("必须严格新的在前: %+v", got)
	}
	got = l.Read(1)
	if len(got) != 1 || got[0].After != 30 {
		t.Fatalf("Read(1) 应取最新一条: %+v", got)
	}
}

// ── 5. Δ 边界 ────────────────────────────────────────────────────────────

// 恰好 ±maxDelta 记账；±(maxDelta+1) 只更新基线、不留痕，且基线必须推进到新值。
func TestFaultDeltaBoundaries(t *testing.T) {
	// +++ 恰好 +1000000 记账
	l := New("", 100)
	l.Observe("u1", 0)
	l.Observe("u1", maxDelta)
	if got := l.Read(0); len(got) != 1 || got[0].Delta != maxDelta || got[0].Before != 0 || got[0].After != maxDelta {
		t.Fatalf("恰好 +%d 应记账: %+v", maxDelta, got)
	}
	// --- 恰好 -1000000 记账
	l = New("", 100)
	l.Observe("u1", maxDelta)
	l.Observe("u1", 0)
	if got := l.Read(0); len(got) != 1 || got[0].Delta != -maxDelta {
		t.Fatalf("恰好 -%d 应记账: %+v", maxDelta, got)
	}
	// +++ 超限 +1000001 不留痕，但基线推进
	l = New("", 100)
	l.Observe("u1", 0)
	l.Observe("u1", maxDelta+1)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("超限 +%d 不应留痕，得到 %d 条", maxDelta+1, got)
	}
	if l.snapshot["u1"] != maxDelta+1 {
		t.Fatalf("超限后基线应推进到 %d，得到 %d", maxDelta+1, l.snapshot["u1"])
	}
	l.Observe("u1", 1) // Δ = 1-(maxDelta+1) = -maxDelta → 正常记账
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != -maxDelta || got[0].Before != maxDelta+1 || got[0].After != 1 {
		t.Fatalf("超限后应按新基线记账: %+v", got)
	}
	// --- 超限 -1000001 不留痕，基线推进
	l = New("", 100)
	l.Observe("u1", 0)
	l.Observe("u1", -(maxDelta + 1))
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("超限 -%d 不应留痕，得到 %d 条", maxDelta+1, got)
	}
	if l.snapshot["u1"] != -(maxDelta + 1) {
		t.Fatalf("超限后基线应推进到 %d，得到 %d", -(maxDelta + 1), l.snapshot["u1"])
	}
	l.Observe("u1", -1) // Δ = -1-(-(maxDelta+1)) = +maxDelta
	got = l.Read(0)
	if len(got) != 1 || got[0].Delta != maxDelta || got[0].Before != -(maxDelta+1) {
		t.Fatalf("超限后应按新基线记账: %+v", got)
	}
}

// ── 6. uid 边界 ───────────────────────────────────────────────────────────

// 空 uid（上游异常条目）不建基线、不记账。
func TestFaultEmptyUIDIgnored(t *testing.T) {
	l := New("", 100)
	l.Observe("", 100)
	l.Observe("", 200)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("空 uid 不应记账，得到 %d 条", got)
	}
	if len(l.snapshot) != 0 {
		t.Fatalf("空 uid 不应建基线，得到 %v", l.snapshot)
	}
}

// 任务描述要求纯空白 uid（上游脏数据）同样不建基线。当前实现只判 uid == ""，
// 纯空白会进快照——此用例作为复现保留，等 lead 裁决。
func TestFaultWhitespaceUIDNotBaselined(t *testing.T) {
	l := New("", 100)
	l.Observe("   ", 100)
	l.Observe("\t\n", 100)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("纯空白 uid 不应记账，得到 %d 条", got)
	}
	if len(l.snapshot) != 0 {
		t.Fatalf("纯空白 uid 不应建基线，但快照里有 %d 个键: %v", len(l.snapshot), l.snapshot)
	}
}

// ── 7. 并发 ──────────────────────────────────────────────────────────────

// 多 goroutine（不同 uid 定量 + 同一 uid 混跑）不 panic、不同 uid 条数不丢、
// 磁盘与内存逐条一致；同一 uid 并发后再顺序写一个唯一哨兵，必须是最终基线。
func TestFaultConcurrentMixedUIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-history.json")
	l := New(path, 4000)
	const accounts = 6
	const rounds = 30
	for i := 0; i < accounts; i++ {
		l.Observe(fmt.Sprintf("acct-%d", i), 0)
	}
	l.Observe("shared", 0)

	var wg sync.WaitGroup
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); n <= rounds; n++ {
				l.Observe(uid, n*10)
			}
		}()
	}
	for w := 0; w < 8; w++ {
		base := int64(w) * 1000
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); n <= 20; n++ {
				l.Observe("shared", base+n)
			}
		}()
	}
	wg.Wait()

	// 顺序写入唯一哨兵 = 「最后一次写入」，并发结束后必须生效。
	l.Observe("shared", 777777)
	if l.snapshot["shared"] != 777777 {
		t.Fatalf("最后写入应成为基线，得到 %d", l.snapshot["shared"])
	}

	mem := l.Read(0)
	per := map[string]int{}
	for _, e := range mem {
		per[e.UID]++
	}
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		if per[uid] != rounds {
			t.Fatalf("%s 应有 %d 条流水，得到 %d", uid, rounds, per[uid])
		}
	}

	// 磁盘必须与内存逐条一致（慢写覆盖会让磁盘少条/快照回退）。
	l2 := New(path, 4000)
	disk := l2.Read(0)
	if len(disk) != len(mem) {
		t.Fatalf("重启后条数不一致：内存 %d / 磁盘 %d", len(mem), len(disk))
	}
	for i := range mem {
		m, d := mem[i], disk[i]
		// 不能直接比较 struct：内存里的 time.Time 带单调时钟读数，落盘再读回后没有。
		if m.UID != d.UID || m.Delta != d.Delta || m.Before != d.Before || m.After != d.After || !m.Time.Equal(d.Time) {
			t.Fatalf("第 %d 条内存/磁盘不一致: 内存 %+v / 磁盘 %+v", i, m, d)
		}
	}
	if l2.snapshot["shared"] != 777777 {
		t.Fatalf("重启后 shared 基线应为 777777，得到 %d", l2.snapshot["shared"])
	}
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		if l2.snapshot[uid] != rounds*10 {
			t.Fatalf("重启后 %s 基线应为 %d，得到 %d", uid, rounds*10, l2.snapshot[uid])
		}
	}
}

// ── 8. 临时文件残留与文件权限 ──────────────────────────────────────────────

// 正常落盘（临时文件 + rename）不得在目录里留下 .tmp 残留。
func TestFaultNoTempResidueAfterWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-history.json")
	l := New(path, 100)
	l.Observe("u1", 1)
	l.Observe("u1", 2)
	l.Observe("u1", 3)
	left, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("正常落盘不应残留临时文件，发现 %v", left)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应有账本文件: %v", err)
	}
}

// 账本文件权限不得宽于 0600。Windows 的 os.WriteFile 不套用 POSIX 权限位，
// Stat().Mode().Perm() 恒为 0666，故该断言只在 POSIX 生效。
func TestFaultFileModeNotWiderThan0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不应用 POSIX 权限位（Perm() 恒为 0666）；该断言只在 POSIX 生效")
	}
	path := filepath.Join(t.TempDir(), "credit-history.json")
	l := New(path, 100)
	l.Observe("u1", 1)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("账本文件权限不应宽于 0600，得到 %04o", perm)
	}
}
