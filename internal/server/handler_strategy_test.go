package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

func TestChatRegionSchedulingAndStickyPriority(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "x", AccessToken: "x", ExpiresAt: 9999999999, Domain: "workbuddy.ai"},
		&auth.Auth{UID: "y", AccessToken: "y", ExpiresAt: 9999999999, Domain: "workbuddy.ai"},
	)
	p.SetScheduling(pool.RegionStrategies{Domestic: pool.RoundRobin, International: pool.SequentialExhaust}, pool.RegionAccountOrder{})
	p.SetCostExploreInterval(0)
	// 固定同层，避免成功后成本首观测影响本测试的路由断言。
	for _, uid := range []string{"a", "b", "x", "y"} {
		p.NoteModelCost(uid, "m", 0, 1000)
	}
	selected := ""
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		selected = strings.TrimPrefix(authz, "Bearer ")
		return 200, sseOK, true
	})
	sess := session.New(session.Config{TTL: time.Minute, Available: p.AvailableUIDs})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	for _, tc := range []struct{ model, conv, want string }{
		{"cn:m", "cn-one", "a"},
		{"cn:m", "cn-two", "b"},
		{"global:m", "global-one", "x"},
		{"global:m", "global-two", "x"},
		{"cn:m", "cn-two", "b"}, // 已绑定 b，轮询下一号 a 不抢占。
		{"cn:m", "cn-three", "a"},
	} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"messages":[],"metadata":{"conversation_id":%q}}`, tc.model, tc.conv)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 || selected != tc.want {
			t.Fatalf("%+v: status=%d selected=%s body=%s", tc, rec.Code, selected, rec.Body.String())
		}
	}
	p.CooldownSoftForModel("x", time.Hour, time.Now().Add(time.Hour), "m", "6004")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:m","messages":[],"metadata":{"conversation_id":"global-one"}}`)))
	if rec.Code != 200 || selected != "y" {
		t.Fatalf("cooled sticky: status=%d selected=%s", rec.Code, selected)
	}
}
