// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；每个频道独立限额，任务日志可原子落盘。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码", "panel: task ", "panel: 接受任务", "panel: 定时成长",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
type Ring struct {
	mu           sync.Mutex
	entries      []LogEntry
	cap          int
	taskArchive  string
	archiveError string
}

// NewRing 构建每频道容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// SetTaskArchive 恢复并持久化任务频道的最近日志。不存在的文件是正常首启。
// 在连接 log 输出之前调用；路径与 state_file 同目录，由入口负责注入。
func (r *Ring) SetTaskArchive(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taskArchive = path
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.archiveError = err.Error()
		return err
	}
	defer f.Close()
	var entries []LogEntry
	if err := json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&entries); err != nil {
		r.archiveError = err.Error()
		return fmt.Errorf("read task log archive: %w", err)
	}
	for _, entry := range entries {
		if entry.Ch != ChTask || entry.Text == "" {
			continue
		}
		entry.Text = boundedLogText(entry.Text)
		r.appendLocked(entry)
	}
	sort.SliceStable(r.entries, func(i, j int) bool { return r.entries[i].TS.Before(r.entries[j].TS) })
	r.archiveError = ""
	return nil
}

// ArchiveError 供面板反馈写盘故障，日志写失败不阻塞/破坏模型请求。
func (r *Ring) ArchiveError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.archiveError
}

func boundedLogText(text string) string {
	const maxRunes = 4096
	if len(text) <= maxRunes {
		return text
	}
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}

func (r *Ring) appendLocked(entry LogEntry) {
	r.entries = append(r.entries, entry)
	count, first := 0, -1
	for i, e := range r.entries {
		if e.Ch != entry.Ch {
			continue
		}
		count++
		if first == -1 {
			first = i
		}
	}
	if count > r.cap {
		r.entries = append(r.entries[:first], r.entries[first+1:]...)
	}
}

func (r *Ring) persistTasksLocked() {
	if r.taskArchive == "" {
		return
	}
	tasks := make([]LogEntry, 0, r.cap)
	for _, entry := range r.entries {
		if entry.Ch == ChTask {
			tasks = append(tasks, entry)
		}
	}
	raw, err := json.Marshal(tasks)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(r.taskArchive), 0700)
	}
	tmp := r.taskArchive + ".tmp"
	if err == nil {
		err = os.WriteFile(tmp, raw, 0600)
	}
	if err == nil {
		err = os.Rename(tmp, r.taskArchive)
	}
	if err != nil {
		r.archiveError = err.Error()
		_ = os.Remove(tmp)
	} else {
		r.archiveError = ""
	}
}

// Write 按 \n 切分入环（实现 io.Writer）。频道互不淘汰，任务结果独立保存。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	taskChanged := false
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := boundedLogText(tsPrefixRe.ReplaceAllString(line, ""))
		channel := classifyLine(text)
		r.appendLocked(LogEntry{TS: now, Ch: channel, Text: text})
		taskChanged = taskChanged || channel == ChTask
	}
	if taskChanged {
		r.persistTasksLocked()
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
