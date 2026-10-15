package gateway

// proxies.go — proxy profiles:
// CRUD in the meta table, per-account binding, sticky|rotate mode, live
// IP probe (3-tier fallback), auto-pause on probe failure.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"sync"
	"time"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/proxy"
)

type ProxyProfile struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Mode    string `json:"mode"` // "sticky" | "rotate"
	Paused  bool   `json:"paused"`
	BoundTo int64  `json:"bound_to_account"` // account id, 0 = any
}

var proxyMu sync.Mutex

// seedProfilesFromFile imports the PROXY_FILE pool into editable profiles
// once, on the first boot with a fresh database; while profiles exist the
// stored set is the single source of truth (bindings survive edits). When
// the stored set is EMPTY (fresh DB or after Clear all) the file seeds it
// again at boot — a refreshed proxy.txt loads with a plain restart.
func (s *Server) seedProfilesFromFile() {
	proxyMu.Lock()
	raw := strings.TrimSpace(s.store.Setting("proxy_profiles", ""))
	if raw != "" && raw != "[]" && raw != "null" {
		proxyMu.Unlock()
		return // profiles exist — the file already seeded once
	}
	var list []ProxyProfile
	pool, err := proxy.LoadProxyPool(s.cfg.ProxyFile)
	if err == nil {
		for i := 0; i < pool.Len(); i++ {
			u := pool.At(i)
			if u == nil {
				continue
			}
			list = append(list, ProxyProfile{
				ID:   time.Now().UnixMilli() + int64(i),
				Name: u.Host,
				URL:  u.String(),
				Mode: "sticky",
			})
		}
	}
	if len(list) == 0 {
		proxyMu.Unlock()
		return // nothing to seed (no file yet) — stay on the empty pool
	}
	s.saveProxyProfilesLocked(list)
	proxyMu.Unlock() // rebuildEgress re-locks — never call it under proxyMu
	log.Printf("[proxies] seeded %d lines from %s into proxy profiles", len(list), s.cfg.ProxyFile)
	s.rebuildEgress()
}

// rebuildEgress is the bridge from the profile store to the live pools:
// every unpaused, unbound profile forms the shared egress pool (sticky
// chat egress + round-robin billing egress). Called on every mutation.
func (s *Server) rebuildEgress() {
	var urls []*url.URL
	for _, p := range s.loadProxyProfiles() {
		if p.Paused || p.BoundTo != 0 {
			continue
		}
		if u, err := url.Parse(p.URL); err == nil && u.Host != "" {
			urls = append(urls, u)
		}
	}
	s.proxies.SetURLs(urls)
	// drop the sticky pick if it just left the pool (deleted/paused/bound)
	if cur := s.sticky.Current(); cur != nil && !containsURL(urls, cur) {
		s.sticky.Pin(nil)
	}
}

func containsURL(urls []*url.URL, u *url.URL) bool {
	want := u.String()
	for _, x := range urls {
		if x.String() == want {
			return true
		}
	}
	return false
}

func (s *Server) loadProxyProfiles() []ProxyProfile {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	raw := s.store.Setting("proxy_profiles", "[]")
	var list []ProxyProfile
	_ = json.Unmarshal([]byte(raw), &list)
	return list
}

func (s *Server) saveProxyProfilesLocked(list []ProxyProfile) {
	b, _ := json.Marshal(list)
	s.store.SetSetting("proxy_profiles", string(b))
}

func (s *Server) saveProxyProfiles(list []ProxyProfile) {
	proxyMu.Lock()
	s.saveProxyProfilesLocked(list)
	proxyMu.Unlock()
	s.rebuildEgress()
}

func (s *Server) apiProxyList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.seedProfilesFromFile()
	writeJSON(w, map[string]any{"profiles": s.loadProxyProfiles()})
}

func (s *Server) apiProxySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var p ProxyProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.URL == "" {
		http.Error(w, `{"error":"url required"}`, http.StatusBadRequest)
		return
	}
	if _, err := url.Parse(p.URL); err != nil {
		http.Error(w, `{"error":"bad url"}`, http.StatusBadRequest)
		return
	}
	if p.Mode != "rotate" {
		p.Mode = "sticky"
	}
	list := s.loadProxyProfiles()
	if p.ID == 0 {
		p.ID = time.Now().UnixMilli()
		list = append(list, p)
	} else {
		for i := range list {
			if list[i].ID == p.ID {
				list[i] = p
				break
			}
		}
	}
	s.saveProxyProfiles(list)
	writeJSON(w, map[string]any{"ok": true, "profiles": list})
}

func (s *Server) apiProxyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	list := s.loadProxyProfiles()
	out := list[:0]
	for _, p := range list {
		if p.ID != body.ID {
			out = append(out, p)
		}
	}
	s.saveProxyProfiles(out)
	writeJSON(w, map[string]any{"ok": true, "profiles": out})
}

