// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model, rate) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// fileVersion 是 usage.json 的当前格式版本。版本 2 增加积分观测字段，版本 3
// 增加模型生效倍率分区，版本 4 增加前缀缓存命中/未命中累计；
// 旧版本缺失字段按零值加载，旧数据不会丢弃。
const fileVersion = 4

// bucket 一个 (时间片, realm, uid, model, rate) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"`            // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`            // cn / global
	UID   string  `json:"u"`            // 账号 uid
	Model string  `json:"m"`            // 上游裸模型名
	Rate  string  `json:"x,omitempty"`  // 请求时生效积分倍率（规范化数值；旧桶为空）
	Req   int64   `json:"q"`            // 请求数（含失败）
	Err   int64   `json:"e"`            // 失败数
	PT    int64   `json:"p"`            // prompt tokens
	CT    int64   `json:"c"`            // completion tokens
	TT    int64   `json:"t"`            // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`            // 延迟累计（ms）
	LatN  int64   `json:"ln"`           // 延迟样本数
	TPS   float64 `json:"v"`            // 吐字速率累计
	TPSN  int64   `json:"vn"`           // 速率样本数
	CR    float64 `json:"cr,omitempty"` // usage.credit 累计（仅明确存在的观测）
	CRN   int64   `json:"cn,omitempty"` // usage.credit 样本数（区分缺字段与真实 0）
	CRT   int64   `json:"ct,omitempty"` // 同时具备 credit 与 token 的 Token 合计
	CH    int64   `json:"ch,omitempty"` // 前缀缓存命中 token 累计（上游回该维度才累计）
	CM    int64   `json:"cm,omitempty"` // 前缀缓存未命中 token 累计
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model|rate
	dirty   bool
	started time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	Credit           float64
	HasCredit        bool
	ModelRate        string
	// CacheHitTokens / CacheMissTokens 前缀缓存命中/未命中观测（issue #92）。
	HasCacheTokens  bool
	CacheHitTokens  int64
	CacheMissTokens int64
	LatencyMs       int64
	HasLatency      bool
	TokensPerSecond float64
	HasTPS          bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + d.ModelRate

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: d.ModelRate}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasCredit {
		b.CR += d.Credit
		b.CRN++
		// 比例只使用同一次请求同时具备 credit 与 token 的样本，避免把
		// 仅 token 的旧记录或仅 credit 的观测混进分母。
		if d.HasTotal {
			b.CRT += d.TotalTokens
		} else if d.HasPromptTokens || d.HasCompletion {
			b.CRT += d.PromptTokens + d.CompletionTokens
		}
	}
	if d.HasCacheTokens {
		b.CH += d.CacheHitTokens
		b.CM += d.CacheMissTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
			dst.CR += src.CR
			dst.CRN += src.CRN
			dst.CRT += src.CRT
			dst.CH += src.CH
			dst.CM += src.CM
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Rate] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests           int64   `json:"requests"`
	Errors             int64   `json:"errors"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTok      int64   `json:"completion_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	AvgTPS             float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.Credits += b.CR
	g.CreditSamples += b.CRN
	g.CreditTokens += b.CRT
	g.CacheHitTokens += b.CH
	g.CacheMissTokens += b.CM
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	if g.CreditTokens > 0 {
		a.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}
	if total := g.CacheHitTokens + g.CacheMissTokens; total > 0 {
		a.CacheHitRate = float64(g.CacheHitTokens) / float64(total) * 100
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// CreditAgg 积分扣除统计的一行。Key 在账号维度是 UID，在模型维度是裸模型名；
// Rate 仅模型维度使用；比例分母只统计与 credit 同时存在的 Token 样本。
type CreditAgg struct {
	Key                string  `json:"key"`
	Realm              string  `json:"realm,omitempty"`
	Nickname           string  `json:"nickname,omitempty"`
	Rate               string  `json:"rate,omitempty"`
	Requests           int64   `json:"requests"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
}

type creditAcc struct {
	CreditAgg
}

func (a *creditAcc) add(b *bucket) {
	a.Requests += b.Req
	a.Credits += b.CR
	a.CreditSamples += b.CRN
	a.CreditTokens += b.CRT
	a.CacheHitTokens += b.CH
	a.CacheMissTokens += b.CM
}

func (a *creditAcc) finish() CreditAgg {
	out := a.CreditAgg
	if a.CreditTokens > 0 {
		out.CreditsPer1MTokens = a.Credits / float64(a.CreditTokens) * 1_000_000
	}
	if total := a.CacheHitTokens + a.CacheMissTokens; total > 0 {
		out.CacheHitRate = float64(a.CacheHitTokens) / float64(total) * 100
	}
	return out
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals          Agg         `json:"totals"`
	ByRealm         []KeyedAgg  `json:"by_realm"`
	ByAccount       []KeyedAgg  `json:"by_account"`
	ByModel         []KeyedAgg  `json:"by_model"`
	Series          []Point     `json:"series"`
	CreditByAccount []CreditAgg `json:"credit_by_account"`
	CreditByModel   []CreditAgg `json:"credit_by_model"`
	Buckets         int         `json:"buckets"`
	FileBytes       int64       `json:"file_bytes"`
	Since           string      `json:"since,omitempty"`
	// WindowFrom/WindowTo 本次实际生效的统计区间（本地时间，RFC3339），供面板
	// 回显口径——「自定义」区间下用户必须能确认服务端到底按哪段算的。
	// 空串 = 该侧不设界（全部历史 / 到今天为止）。
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo   string `json:"window_to,omitempty"`
	Generated  string `json:"generated"`
}

// Window 用量统计窗口。三种口径按优先级解析（见 bounds）：
//   - From/To 任一非零 → 显式区间 [From, To]（To 零值 = 不设上界）
//   - 否则 Hours>0     → 滚动窗口：当前整点往回 Hours-1 小时
//   - 否则             → 全部历史
//
// 为什么显式区间用「桶起点落在 [From, To] 内」判定而不是求交集：小时桶的粒度
// 就是一小时，用户选到 14:00 时把 14:00 这一小时的桶算进来符合直觉；同时这也
// 让 Hours 口径与历史行为逐位一致（原实现就是 ts.Before(from) 即跳过）。
type Window struct {
	Hours int
	From  time.Time
	To    time.Time
}

// bounds 解析出实际生效的 [from, to]；零值表示该侧不设界。
func (w Window) bounds() (time.Time, time.Time) {
	if !w.From.IsZero() || !w.To.IsZero() {
		return w.From, w.To
	}
	if w.Hours <= 0 {
		return time.Time{}, time.Time{}
	}
	h := w.Hours
	if h > 24*60 {
		h = 24 * 60
	}
	return time.Now().Truncate(time.Hour).Add(-time.Duration(h-1) * time.Hour), time.Time{}
}

// bucketTime 把桶 scope 解析成本地时间；脏 scope 返回 false（不进任何口径）。
func bucketTime(scope string) (time.Time, bool) {
	if strings.HasPrefix(scope, "h:") {
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
		return ts, err == nil
	}
	ts, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), time.Local)
	return ts, err == nil
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	return r.SnapshotWithRates(hours, nicks, nil)
}

// SnapshotWithRates 与 Snapshot 相同，但允许为缺少历史倍率的旧桶提供当前
// 模型倍率回填。currentRate 返回空串时该行按“未知倍率”聚合，不伪造价格。
func (r *Recorder) SnapshotWithRates(hours int, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.SnapshotWindow(Window{Hours: hours}, nicks, currentRate)
}

// SnapshotWindow 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// 窗口语义见 Window：滚动窗口（Hours）/ 显式区间（From-To）/ 全部历史。卡片汇总、
// 按域、按账号、按模型、时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化
// （曾长期是"卡片为全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没
// 生效"，已废弃）。小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，
// 故小时窗口天然不含更早的日桶。
func (r *Recorder) SnapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	from, to := w.bounds()
	explicit := !w.From.IsZero() || !w.To.IsZero()
	windowed := !from.IsZero() || !to.IsZero()

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}
	creditAcctAgg := map[string]*creditAcc{}
	creditModelAgg := map[string]*creditAcc{}
	rateCache := map[string]string{}

	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if b.Scope < since || since == "" {
			since = b.Scope
		}
		if windowed {
			ts, ok := bucketTime(b.Scope)
			// 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
			if !ok {
				continue
			}
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
		}
		matched++
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		if strings.HasPrefix(b.Scope, "h:") {
			scope := strings.TrimPrefix(b.Scope, "h:")
			if hourSeries[scope] == nil {
				hourSeries[scope] = &aggAcc{}
			}
			hourSeries[scope].add(b)
		} else {
			scope := strings.TrimPrefix(b.Scope, "d:")
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
		if b.CRN > 0 {
			ca := creditAcctAgg[b.UID]
			if ca == nil {
				ca = &creditAcc{CreditAgg: CreditAgg{
					Key:      b.UID,
					Realm:    b.Realm,
					Nickname: nicks[b.UID],
				}}
				creditAcctAgg[b.UID] = ca
			}
			ca.add(b)

			model := canonicalUsageModel(b.Model)
			rate := b.Rate
			if rate == "" && currentRate != nil {
				cacheKey := b.Realm + "\x00" + model
				if cached, ok := rateCache[cacheKey]; ok {
					rate = cached
				} else {
					rate = currentRate(b.Realm, model)
					rateCache[cacheKey] = rate
				}
			}
			modelKey := model + "\x00" + rate
			cm := creditModelAgg[modelKey]
			if cm == nil {
				cm = &creditAcc{CreditAgg: CreditAgg{Key: model, Rate: rate}}
				creditModelAgg[modelKey] = cm
			}
			cm.add(b)
		}
	}

	snap := Snapshot{
		Totals: total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) {
			return k, ""
		}),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:         keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		CreditByAccount: creditKeyed(creditAcctAgg),
		CreditByModel:   creditKeyed(creditModelAgg),
		Buckets:         matched,
		Generated:       time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-09-16T13" → "2026-09-16T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	// 回显实际生效的区间：**仅显式区间口径**。滚动窗口由 hours 表达（前端自己
	// 知道选的是哪个预设），全部历史没有区间——两者回显都会变成噪音。
	if explicit {
		if !w.From.IsZero() {
			snap.WindowFrom = w.From.Format(time.RFC3339)
		}
		if !w.To.IsZero() {
			snap.WindowTo = w.To.Format(time.RFC3339)
		}
	}
	return snap
}

func canonicalUsageModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cn:", "global:"} {
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
			break
		}
	}
	if model == "" {
		return "(unknown)"
	}
	return model
}

func creditKeyed(m map[string]*creditAcc) []CreditAgg {
	out := make([]CreditAgg, 0, len(m))
	for _, v := range m {
		out = append(out, v.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		if out[i].CreditTokens != out[j].CreditTokens {
			return out[i].CreditTokens > out[j].CreditTokens
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Rate < out[j].Rate
	})
	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
