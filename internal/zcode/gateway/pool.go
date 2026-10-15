package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/captcha"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/proxy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/upstream"
)

type Server struct {
	cfg        config.Config
	store      *store.Store
	captcha    captcha.Solver
	sticky     *proxy.StickyProxy
	catalog    *Catalog
	proxies    *proxy.ProxyPool
	up         *upstream.Client
	mu         sync.Mutex
	loginFlows map[string]*loginFlow
	warmOnce   sync.Once
	quotaOnce  sync.Once
	// runtime knobs (Settings page): quota monitor interval + captcha pool
	quotaRefreshMin int
	poolMin         int
	poolMax         int
	poolTTL         time.Duration
}

func NewServer(cfg config.Config) (*Server, error) {
	st, err := store.OpenStore(config.AccountsDBPath)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:   cfg,
		store: st,
		// actual models come from the origin billing (ModelReport/refresh);
		// only the default model bootstraps the list before the first scan
		catalog: NewCatalog([]string{config.DefaultModelID}),
	}
	// THE proxy source: profiles in the store (the dashboard edits them).
	// PROXY_FILE only seeds them once on a fresh database; chat and billing
	// egress always read the live profile set.
	s.proxies = &proxy.ProxyPool{}
	s.sticky = &proxy.StickyProxy{Pool: s.proxies}
	s.seedProfilesFromFile()
	s.rebuildEgress()
	if n := s.proxies.Len(); n > 0 {
		log.Printf("[proxies] egress pool: %d lines (from proxy profiles)", n)
	} else {
		log.Printf("[proxies] egress pool empty — origin goes direct")
	}
	// mint output goes to the log only — params are single-use and never
	// replayed, so there is no persisted store
	var node captcha.Solver = captcha.NewNodeSolver(cfg)
	log.Printf("[captcha] solver=node (happy-dom solver subprocess)")
	// pre-solved stock: refill only while a servable account exists
	// (CAPTCHA_MODE=off mutes the background mint entirely)
	s.captcha = captcha.NewMintPool(node, func() bool { return !cfg.CaptchaOff && s.store.Active() != nil },
		s.quotaMin(), s.quotaMax(), s.quotaTTL())
	// keys set from the Settings page are user settings: persisted in the
	// store, they win over env values after restart
	if v := s.store.Setting("admin_password", ""); v != "" {
		s.cfg.AuthToken = v
	}
	if v := s.store.Setting("gateway_key", ""); v != "" {
		s.cfg.GatewayKey = v
	}
	// runtime knobs from stored settings (Settings page overrides live)
	s.quotaRefreshMin = atoiSetting(s.store, "quota_refresh_min", 0)
	s.poolMin = atoiSetting(s.store, "captcha_pool_min", 3)
	s.poolMax = atoiSetting(s.store, "captcha_pool_max", 10)
	if d, err := time.ParseDuration(s.store.Setting("captcha_pool_ttl", "95s")); err == nil && d > 0 {
		s.poolTTL = d
	} else {
		s.poolTTL = 95 * time.Second
	}

	s.up = &upstream.Client{
		Cfg:               cfg,
		Captcha:           s.captcha,
		Sticky:            s.sticky,  // chat: one sticky line
		Pool:              s.proxies, // billing family: round-robin spread
		ProfileForAccount: s.profileForAccountMid,
	}
	return s, nil
}

func (s *Server) importKeysFile() {
	n := 0
	for _, k := range config.LoadKeyFile(config.KeysFilePath) {
		if _, err := s.store.Add(k, ""); err == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("[pool] +%d from %s", n, config.KeysFilePath)
	}
}

