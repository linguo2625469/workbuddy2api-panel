package gateway

// reqlog.go — request ring log + usage accounting
// go_eng usage tee): every chat request lands in a 500-entry ring with
// model/account/status/timing; SSE bodies are tee-parsed for the final
// usage (input/output/cache tokens) and accumulated per account.

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

type ReqEntry struct {
	TS      time.Time `json:"ts"`
	Model   string    `json:"model"`
	Account string    `json:"account"` // masked jwt
	OK      bool      `json:"ok"`
	Status  int       `json:"status"`
	Stream  bool      `json:"stream"`
	TFirst  int64     `json:"t_first_ms"`
	TTotal  int64     `json:"t_total_ms"`
	InTok   int       `json:"in_tok"`
	OutTok  int       `json:"out_tok"`
	Cache   int       `json:"cache_tok"`
	ErrNote string    `json:"err,omitempty"`
}

const reqRingSize = 500

type reqLog struct {
	mu    sync.Mutex
	ring  []ReqEntry
	idx   int
	stats struct {
		total    int64
		ok       int64
		inTok    int64
		outTok   int64
		cacheTok int64
	}
	started time.Time
}

var rlog = &reqLog{ring: make([]ReqEntry, reqRingSize), started: time.Now()}

func (l *reqLog) Add(e ReqEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring[l.idx] = e
	l.idx = (l.idx + 1) % len(l.ring)
	l.stats.total++
	if e.OK {
		l.stats.ok++
	}
	l.stats.inTok += int64(e.InTok)
	l.stats.outTok += int64(e.OutTok)
	l.stats.cacheTok += int64(e.Cache)
}

// LogOK records a finished request (forward.go hot path).
func LogOK(mdl, acc string, status int, stream bool, tFirst, tTotal time.Duration, usg Usage) {
	rlog.Add(ReqEntry{
		TS: time.Now(), Model: mdl, Account: acc, OK: status == 200,
		Status: status, Stream: stream,
		TFirst: tFirst.Milliseconds(), TTotal: tTotal.Milliseconds(),
		InTok: usg.In, OutTok: usg.Out, Cache: usg.CacheRead + usg.CacheCreate,
	})
}

// Usage — token triple extracted from the SSE stream.
type Usage struct {
	In          int
	Out         int
	CacheRead   int
	CacheCreate int
}

// usageScanner parses Anthropic SSE chunks for usage fields (message_start
// usage.input_tokens + cache_read/creation; message_delta usage.output_tokens).
type usageScanner struct {
	buf   bytes.Buffer
	found Usage
}

func (u *usageScanner) Write(p []byte) (int, error) {
	u.buf.Write(p)
	for {
		line, err := u.buf.ReadBytes('\n')
		if err != nil {
			// partial line — put it back for the next write
			u.buf.Write(line)
			break
		}
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Message struct {
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		if ev.Usage != nil {
			if ev.Usage.OutputTokens > 0 {
				u.found.Out = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				u.found.In = ev.Usage.InputTokens
			}
		}
		if ev.Message.Usage != nil { // message_start
			if ev.Message.Usage.InputTokens > 0 {
				u.found.In = ev.Message.Usage.InputTokens
			}
			u.found.CacheRead = ev.Message.Usage.CacheReadInputTokens
			u.found.CacheCreate = ev.Message.Usage.CacheCreationInputTokens
		}
	}
	return len(p), nil
}

// Usage returns the scanned token triple. Falls back to parsing the
// buffered body as a plain JSON response (non-stream path: usage sits in
// the message object, not SSE data lines).
func (u *usageScanner) Usage() Usage {
	if u.found.In == 0 && u.found.Out == 0 {
		var resp struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(u.buf.Bytes(), &resp) == nil && resp.Usage != nil {
			return Usage{
				In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens,
				CacheRead:   resp.Usage.CacheReadInputTokens,
				CacheCreate: resp.Usage.CacheCreationInputTokens,
			}
		}
	}
	return u.found
}

// monitorSnapshot powers /api/monitor.
type monitorSnapshot struct {
	Uptime      string     `json:"uptime"`
	Requests    int64      `json:"requests"`
	SuccessRate float64    `json:"success_rate"`
	QPS         float64    `json:"qps"`
	InTok       int64      `json:"in_tok"`
	OutTok      int64      `json:"out_tok"`
	CacheTok    int64      `json:"cache_tok"`
	Recent      []ReqEntry `json:"recent"`
}

func monitor() monitorSnapshot {
	l := rlog
	l.mu.Lock()
	defer l.mu.Unlock()
	up := time.Since(l.started).Seconds()
	sr := 0.0
	if l.stats.total > 0 {
		sr = float64(l.stats.ok) / float64(l.stats.total)
	}
	qps := 0.0
	if up > 0 {
		qps = float64(l.stats.total) / up
	}
	recent := make([]ReqEntry, 0, reqRingSize)
	for i := 0; i < reqRingSize; i++ {
		j := (l.idx + reqRingSize - 1 - i + reqRingSize) % reqRingSize // newest first
		if !l.ring[j].TS.IsZero() {
			recent = append(recent, l.ring[j])
		}
	}
	return monitorSnapshot{
		Uptime:   time.Since(l.started).Round(time.Second).String(),
		Requests: l.stats.total, SuccessRate: sr, QPS: qps,
		InTok: l.stats.inTok, OutTok: l.stats.outTok, CacheTok: l.stats.cacheTok,
		Recent: recent,
	}
}
