// Package credithist 积分历史：把「每次真实查到的余额」与上一次比对，变动即留痕。
//
// 为什么需要：签到 / 活跃上报 / 猫猫旅行 / 成长任务都会让余额增加，但上游只在少数
// 渠道留下可读金额；面板「用量」折算的是**消耗**，此前没有任何逐账号的余额时间线。
// 把余额当观测量逐次比对，就能覆盖全部获取渠道（做法与 workbuddy-manager 的
// 「积分变动流水」一致）。
//
// 与参考实现的有意差异：参考实现只记增加（减少属"消耗"，已有用量视图覆盖）。本
// 实现两个方向都留痕（Delta 带符号），因为本面板缺的正是"什么时候变成多少"这条
// 时间线；展示层用正负号区分获取与消耗。
//
// 首次见到某个 uid 只建立基线、不记流水——否则会把历史余额误报成"刚获得"。
package credithist

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxDelta 单次变动的留痕上限（对齐参考实现的 _MAX_DELTA）：超过它基本可断定是
// 上游脏数据或口径切换（如企业版"不限量"哨兵值），只更新基线、不留痕，避免一条
// 假流水污染整段历史。
const maxDelta = 1_000_000

// defaultMaxEntries 流水条数上限（超出丢最旧）：按每日数十条估计可留数月。
const defaultMaxEntries = 2000

// Entry 一条积分变动流水。Before/After 是变动前后余额，Delta = After - Before
// （正 = 获取，负 = 消耗）。
type Entry struct {
	Time   time.Time `json:"time"`
	UID    string    `json:"uid"`
	Delta  int64     `json:"delta"`
	Before int64     `json:"before"`
	After  int64     `json:"after"`
}

// fileVersion 落盘格式版本（与 usage.json 同风格，便于将来迁移）。
const fileVersion = 1

// diskFile 落盘结构：余额快照（uid → 最近余额）与流水同文件——重启后基线不丢，
// 不会把"上次进程结束时的余额"当成新增。
type diskFile struct {
	Version  int              `json:"version"`
	Saved    string           `json:"saved,omitempty"`
	Snapshot map[string]int64 `json:"snapshot"`
	Entries  []Entry          `json:"entries"`
}

// Ledger 并发安全的积分账本。path 为空 = 纯内存（不落盘）。
type Ledger struct {
	mu       sync.Mutex
	path     string
	max      int
	snapshot map[string]int64
	entries  []Entry // 时间升序
}

// New 创建账本；path 非空且文件存在时加载既有快照与流水。加载失败只记日志、
// 从空账本开始——丢历史好过让面板起不来。maxEntries <= 0 回落默认上限。
func New(path string, maxEntries int) *Ledger {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	l := &Ledger{path: path, max: maxEntries, snapshot: map[string]int64{}}
	l.load()
	return l
}

// load 读回快照与流水（无文件 = 空账本，不是错误）。
func (l *Ledger) load() {
	if l.path == "" {
		return
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[credithist] 读取 %s 失败（从空账本开始）: %v", l.path, err)
		}
		return
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("[credithist] 解析 %s 失败（从空账本开始）: %v", l.path, err)
		return
	}
	if f.Snapshot != nil {
		l.snapshot = f.Snapshot
	}
	l.entries = f.Entries
	if len(l.entries) > l.max {
		l.entries = append([]Entry(nil), l.entries[len(l.entries)-l.max:]...)
	}
	log.Printf("[credithist] 已恢复 %d 条积分流水 / %d 个余额基线（%s）",
		len(l.entries), len(l.snapshot), l.path)
}

// Observe 记录一次真实查到的余额（uid + 余额绝对值）。
//
// 语义：
//   - 首次见到该 uid：只建基线，不记流水；
//   - 余额与基线相同：直接返回（不落盘）；
//   - 余额变化且 |Delta| <= maxDelta：追加一条流水；
//   - 余额变化但 |Delta| > maxDelta：只更新基线（脏数据不留痕）。
//
// 同步落盘：留痕频率是"每次余额刷新"级（分钟级），文件很小，无需防抖队列。
func (l *Ledger) Observe(uid string, credits int64) {
	// 纯空白 uid 与空 uid 一样无效：放进去只会留下一个面板永远筛不到的幽灵基线
	// （uid 过滤参数为空 = 不过滤，所以这种条目既无用也删不掉）。
	if l == nil || strings.TrimSpace(uid) == "" {
		return
	}
	l.mu.Lock()
	prev, seen := l.snapshot[uid]
	if seen && prev == credits {
		l.mu.Unlock()
		return
	}
	l.snapshot[uid] = credits
	if seen {
		if delta := credits - prev; delta >= -maxDelta && delta <= maxDelta {
			l.entries = append(l.entries, Entry{
				Time:   time.Now(),
				UID:    uid,
				Delta:  delta,
				Before: prev,
				After:  credits,
			})
			if len(l.entries) > l.max {
				// 丢最旧：copy 到新底层数组，避免 append 复用旧数组导致内存不释放。
				l.entries = append([]Entry(nil), l.entries[len(l.entries)-l.max:]...)
			}
		}
	}
	raw, err := json.Marshal(diskFile{
		Version:  fileVersion,
		Saved:    time.Now().Format(time.RFC3339),
		Snapshot: l.snapshot,
		Entries:  l.entries,
	})
	if err != nil {
		l.mu.Unlock()
		log.Printf("[credithist] 序列化失败: %v", err)
		return
	}
	// 落盘仍在锁内：多个账号并发刷新余额时，"快照+流水"必须按同一顺序写盘，
	// 否则慢的那次会把新的文件内容覆盖成旧的（内存正确、重启后少一条）。
	// 留痕频率是分钟级且仅在余额变化时触发，锁内小文件原子写无性能顾虑。
	l.write(raw)
	l.mu.Unlock()
}

// Read 返回最近 limit 条流水（新的在前）；limit <= 0 = 全部。
func (l *Ledger) Read(limit int) []Entry {
	if l == nil {
		return []Entry{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Entry, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// write 原子写盘（临时文件 + rename，与 usage.Recorder 同口径）。失败只记日志：
// 留痕是旁路观测，不能反过来影响余额刷新主流程。
func (l *Ledger) write(raw []byte) {
	if l.path == "" {
		return
	}
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[credithist] 建目录失败: %v", err)
			return
		}
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[credithist] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("[credithist] 原子替换失败: %v", err)
	}
}
