package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunAutoAllClaimsDoneButUnclaimed issue #174 顺带项：runAutoAll 遇到
// 「已达标但未领取」的任务时，必须先把奖补领掉再标记 skipped——真实对话等
// 链路点亮的任务不经过任何自动动作，若只 skip 不领，积分永远卡在半路。
func TestRunAutoAllClaimsDoneButUnclaimed(t *testing.T) {
	old := reportGap
	reportGap = 0 // 项间节流在测试里无意义，避免拖慢
	t.Cleanup(func() { reportGap = old })

	var claims int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			// 唯一存在且已达标（5/5）未完成领取的任务；accept_status=accepted，
			// 因此阶段 0 不会再产生 accept 请求。
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "chat_5", "title": "对话 5 次", "target": 5, "current": 5,
						"accept_status": "accepted", "reward_credit": 100, "reward_energy": 5},
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			atomic.AddInt32(&claims, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"already_claimed": false, "credit": 100, "energy": 5},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	out := p.runAutoAll(&auth.Auth{AccessToken: "at", UID: "u1"})

	var hit map[string]any
	for _, item := range out {
		if item["task_code"] == "chat_5" {
			hit = item
		}
	}
	if hit == nil {
		t.Fatalf("runAutoAll 未处理 chat_5，out=%v", out)
	}
	if got := atomic.LoadInt32(&claims); got != 1 {
		t.Fatalf("claim 调用次数 = %d, want 1（达标未领必须补领）", got)
	}
	if hit["claimed"] != true {
		t.Errorf("claimed = %v, want true；out=%+v", hit["claimed"], hit)
	}
	if hit["credit"] != int64(100) || hit["energy"] != int64(5) {
		t.Errorf("credit/energy = %v/%v, want 100/5", hit["credit"], hit["energy"])
	}
	// 仍是 skipped（进度早已达标），但文案要体现补领结果。
	if hit["status"] != "skipped" {
		t.Errorf("status = %v, want skipped", hit["status"])
	}
	if msg, _ := hit["message"].(string); !strings.Contains(msg, "已补领") {
		t.Errorf("message = %q, 应含「已补领」", msg)
	}
}

// TestRunAutoAllSkipsClaimedTask 已领取的任务不再触发领奖请求（幂等入口不应
// 无意义地打上游）。
func TestRunAutoAllSkipsClaimedTask(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	var claims int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "chat_5", "target": 5, "current": 5, "accept_status": "claimed"},
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			atomic.AddInt32(&claims, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	p.runAutoAll(&auth.Auth{AccessToken: "at", UID: "u1"})
	if got := atomic.LoadInt32(&claims); got != 0 {
		t.Fatalf("已领取任务不应再打领奖端点，got %d", got)
	}
}
