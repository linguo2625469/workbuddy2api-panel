package upstream

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type wrapRT struct {
	next http.RoundTripper
}

func (w wrapRT) RoundTrip(req *http.Request) (*http.Response, error) {
	return w.next.RoundTrip(req)
}

type closeIdleSpyRT struct {
	closed atomic.Bool
}

func (s *closeIdleSpyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, context.Canceled
}

func (s *closeIdleSpyRT) CloseIdleConnections() {
	s.closed.Store(true)
}

func TestResinRoutesAccountRequestAndKeepsIdentity(t *testing.T) {
	var gotPath, gotAccount string
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAccount = r.URL.RequestURI(), r.Header.Get(resinAccountHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer resin.Close()
	c := New()
	if err := c.ConfigureResin(resin.URL+"/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "account-1"}
	req, _ := http.NewRequest(http.MethodGet, "https://copilot.tencent.com/v2/models?x=1", nil)
	c.CommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/secret/workbuddy2api/https/copilot.tencent.com/v2/models?x=1" {
		t.Fatalf("proxy path = %q", gotPath)
	}
	if gotAccount != "account-1" {
		t.Fatalf("resin account = %q", gotAccount)
	}
}

func TestResinLeavesNonAccountRequestDirect(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer direct.Close()
	c := New()
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTP.Get(direct.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestResinBlocksAuthenticatedRequestWithoutAccountHeader(t *testing.T) {
	var hitDirect atomic.Bool
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitDirect.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer direct.Close()

	c := New()
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, direct.URL+"/v2/models", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	if _, err := c.HTTP.Do(req); err == nil {
		t.Fatal("expected error for authenticated request missing X-Resin-Account")
	}
	if hitDirect.Load() {
		t.Fatal("authenticated request without X-Resin-Account must not fall back to direct egress")
	}
}

func TestResinEnabledSurvivesTransportWrapping(t *testing.T) {
	var gotAccount string
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount = r.Header.Get(resinAccountHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer resin.Close()

	c := New()
	if err := c.ConfigureResin(resin.URL+"/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	// Simulate downstream middleware wrapping Transport after ConfigureResin.
	c.HTTP.Transport = wrapRT{next: c.HTTP.Transport}

	a := &auth.Auth{UID: "account-wrapped", AccessToken: "tok"}
	req, _ := http.NewRequest(http.MethodGet, "https://copilot.tencent.com/v2/models", nil)
	c.CommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAccount != "account-wrapped" {
		t.Fatalf("resin account = %q, want account-wrapped", gotAccount)
	}
}

func TestResinBillingAndProfileDoNotInjectDeviceHeaders(t *testing.T) {
	c := New()
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "account-1", AccessToken: "tok"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/billing/meter/daily-checkin", nil)
	c.BillingHeaders(req, a)
	if got := req.Header.Get(resinAccountHeader); got != "account-1" {
		t.Fatalf("billing X-Resin-Account = %q, want account-1", got)
	}
	if got := req.Header.Get("X-Machine-ID"); got != "" {
		t.Fatalf("billing unexpectedly injected X-Machine-ID = %q", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != "" {
		t.Fatalf("billing unexpectedly injected X-Session-ID = %q", got)
	}
}

func TestResinCoversV3ConfigAndWebEndpoints(t *testing.T) {
	var seen []string
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(resinAccountHeader)+":"+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"data":{"models":[{"id":"m1","name":"m1"}],"credit":10,"energy":1}}`))
	}))
	defer resin.Close()

	c := New()
	c.ChatBaseCN = "https://copilot.tencent.com"
	c.WebBaseCN = "https://www.workbuddy.cn"
	if err := c.ConfigureResin(resin.URL+"/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "u-resin", AccessToken: "tok"}
	if _, err := c.fetchV3ConfigModelMap(a, ""); err != nil {
		t.Fatalf("fetchV3ConfigModelMap: %v", err)
	}
	if _, _, err := c.ClaimReward(a, "task-1"); err != nil {
		t.Fatalf("ClaimReward: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("seen = %v, want 2 Resin-routed calls", seen)
	}
	if seen[0] != "u-resin:/secret/workbuddy2api/https/copilot.tencent.com/v3/config" {
		t.Fatalf("v3/config path = %q", seen[0])
	}
	if seen[1] != "u-resin:/secret/workbuddy2api/https/www.workbuddy.cn/activity/growth/tasks/task-1/claim" {
		t.Fatalf("claim path = %q", seen[1])
	}
}

func TestResinPreservesDNSDialerAndCloseIdleConnections(t *testing.T) {
	c := New()
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	var customDialCalled atomic.Bool
	dnsDialer := NewDNSDialer([]string{"127.0.0.1:1"})
	// Verify SetDNSDialer replaces DialContext on the underlying *http.Transport even after ConfigureResin.
	prevTr := unwrapHTTPTransport(c.HTTP.Transport)
	prevTr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		customDialCalled.Store(true)
		return nil, context.Canceled
	}
	c.SetDNSDialer(dnsDialer)
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/models", nil)
	_, _ = c.HTTP.Do(req)
	if customDialCalled.Load() {
		t.Fatal("SetDNSDialer did not replace DialContext on wrapped *http.Transport")
	}

	spy := &closeIdleSpyRT{}
	c.HTTP.Transport = spy
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	roundTripCloseIdle(c.HTTP.Transport)
	if !spy.closed.Load() {
		t.Fatal("roundTripCloseIdle did not forward CloseIdleConnections through resinRoundTripper")
	}
}
