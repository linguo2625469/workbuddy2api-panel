package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/captcha"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/upstream"
)

type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int
	MaxCompletion int
}

type Catalog struct {
	mu      sync.Mutex
	models  []ModelInfo
	aliases map[string]string // lowercase -> display id from billing
}

func NewCatalog(fallback []string) *Catalog {
	c := &Catalog{aliases: map[string]string{}}
	for _, id := range fallback {
		c.add(id)
	}
	return c
}

func (c *Catalog) add(id string) {
	if id == "" {
		return
	}
	c.models = append(c.models, ModelInfo{ID: id, Name: id})
	c.aliases[upstream.Norm(id)] = id
}

// Resolve maps any casing/alias of a model to the canonical id from billing.
func (c *Catalog) Resolve(model string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, ok := c.aliases[upstream.Norm(model)]; ok {
		return id
	}
	return model
}

// EnrichConfigs merges per-model metadata from the origin client/configs
// endpoint into existing catalog entries (billing models only are served).
func (c *Catalog) EnrichConfigs(meta map[string]ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.models {
		if m, ok := meta[c.models[i].ID]; ok {
			if m.ContextWindow > 0 {
				c.models[i].ContextWindow = m.ContextWindow
			}
			if m.MaxCompletion > 0 {
				c.models[i].MaxCompletion = m.MaxCompletion
			}
		}
	}
}

func (c *Catalog) MergeFromQuota(q store.Quota) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range q.Entitlements {
		name := upstream.Str(e["name"])
		if name == "" {
			continue
		}
		if _, ok := c.aliases[upstream.Norm(name)]; ok {
			continue
		}
		c.add(name)
	}
	for _, b := range q.Balances {
		name := upstream.Str(b["show_name"])
		if name == "" {
			continue
		}
		if _, ok := c.aliases[upstream.Norm(name)]; ok {
			continue
		}
		c.add(name)
	}
}

func (c *Catalog) List() []ModelInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ModelInfo, len(c.models))
	copy(out, c.models)
	return out
}

// ModelStat — actual model availability aggregated across the account pool.
type ModelStat struct {
	Model     string
	Accounts  int
	Remaining int
	Plan      string
}

// ModelReport refreshes billing for every account (same as the dashboard
// "Refresh quota") and aggregates the ACTUAL per-model availability from the
// origin. The catalog is merged along the way, so /v1/models matches the
// server afterwards. limit caps the accounts scanned (0 = all).
func (s *Server) ModelReport(limit int) []ModelStat {
	accs, err := s.store.Accounts()
	if err != nil {
		return nil
	}
	if limit > 0 && len(accs) > limit {
		accs = accs[:limit]
	}
	var mu sync.Mutex
	byModel := map[string]*ModelStat{}
	sem := make(chan struct{}, max(1, s.cfg.BillingWorkers))
	var wg sync.WaitGroup
	for _, a := range accs {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			q, err := s.up.Billing(a.JWT, a.DeviceMid)
			if err != nil {
				log.Printf("[model-check] %s: %v", maskJWT(a.JWT), err)
				return
			}
			names := map[string]bool{}
			for _, b := range q.Balances {
				if n := upstream.Str(b["show_name"]); n != "" {
					names[n] = true
				}
			}
			for _, e := range q.Entitlements {
				if n := upstream.Str(e["name"]); n != "" {
					names[n] = true
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for n := range names {
				st := byModel[n]
				if st == nil {
					st = &ModelStat{Model: n, Plan: q.PlanName}
					byModel[n] = st
				}
				st.Accounts++
			}
			for _, e := range q.Entitlements {
				if st := byModel[upstream.Str(e["name"])]; st != nil {
					st.Remaining += upstream.AsInt(e["remaining"])
				}
			}
			s.catalog.MergeFromQuota(q)
		}()
	}
	wg.Wait()
	out := make([]ModelStat, 0, len(byModel))
	for _, st := range byModel {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// enrichCatalogFromConfigs pulls the client model metadata (context window,
// reasoning config) from the origin client/configs endpoint (the shared
// captcha.ClientConfigs fetch) and merges it into the billing-derived
// catalog.
func (s *Server) enrichCatalogFromConfigs() {
	raw := captcha.ClientConfigs(context.Background())
	if raw == nil {
		log.Printf("[models] client/configs: no data")
		return
	}
	var wrap struct {
		Code int `json:"code"`
		Data struct {
			Providers []struct {
				ID     string `json:"id"`
				Models []struct {
					ModelID       string `json:"modelId"`
					Name          string `json:"name"`
					ContextWindow int    `json:"contextWindow"`
					MaxCompletion int    `json:"maxCompletionTokens"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.Code != 0 {
		log.Printf("[models] client/configs parse: %v", err)
		return
	}
	meta := map[string]ModelInfo{}
	for _, p := range wrap.Data.Providers {
		for _, m := range p.Models {
			if m.ModelID == "" {
				continue
			}
			meta[m.ModelID] = ModelInfo{
				ID:            m.ModelID,
				Name:          m.Name,
				ContextWindow: m.ContextWindow,
				MaxCompletion: m.MaxCompletion,
			}
		}
	}
	s.catalog.EnrichConfigs(meta)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	type item struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		OwnedBy       string `json:"owned_by"`
		Name          string `json:"name,omitempty"`
		ContextWindow int    `json:"context_window,omitempty"`
		MaxCompletion int    `json:"max_completion_tokens,omitempty"`
	}
	var data []item
	for _, m := range s.catalog.List() {
		it := item{
			ID:      m.ID,
			Object:  "model",
			OwnedBy: "zcode",
			Name:    m.Name,
		}
		if m.ContextWindow > 0 {
			it.ContextWindow = m.ContextWindow
		}
		if m.MaxCompletion > 0 {
			it.MaxCompletion = m.MaxCompletion
		}
		data = append(data, it)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	acc := s.activeAccount()
	if acc == nil {
		http.Error(w, `{"error":"no accounts"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"account_id": acc.ID,
		"plan":       acc.PlanName,
		"quota_json": json.RawMessage(emptyJSON(acc.QuotaJSON)),
		"expires_at": acc.ExpiresAt,
	})
}

func emptyJSON(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}
