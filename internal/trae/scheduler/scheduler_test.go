package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// fakeUpstream 同时模拟 checkin/ent_usage/refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	claimCalls     atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"checked_in":false,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			f.claimCalls.Add(1)
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":` +
				jsonI64(f.resourceRemain) + `}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestScheduler(f *fakeUpstream, p *pool.Pool, srv *httptest.Server) *Scheduler {
	up := &upstream.Client{
		HTTP:      srv.Client(),
		AgentHost: srv.URL,
		UgHost:    srv.URL,
		OAuthHost: srv.URL,
		ClientID:  upstream.ClientID,
	}
	return New(Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  9,
		RefreshHours: []int{3},
		RefreshSkew:  time.Hour,
	})
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin status calls=%d", f.checkinCalls.Load())
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim calls=%d", f.claimCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunCheckinSkipsDisabled(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 0 {
		t.Errorf("disabled account should be skipped, calls=%d", f.checkinCalls.Load())
	}
}

func TestRunRefreshRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "newat" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunRefreshSkipsFreshToken(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "fresh", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: srv.URL})

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 0 {
		t.Errorf("fresh token should not refresh, calls=%d", f.refreshCalls.Load())
	}
}

func TestRunRefreshSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":20101,"msg":"login required"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	s := New(Config{Pool: p, Upstream: up})
	s.RunRefreshNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}
