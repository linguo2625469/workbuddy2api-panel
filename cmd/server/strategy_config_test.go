package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegionStrategyConfigCompatibility(t *testing.T) {
	for _, raw := range []string{`{}`, `{"pool":{}}`, `{"pool":{"strategy":{"domestic":""}}}`} {
		c, err := ParseConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if c.Pool.Strategy.Domestic != pool.WeightedRandom || c.Pool.Strategy.International != pool.WeightedRandom {
			t.Fatalf("legacy defaults: %+v", c.Pool.Strategy)
		}
	}
	c, err := ParseConfig([]byte(`{"pool":{"strategy":{"domestic":" ROUND_ROBIN ","international":"sequential_exhaust"},"account_order":{"international":["b","a"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.Strategy.Domestic != pool.RoundRobin || c.Pool.Strategy.International != pool.SequentialExhaust || c.Pool.AccountOrder.International[0] != "b" {
		t.Fatalf("config=%+v", c.Pool)
	}
	for _, raw := range []string{`{"pool":{"strategy":{"domestic":"bad"}}}`, `{"pool":{"strategy":{"international":"bad"}}}`, `{"pool":{"strategy":"round_robin"}}`} {
		_, err := ParseConfig([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "strategy") {
			t.Fatalf("invalid config err=%v", err)
		}
	}
}

func TestSaveRegionSchedulingHotAppliesAndRejectsInvalid(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"pool":{"strategy":{"international":"sequential_exhaust"}},"custom":"keep"}`)
	if err := os.WriteFile(fp, original, 0600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	live := livecfg.New(livecfg.Snapshot{})
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	raw := []byte(`{"pool":{"strategy":{"domestic":"round_robin"},"account_order":{"domestic":["b","a"]}}}`)
	restart, err := saveConfig(raw, fp, live, p, up, sch)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range restart {
		if strings.HasPrefix(key, "pool.") {
			t.Fatalf("strategy requires restart: %s", key)
		}
	}
	for _, uid := range []string{"b", "a", "b"} {
		if a := p.PickExcludingForRealm(nil, "m", "cn"); a == nil || a.UID != uid {
			t.Fatalf("hot pick=%v want %s", a, uid)
		}
	}
	saved, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.Strategy.International != pool.SequentialExhaust || !strings.Contains(string(saved), "keep") {
		t.Fatalf("merge lost config: %s", saved)
	}
	_, err = saveConfig([]byte(`{"pool":{"strategy":{"domestic":"bad"}}}`), fp, live, p, up, sch)
	if err == nil {
		t.Fatal("invalid strategy accepted")
	}
	after, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(after) {
		t.Fatal("invalid save changed file")
	}
	if a := p.PickExcludingForRealm(nil, "m", "cn"); a == nil || a.UID != "a" {
		t.Fatalf("invalid save changed strategy: %v", a)
	}
}