var ipProbeServices = []string{
	"https://api.ip.sb/ip",
	"https://ipwho.is/?field=ip",
	"https://ipapi.is/",
}

// apiProxyProbe tests a profile URL: public IP + latency through the proxy
// (3-tier service fallback). Two consecutive failures auto-pause a profile
// (probe-fail → pause, per the dashboard design).
func (s *Server) apiProxyProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	list := s.loadProxyProfiles()
	var target *ProxyProfile
	for i := range list {
		if list[i].ID == body.ID {
			target = &list[i]
			break
		}
	}
	if target == nil && body.URL != "" {
		target = &ProxyProfile{ID: 0, Name: "ad-hoc", URL: body.URL, Mode: "sticky"}
	}
	if target == nil {
		// "Test current line": probe the direct egress (no proxy line) —
		// the IP the server actually leaves from today
		ip, ms, err := probeIP("")
		resp := map[string]any{"url": "", "ip": ip, "latency_ms": ms}
		if err != nil {
			resp["error"] = err.Error()
		}
		writeJSON(w, resp)
		return
	}
	ip, ms, err := probeIP(target.URL)
	resp := map[string]any{"url": target.URL, "ip": ip, "latency_ms": ms}
	if err != nil {
		resp["error"] = err.Error()
		// auto-pause on repeated failure (only for stored profiles)
		if target.ID != 0 {
			fails := s.store.Setting(proxyFailKey(target.ID), "0")
			n := 1
			fmt.Sscanf(fails, "%d", &n)
			n++
			s.store.SetSetting(proxyFailKey(target.ID), fmt.Sprint(n))
			if n >= 2 {
				for i := range list {
					if list[i].ID == target.ID {
						list[i].Paused = true
					}
				}
				s.saveProxyProfiles(list)
				resp["paused"] = true
			}
		}
	} else if target.ID != 0 {
		s.store.SetSetting(proxyFailKey(target.ID), "0")
	}
	writeJSON(w, resp)
}

func proxyFailKey(id int64) string { return fmt.Sprintf("proxy_fail_%d", id) }

func probeIP(proxyURL string) (string, int64, error) {
	var proxy func(*http.Request) (*url.URL, error)
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return "", 0, err
		}
		proxy = http.ProxyURL(u)
	} // empty → direct (no proxy)
	tr := &http.Transport{Proxy: proxy}
	client := &http.Client{Timeout: 12 * time.Second, Transport: tr}
	var lastErr error
	for _, svc := range ipProbeServices {
		start := time.Now()
		resp, err := client.Get(svc)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		ip := extractIP(string(raw))
		if ip != "" {
			return ip, time.Since(start).Milliseconds(), nil
		}
		lastErr = fmt.Errorf("%s: no ip in response", svc)
	}
	return "", 0, lastErr
}

func extractIP(body string) string {
	// plain-text services return the IP alone; ipwho.is returns JSON
	body = strings.TrimSpace(body)
	if i := strings.Index(body, `"ip"`); i >= 0 {
		rest := body[i+4:]
		if j := strings.Index(rest, `"`); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, `"`); k >= 0 {
				return rest[:k]
			}
		}
	}
	fields := strings.Fields(body)
	if len(fields) > 0 {
		f := fields[0]
		if len(f) <= 45 && !strings.Contains(f, " ") {
			return f
		}
	}
	return ""
}

// profileForAccountMid resolves a bound proxy by the account's device-mid
// (the identity PostMessages carries). nil = sticky pool.
func (s *Server) profileForAccountMid(deviceMid string) *url.URL {
	if deviceMid == "" {
		return nil
	}
	accs, _ := s.store.Accounts()
	for _, a := range accs {
		if a.DeviceMid == deviceMid {
			for _, p := range s.loadProxyProfiles() {
				if p.BoundTo == a.ID && !p.Paused {
					if u, err := url.Parse(p.URL); err == nil {
						return u
					}
				}
			}
			return nil
		}
	}
	return nil
}

// bindAccount pins proxy profile pid to the account.
func (s *Server) bindAccount(accID, pid int64) {
	list := s.loadProxyProfiles()
	for i := range list {
		if list[i].ID == pid {
			list[i].BoundTo = accID
		}
	}
	s.saveProxyProfiles(list)
}

// bindAccountPool clears any binding: the account uses the sticky pool.
func (s *Server) bindAccountPool(accID int64) {
	list := s.loadProxyProfiles()
	for i := range list {
		if list[i].BoundTo == accID {
			list[i].BoundTo = 0
		}
	}
	s.saveProxyProfiles(list)
}

// unbindAccount = same as pool (direct = no egress line; pool is the default path).
func (s *Server) unbindAccount(accID int64) { s.bindAccountPool(accID) }
