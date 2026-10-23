package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newKeepaliveSched 构造带单个账号 + 可控上游的调度器，返回 (s, p, a, refreshCalls)。
// expiresAt: 账号的 ExpiresAt；window: 周期保活临期窗口（<0 表示不过滤）。
func newKeepaliveSched(t *testing.T, expiresAt int64, window time.Duration) (*Scheduler, *pool.Pool, *auth.Auth, *atomic.Int32) {
	t.Helper()
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: expiresAt}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRefreshWindow: window})
	return s, p, a, &refreshCalls
}

// TestScheduledKeepaliveSkipsFreshToken 排程保活只刷新临期窗口内的账号：
// token 还很新鲜（距过期 30 天，窗口 7 天）→ 排程路径不刷新；手动入口照常全量。
func TestScheduledKeepaliveSkipsFreshToken(t *testing.T) {
	fresh := time.Now().Add(30 * 24 * time.Hour).Unix()
	s, _, a, calls := newKeepaliveSched(t, fresh, 7*24*time.Hour)

	s.runKeepaliveScheduled()
	if calls.Load() != 0 {
		t.Fatalf("临期窗口外账号不应被排程保活刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "old" {
		t.Errorf("token 不应被改动: %q", a.AccessToken)
	}

	// 手动全量保活语义不变：同一账号立即刷新。
	s.RunKeepaliveNow()
	if calls.Load() != 1 {
		t.Errorf("手动保活应全量刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("手动保活后 token 应更新: %q", a.AccessToken)
	}
}

// TestScheduledKeepaliveRefreshesWithinWindow 窗口内（剩余 3 天 < 窗口 7 天）→ 排程刷新。
func TestScheduledKeepaliveRefreshesWithinWindow(t *testing.T) {
	soon := time.Now().Add(3 * 24 * time.Hour).Unix()
	s, _, a, calls := newKeepaliveSched(t, soon, 7*24*time.Hour)

	s.runKeepaliveScheduled()
	if calls.Load() != 1 {
		t.Fatalf("窗口内账号应被刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token 应更新: %q", a.AccessToken)
	}
}

// TestScheduledKeepaliveExpiryMissingOrExpired 无 expiry / 已过期的账号必须被排程刷新
// （NeedsRefresh 的兜底语义，避免「不知道何时过期」的号被窗口漏掉）。
func TestScheduledKeepaliveExpiryMissingOrExpired(t *testing.T) {
	for name, exp := range map[string]int64{
		"missing":  0,
		"negative": -1,
		"expired":  time.Now().Add(-time.Hour).Unix(),
	} {
		t.Run(name, func(t *testing.T) {
			s, _, a, calls := newKeepaliveSched(t, exp, 7*24*time.Hour)
			s.runKeepaliveScheduled()
			if calls.Load() != 1 {
				t.Errorf("expiry=%d 应被排程刷新: calls=%d", exp, calls.Load())
			}
			if a.AccessToken != "new" {
				t.Errorf("token 应更新: %q", a.AccessToken)
			}
		})
	}
}

// TestScheduledKeepaliveNegativeWindowNoFilter 负窗口 = 不过滤（退化为全量，与手动同语义）。
func TestScheduledKeepaliveNegativeWindowNoFilter(t *testing.T) {
	fresh := time.Now().Add(30 * 24 * time.Hour).Unix()
	s, _, _, calls := newKeepaliveSched(t, fresh, -1)

	s.runKeepaliveScheduled()
	if calls.Load() != 1 {
		t.Errorf("负窗口应全量刷新: calls=%d", calls.Load())
	}
}

// TestScheduledKeepaliveDefaultWindowWhenUnset 未配置窗口（0）时 New 回落 7 天，
// 且新鲜账号被排程路径跳过（证明默认窗口真的生效）。
func TestScheduledKeepaliveDefaultWindowWhenUnset(t *testing.T) {
	s, _, _, _ := newKeepaliveSched(t, time.Now().Add(30*24*time.Hour).Unix(), 0)
	if got := s.keepaliveRefreshWindow(); got != 7*24*time.Hour {
		t.Fatalf("未配置时窗口应回落 7 天: got=%v", got)
	}
}

// TestScheduledKeepaliveSessionDeadThreshold 排程路径同样保留「连续 3 次 12153 才禁用」
// 的保护（临期过滤不改变 session_dead 计数语义）。
func TestScheduledKeepaliveSessionDeadThreshold(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	// 已过期 → 落在窗口内，排程路径每次都会尝试刷新。
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRefreshWindow: 7 * 24 * time.Hour})

	s.runKeepaliveScheduled()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.runKeepaliveScheduled()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	s.runKeepaliveScheduled()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
	if calls.Load() != 3 {
		t.Errorf("上游调用次数=%d want 3", calls.Load())
	}
}
