// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	HasCredit        bool      `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	// recentHead 下一个写入位置；recentLen 已写入条数（≤ recentLimit）。
	recentHead int
	recentLen  int
	archive    *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	// 定长环形缓冲：写入 O(1)。此前用 append([]Event{e}, r.recent...) 头插，
	// 每请求都重建并拷贝整个 100 元素切片（且在锁内）。改为尾部追加 + 读取时
	// 反向复制，Recent 仍保持「最新在前」的顺序语义。
	r.recent[r.recentHead] = e
	r.recentHead = (r.recentHead + 1) % recentLimit
	if r.recentLen < recentLimit {
		r.recentLen++
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// recentDescending 返回「最新在前」的最近事件副本。调用方须持有 r.mu。
//
// 环形缓冲按写入顺序（最旧→最新）存放，此处从 newest 往回走完成反转。
func (r *Recorder) recentDescending() []Event {
	out := make([]Event, 0, r.recentLen)
	for i := 0; i < r.recentLen; i++ {
		idx := (r.recentHead - 1 - i + recentLimit) % recentLimit
		out = append(out, r.recent[idx])
	}
	return out
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    r.recentDescending(),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		rows, err := readFile(filepath.Join(w.cfg.Dir, name), filter)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func readFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []Event
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
