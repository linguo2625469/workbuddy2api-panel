package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sweepTask 构造假上游的一个任务条目。
func sweepTask(code string, target, current int64, acceptStatus string, extra map[string]any) map[string]any {
	t := map[string]any{
		"task_code": code, "title": code, "target": target,
		"current": current, "accept_status": acceptStatus,
		"reward_credit": 100, "reward_energy": 5,
	}
	for k, v := range extra {
		t[k] = v
	}
	return t
}

// sweepBackend 假上游：默认任务列表 + mp 列表 + accept + claim，带请求计数。
type sweepBackend struct {
	listFn   func(isMP bool) []map[string]any
	acceptFn func(codes []string, isMP bool) (int, string) // (status, body)
	claimFn  func(code string) (int, map[string]any)       // (status, data)

	reqs    atomic.Int32
	accepts atomic.Int32
	claims  atomic.Int32
}

func sweepTasksBody(tasks []map[string]any) string {
	b, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
	return string(b)
}

func (b *sweepBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.reqs.Add(1)
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			if b.listFn != nil {
				w.Write([]byte(sweepTasksBody(b.listFn(isMP))))
				return
			}
			w.Write([]byte(sweepTasksBody(nil)))
		case strings.HasSuffix(r.URL.Path, "/accept"):
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for range body.TaskCodes {
				b.accepts.Add(1)
			}
			deferFN := func() {
				w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
			}
			if b.acceptFn == nil {
				deferFN()
				return
			}
			if status, msg := b.acceptFn(body.TaskCodes, isMP); status != 0 {
				w.WriteHeader(status)
				w.Write([]byte(msg))
				return
			}
			deferFN()
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			b.claims.Add(1)
			if b.claimFn == nil {
				w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
				return
			}
			status, data := b.claimFn(code)
			if status != 0 {
				w.WriteHeader(status)
			}
			if data == nil {
				data = map[string]any{"already_claimed": true}
			}
			out, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": data})
			w.Write(out)
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}
}

