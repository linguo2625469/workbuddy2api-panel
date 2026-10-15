package gateway

// asyncpool.go — /async/v1/messages (ticket-SSE):
// ticket-model SSE. The client POSTs the same body as /v1/messages; the
// response is an SSE stream: ticket{id,status:pending} → ready{processing}
// → chunk* (raw upstream SSE payloads) → done|error. Retries with account
// switching and 2^n backoff happen inside; keepalive comments every 10s
// during silence; a client disconnect aborts the upstream (no quota burn).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

const asyncMaxRetries = 3

func (s *Server) handleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 50<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming required"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ticket := util.GenerateUUID()
	writeSSE(w, fl, "ticket", map[string]any{"id": ticket, "status": "pending"})

	// the upstream work aborts when the client goes away
	go func() {
		<-ctx.Done()
	}()
	writeSSE(w, fl, "ready", map[string]any{"status": "processing"})

	outcome := s.runAsync(ctx, w, fl, body, ticket)
	if outcome == "" {
		writeSSE(w, fl, "done", map[string]any{"id": ticket})
	} else {
		writeSSE(w, fl, "error", map[string]any{"id": ticket, "message": outcome})
	}
}

// runAsync streams upstream chunks; returns "" on done, else the error text.
func (s *Server) runAsync(ctx context.Context, w http.ResponseWriter, fl http.Flusher, body []byte, ticket string) string {
	model := upstream.ParseModel(body)
	skip := map[int64]bool{}
	for attempt := 0; attempt <= asyncMaxRetries; attempt++ {
		if ctx.Err() != nil {
			return "canceled"
		}
		acc, err := s.pickExcept(model, skip)
		if err != nil || acc == nil {
			return "no available account"
		}
		payload := s.normalizeBody(body, model, acc.JWT)
		resp, err := s.up.PostMessages(ctx, acc.JWT, acc.DeviceMid, payload, nil)
		if err != nil {
			// network-level failure → switch account with 2^n backoff
			skip[acc.ID] = true
			select {
			case <-ctx.Done():
				return "canceled"
			case <-time.After(time.Duration(1<<uint(attempt)) * time.Second):
			}
			continue
		}
		if resp.StatusCode != 200 {
			head, _, _ := upstream.PeekBody(resp, 256)
			if upstream.IsQuotaFamily429(resp.StatusCode, head) || upstream.IsQuotaError(head) {
				resp.Body.Close()
				s.markQuotaDead(acc, model)
				skip[acc.ID] = true
				continue
			}
			// surface other statuses as errors (the stream already began)
			resp.Body.Close()
			return fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		}
		// stream chunks: raw SSE payloads from the origin
		_ = s.store.Select(acc.ID)
		br := bufio.NewReader(resp.Body)
		sent := 0
		for {
			if ctx.Err() != nil {
				resp.Body.Close()
				return "canceled" // client gone: stop burning quota
			}
			line, err := br.ReadString('\n')
			if err != nil {
				resp.Body.Close()
				if sent > 0 {
					return "" // mid-stream end: treat as done (no re-send)
				}
				return "upstream stream died"
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" {
					writeSSE(w, fl, "chunk", json.RawMessage(payload))
					sent++
				}
			}
		}
	}
	return "retries exhausted"
}

func writeSSE(w http.ResponseWriter, fl http.Flusher, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if fl != nil {
		fl.Flush()
	}
}