func (s *Server) Close() {
	if s.captcha != nil {
		s.captcha.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}

func (s *Server) activeAccount() *store.Account {
	return s.store.Active()
}

func (s *Server) refreshAllBilling() (checked, dropped int) {
	accs, err := s.store.Accounts()
	if err != nil {
		return 0, 0
	}
	n := s.cfg.BillingWorkers
	if n < 1 {
		n = 1
	}
	if n > 32 {
		n = 32
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var nChk atomic.Int32
	var markMu sync.Mutex
	expiredIDs := []int64{}
	revokedIDs := []int64{}
	for _, a := range accs {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// billing rides the account's own device mid: warming and quota
			// scans must present ONE device per account, otherwise the origin
			// serves the scan a device it never saw
			q, err := s.up.Billing(a.JWT, a.DeviceMid)
			if err != nil {
				log.Printf("[billing] %s: %v", maskJWT(a.JWT), err)
				// 401 = the token is rejected by the origin: mark, keep the row
				if strings.Contains(fmt.Sprint(err), "HTTP 401") {
					markMu.Lock()
					revokedIDs = append(revokedIDs, a.ID)
					markMu.Unlock()
				}
				return
			}
			nChk.Add(1)
			if upstream.IsEmptyGrant(q) {
				markMu.Lock()
				expiredIDs = append(expiredIDs, a.ID)
				markMu.Unlock()
				return
			}
			s.mu.Lock()
			_ = s.store.UpdateQuota(a.ID, q)
			s.catalog.MergeFromQuota(q)
			s.mu.Unlock()
			log.Printf("[billing] %s plan=%s rem=%d", maskJWT(a.JWT), q.PlanName, q.Remaining)
		}()
	}
	wg.Wait()
	dropped = s.store.MarkStatus(expiredIDs, "expired")
	if n := s.store.MarkStatus(revokedIDs, "revoked"); n > 0 {
		log.Printf("[billing] marked %d revoked (401) — hidden, kept in db", n)
	}
	if dropped > 0 {
		log.Printf("[billing] marked %d empty grants expired — hidden, kept in db", dropped)
	}
	return int(nChk.Load()), dropped
}

func (s *Server) pickExcept(model string, skip map[int64]bool) (*store.Account, error) {
	accs, err := s.store.Accounts()
	if err != nil {
		return nil, err
	}
	if len(accs) == 0 {
		return nil, fmt.Errorf("no accounts")
	}
	dead := s.store.DeadSet()
	mkey := upstream.Norm(model)
	// per-model rotation cursor: each model gets its own round-robin
	// start position
	start := s.store.Rotate("m:"+mkey, len(accs))
	order := append(accs[start:], accs[:start]...)
	for _, a := range order {
		if skip[a.ID] {
			continue
		}
		if a.Status != "" {
			continue // expired or revoked (hidden, kept in db)
		}
		if a.CoolingUntil > time.Now().Unix() {
			continue // risk-control cooldown (strikes)
		}
		if a.DisabledModels != "" {
			skipDisabled := false
			for _, dm := range strings.Split(a.DisabledModels, ",") {
				if strings.TrimSpace(dm) == mkey {
					skipDisabled = true
					break
				}
			}
			if skipDisabled {
				continue // model manually excluded for this account
			}
		}
		if dead[fmt.Sprintf("%d:%s", a.ID, mkey)] {
			continue
		}
		ents := entitlementsOf(a)
		if len(ents) > 0 {
			// the plan was scanned: rotate the account only for models it
			// offers, and only while quota remains (a planless/exhausted
			// account is dead weight for this request)
			if frac, ok := upstream.FracFromEntitlements(ents, model); !ok {
				continue // plan scanned, this model is not in it
			} else if frac <= 0 {
				// quota exhausted — dead until the period reset
				s.store.MarkDead(a.ID, mkey, upstream.DeadUntilUnix(ents, model))
				continue
			}
		}
		return &a, nil
	}
	return nil, fmt.Errorf("pool empty for model %s", model)
}

func (s *Server) markQuotaDead(cur *store.Account, model string) {
	if cur == nil {
		return
	}
	mkey := upstream.Norm(model)
	ents := entitlementsOf(*cur)
	frac, ok := upstream.FracFromEntitlements(ents, model)
	if !ok || frac <= 0 {
		until := upstream.DeadUntilUnix(ents, model)
		s.store.MarkDead(cur.ID, mkey, until)
		log.Printf("[pool] exhausted %s model=%s until %d", maskJWT(cur.JWT), model, until)
		return
	}
	log.Printf("[pool] 1005 %s still %.0f%% — skip this request, not dead", maskJWT(cur.JWT), frac*100)
}

func entitlementsOf(a store.Account) []map[string]any {
	if a.QuotaJSON == "" {
		return nil
	}
	var ents []map[string]any
	_ = json.Unmarshal([]byte(a.QuotaJSON), &ents)
	return ents
}

func maskJWT(j string) string {
	if len(j) < 12 {
		return "***"
	}
	return j[:6] + "…" + j[len(j)-4:]
}

// withGatewayAuth guards /v1/* client endpoints: GATEWAY_KEY when set,
// otherwise the shared AUTH_TOKEN (gateway-key semantics).
func (s *Server) withGatewayAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.GatewayKey
		if key == "" {
			key = s.cfg.AuthToken
		}
		got := r.Header.Get("Authorization")
		const p = "Bearer "
		if len(got) > len(p) && got[:len(p)] == p {
			got = got[len(p):]
		}
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != key {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"invalid gateway key","type":"authentication_error"}}`))
			return
		}
		next(w, r)
	}
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" {
			next(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		const p = "Bearer "
		if len(got) > len(p) && got[:len(p)] == p {
			got = got[len(p):]
		}
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != s.cfg.AuthToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"invalid auth token","type":"authentication_error"}}`))
			return
		}
		next(w, r)
	}
}

func oauthBaseEnv() string      { return os.Getenv("ZCODE_OAUTH_API_BASE") }
func exchangeOriginEnv() string { return os.Getenv("ZCODE_EXCHANGE_ORIGIN") }

func atoiSetting(st *store.Store, key string, def int) int {
	if n, err := strconv.Atoi(st.Setting(key, "")); err == nil && n > 0 {
		return n
	}
	return def
}

func (s *Server) quotaMin() int           { return s.poolMin }
func (s *Server) quotaMax() int           { return s.poolMax }
func (s *Server) quotaTTL() time.Duration { return s.poolTTL }