// newSweepPanel 用给定假后端与账号搭一个可被 RunTaskSweepOnce 直接调用的 Panel。
func newSweepPanel(t *testing.T, b *sweepBackend, accounts ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	return &Panel{cfg: Config{
		Pool: p,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}
}

func claimOK(data map[string]any) (int, map[string]any) { return 0, data }

// TestTaskSweepAcceptAndClaim 单轮清扫：未接受的任务走 accept（默认/mp 分开提交）、
// 已达标的走 claim，其余（已领取 / locked / 未达标）不产生任何请求。
func TestTaskSweepAcceptAndClaim(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return []map[string]any{
					sweepTask("school_season", 1, 1, "accepted", nil), // mp 口径达标任务
				}
			}
			return []map[string]any{
				sweepTask("chat_5", 5, 5, "accepted", nil),       // 达标未领 → claim
				sweepTask("invite_1", 1, 0, "not_accepted", nil), // 未接受 → accept
				sweepTask("locked_1", 3, 0, "not_accepted", map[string]any{"locked": true}),
				sweepTask("halfway", 10, 3, "accepted", nil), // 未达标 → 不动
				sweepTask("done_1", 5, 5, "claimed", nil),    // 已领取 → 不动
			}
		},
		claimFn: func(code string) (int, map[string]any) {
			return claimOK(map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()

	if st.Accounts != 1 || st.Skipped != 0 {
		t.Fatalf("accounts=%d skipped=%d, want 1/0", st.Accounts, st.Skipped)
	}
	// invite_1 需要 accept（mp 列表里的 school_season 已 accepted，不再接受）。
	if st.Accepted != 1 {
		t.Errorf("accepted = %d, want 1（只有 invite_1 未接受）", st.Accepted)
	}
	// chat_5 与 school_season 均达标未领 → 各领一次。
	if st.Claimed != 2 || st.Credits != 200 || st.Energy != 10 {
		t.Errorf("claimed=%d credits=%d energy=%d, want 2/200/10", st.Claimed, st.Credits, st.Energy)
	}
	if st.Errors != 0 {
		t.Errorf("errors = %d, want 0", st.Errors)
	}
	if got := b.claims.Load(); got != 2 {
		t.Errorf("claim 请求数 = %d, want 2", got)
	}
}

// TestTaskSweepIdempotent 两轮幂等：第二轮任务已被标记为 claimed（不再 Claimable），
// 不应重复打领奖端点；即便上游返回 already_claimed 也不计错误。
func TestTaskSweepIdempotent(t *testing.T) {
	claimed := map[string]bool{}
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			status := "accepted"
			if claimed["chat_5"] {
				status = "claimed"
			}
			return []map[string]any{sweepTask("chat_5", 5, 5, status, nil)}
		},
		claimFn: func(code string) (int, map[string]any) {
			claimed[code] = true
			return claimOK(map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	first := p.RunTaskSweepOnce()
	if first.Claimed != 1 || first.Credits != 100 {
		t.Fatalf("first round: claimed=%d credits=%d, want 1/100", first.Claimed, first.Credits)
	}
	// 第二轮：状态已 claimed → 不再 Claimable → 领奖端点 0 次调用。
	b.claims.Store(0)
	second := p.RunTaskSweepOnce()
	if second.Claimed != 0 || second.Errors != 0 {
		t.Errorf("second round: claimed=%d errors=%d, want 0/0", second.Claimed, second.Errors)
	}
	if got := b.claims.Load(); got != 0 {
		t.Errorf("第二轮不应再打领奖端点，got %d", got)
	}
}

// TestTaskSweepSkipsGlobalAndEnterprise global / 企业版账号无个人成长体系：
// 零上游请求（与任务中心扫描同一门控）。
func TestTaskSweepSkipsGlobalAndEnterprise(t *testing.T) {
	b := &sweepBackend{}
	p := newSweepPanel(t, b,
		&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"},
	)
	st := p.RunTaskSweepOnce()
	if st.Accounts != 0 || st.Errors != 0 || st.Claimed != 0 {
		t.Errorf("stats = %+v, want 全零", st)
	}
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("global/企业版账号不应产生任何上游请求，got %d", got)
	}
}

// TestTaskSweepClaimErrorDoesNotBlock 单个领奖失败只计 errors，不中断后续任务，
// 也不掩盖其他任务的成功。
func TestTaskSweepClaimErrorDoesNotBlock(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return []map[string]any{
				sweepTask("bad_1", 5, 5, "accepted", nil),
				sweepTask("good_1", 5, 5, "accepted", nil),
			}
		},
		claimFn: func(code string) (int, map[string]any) {
			if code == "bad_1" {
				return 400, map[string]any{"code": 400, "msg": "task not completed"}
			}
			return claimOK(map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()
	if st.Errors != 1 {
		t.Errorf("errors = %d, want 1", st.Errors)
	}
	if st.Claimed != 1 || st.Credits != 100 {
		t.Errorf("claimed=%d credits=%d, want 1/100（失败项不阻挡成功项）", st.Claimed, st.Credits)
	}
}

// TestTaskSweepSkipsBusyAccount 账号正在跑任务动作（持有 per-account 锁）时整账号
// 跳过：不打断进行中的链路，下一轮自然补。
func TestTaskSweepSkipsBusyAccount(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.tryLockAccount("u1") {
		t.Fatal("模拟占用失败")
	}
	st := p.RunTaskSweepOnce()
	if st.Skipped != 1 || st.Claimed != 0 {
		t.Errorf("stats = %+v, want skipped=1 claimed=0", st)
	}
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("忙账号不应产生上游请求，got %d", got)
	}
	p.unlockAccount("u1")
	// 解锁后同一轮即可正常清扫（下一周期自动补）。
	st = p.RunTaskSweepOnce()
	if st.Skipped != 0 || st.Claimed != 1 {
		t.Errorf("解锁后 stats = %+v, want skipped=0 claimed=1", st)
	}
}
