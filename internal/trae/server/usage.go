// usage.go trae 请求用量记录：按天（本地日期）+ 按账号/模型维度记请求数与
// tokens，每次记录后落盘（低频写足够）；保留最近 30 天。
// 采集点在 chatCompletions：非流式读响应 usage 字段；流式用 capWriter 抓
// SSE 尾部的 usage 块（OpenAI 兼容末帧）。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// usageEntry 单维度用量聚合。
type usageEntry struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (e *usageEntry) add(o usageEntry) {
	e.Requests += o.Requests
	e.Errors += o.Errors
	e.PromptTokens += o.PromptTokens
	e.CompletionTokens += o.CompletionTokens
	e.TotalTokens += o.TotalTokens
}

// usageDay 单天聚合。
type usageDay struct {
	Date      string                `json:"date"`
	Entry     usageEntry            `json:"-"`
	ByAccount map[string]usageEntry `json:"by_account"`
	ByModel   map[string]usageEntry `json:"by_model"`
}

func (d *usageDay) recompute() {
	d.Entry = usageEntry{}
	for _, e := range d.ByAccount {
		d.Entry.add(e)
	}
}

// usageStore 用量存储。nil 安全：所有方法判空直接返回。
type usageStore struct {
	mu   sync.Mutex
	path string
	days map[string]*usageDay
}

// maxUsageDays 保留天数（超出在 save 时裁剪）。
const maxUsageDays = 30

// capWriter 流式响应尾巴捕获：保留最后 tailCap 字节，用于抓 usage 末帧。
type capWriter struct {
	http.ResponseWriter
	tail []byte
}

const tailCap = 256 << 10

func (w *capWriter) Write(p []byte) (int, error) {
	w.tail = append(w.tail, p...)
	if len(w.tail) > tailCap {
		copy(w.tail, w.tail[len(w.tail)-tailCap:])
		w.tail = w.tail[:tailCap]
	}
	return w.ResponseWriter.Write(p)
}

// usageRe SSE 流中 usage 块起始位置。
var usageRe = regexp.MustCompile(`"usage"\s*:\s*\{`)

// extractUsage 从流尾巴解析最后一个 usage 块（prompt/completion tokens）。
func extractUsage(tail []byte) (prompt, completion int64, ok bool) {
	idx := usageRe.FindAllIndex(tail, -1)
	for i := len(idx) - 1; i >= 0; i-- {
		start := idx[i][1] - 1 // 指向 '{'
		dec := json.NewDecoder(bytes.NewReader(tail[start:]))
		var u struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		}
		if err := dec.Decode(&u); err == nil {
			return u.Prompt, u.Completion, true
		}
	}
	return 0, 0, false
}

// NewUsageStore 打开（或初始化）用量存储；path 为空返回 nil（不记录）。
func NewUsageStore(path string) *usageStore {
	if path == "" {
		return nil
	}
	s := &usageStore{path: path, days: map[string]*usageDay{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.days)
	}
	return s
}

func (s *usageStore) record(uid, model string, prompt, completion int64, isErr bool) {
	if s == nil {
		return
	}
	e := usageEntry{Requests: 1}
	if isErr {
		e.Errors = 1
	}
	e.PromptTokens = prompt
	e.CompletionTokens = completion
	e.TotalTokens = prompt + completion

	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.dayLocked(time.Now().Format("2006-01-02"))
	acc := day.ByAccount[uid]
	acc.add(e)
	day.ByAccount[uid] = acc
	if model == "" {
		model = "unknown"
	}
	m := day.ByModel[model]
	m.add(e)
	day.ByModel[model] = m
	day.recompute()
	s.saveLocked()
}

func (s *usageStore) dayLocked(date string) *usageDay {
	d, ok := s.days[date]
	if !ok {
		d = &usageDay{Date: date, ByAccount: map[string]usageEntry{}, ByModel: map[string]usageEntry{}}
		s.days[date] = d
	}
	return d
}

func (s *usageStore) saveLocked() {
	// 裁剪：只留最近 maxUsageDays 天
	if len(s.days) > maxUsageDays {
		dates := make([]string, 0, len(s.days))
		for k := range s.days {
			dates = append(dates, k)
		}
		sort.Strings(dates)
		for _, k := range dates[:len(dates)-maxUsageDays] {
			delete(s.days, k)
		}
	}
	if b, err := json.Marshal(s.days); err == nil {
		_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
		_ = os.WriteFile(s.path, b, 0o644)
	}
}

// usageDayJSON 对外条目（带天级合计）。
type usageDayJSON struct {
	Date       string                `json:"date"`
	Requests   int64                 `json:"requests"`
	Errors     int64                 `json:"errors"`
	Prompt     int64                 `json:"prompt_tokens"`
	Completion int64                 `json:"completion_tokens"`
	Total      int64                 `json:"total_tokens"`
	ByAccount  map[string]usageEntry `json:"by_account"`
	ByModel    map[string]usageEntry `json:"by_model"`
}

// snapshot 返回按日期倒序的天列表（面板渲染用）。
func (s *usageStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"days": []usageDayJSON{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dates := make([]string, 0, len(s.days))
	for k := range s.days {
		dates = append(dates, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	out := make([]usageDayJSON, 0, len(dates))
	for _, k := range dates {
		d := s.days[k]
		out = append(out, usageDayJSON{
			Date: d.Date, Requests: d.Entry.Requests, Errors: d.Entry.Errors,
			Prompt: d.Entry.PromptTokens, Completion: d.Entry.CompletionTokens, Total: d.Entry.TotalTokens,
			ByAccount: d.ByAccount, ByModel: d.ByModel,
		})
	}
	return map[string]any{"days": out}
}

// adminUsage GET /admin/api/usage：返回按天用量（含按账号/按模型维度）。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.usage.snapshot())
}
