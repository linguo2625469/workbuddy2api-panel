package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compensationRecorder 记录上游路径，并返回可编排的补偿状态。
type compensationRecorder struct {
	mu sync.Mutex
	// statusBody 是 compensation-status 返回的 data 部分；空则用默认的「待领取 500c」。
	statusBody string
	claimHits  int
	paths      []string
}

func (r *compensationRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		switch {
		case strings.HasSuffix(req.URL.Path, "/compensation-status"):
			body := r.statusBody
			if body == "" {
				body = `{"active":true,"claimed":false,"credit_num":500,"end_time":"2026-10-22 23:59:59"}`
			}
			r.mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":` + body + `}`))
		case strings.HasSuffix(req.URL.Path, "/claim-compensation"):
			r.claimHits++
			r.mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":500}}`))
		default:
			r.mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *compensationRecorder) claims() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claimHits
}

func (r *compensationRecorder) pathsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

// compensationScheduler 构造带指定账号的调度器。
func compensationScheduler(t *testing.T, rec *compensationRecorder, accts ...*auth.Auth) *Scheduler {
	t.Helper()
	srv := rec.server(t)
	p := pool.New("")
	for _, a := range accts {
		p.Add(a)
	}
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return New(Config{Pool: p, Upstream: up})
}

// TestCompensationStatusAllKeepsPoolOrder 钉住行序（S10）。
//
// 这条不是为了好看：面板按返回顺序渲染「官方补偿」卡片，若顺序随机，同一批账号
// 每轮刷新都在换位置，用户对不上哪行是自己那个号。实现上必须**按池顺序**返回，
// 与 Pool.List()（按 UID 稳定排序）一致——并发查询只能在各自下标上写，不能靠
// 完成顺序 append。
func TestCompensationStatusAllKeepsPoolOrder(t *testing.T) {
	rec := &compensationRecorder{}
	s := compensationScheduler(t, rec,
		&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "甲", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", Nickname: "乙", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", Nickname: "丙", ExpiresAt: 9999999999},
	)

	want := []string{"u1", "u2", "u3"} // Pool.List() 按 UID 排序
	for i := 0; i < 20; i++ {          // 多跑几轮：单轮撞对的概率掩盖不了竞态
		rows := s.CompensationStatusAll()
		if len(rows) != len(want) {
			t.Fatalf("行数=%d want %d", len(rows), len(want))
		}
		for j, uid := range want {
			if rows[j].UID != uid {
				t.Fatalf("第 %d 轮第 %d 行 uid=%s want %s（顺序漂移）", i, j, rows[j].UID, uid)
			}
		}
	}
}

// TestCompensationSkippedRowsCarryNickname 跳过行必须带昵称（S9 相关）。
//
// 跳过行不参与上游查询，若不返回昵称，面板只能显示截断的 UID，用户根本认不出
// 是哪个号被门控挡掉了。
func TestCompensationSkippedRowsCarryNickname(t *testing.T) {
	rec := &compensationRecorder{}
	ent := &auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "企业号", ExpiresAt: 9999999999}
	ent.EnterpriseID = "ent-1"
	s := compensationScheduler(t, rec,
		&auth.Auth{UID: "u2", AccessToken: "at2", Nickname: "个人号", ExpiresAt: 9999999999},
		ent,
	)

	rows := s.CompensationStatusAll()
	if len(rows) != 2 {
		t.Fatalf("行数=%d want 2", len(rows))
	}
	for i := range rows {
		if rows[i].UID != "u1" {
			continue
		}
		if rows[i].Skip != compSkipEnterprise {
			t.Errorf("企业号 skip=%q want %q", rows[i].Skip, compSkipEnterprise)
		}
		if rows[i].Nickname != "企业号" {
			t.Errorf("跳过行昵称=%q want 企业号（面板靠它显示账号）", rows[i].Nickname)
		}
		if got := rec.pathsSnapshot(); len(got) != 1 {
			t.Errorf("企业号不应发起上游查询，实际请求=%v（个人号 1 次才对）", got)
		}
	}
}

// TestEnterpriseGateSkipsCompensationEntries 企业版在**面板直接触发的三个新入口**
// 上同样不得发起任何上游调用。
//
// 与 TestEnterpriseGateSkipsGrowthTasks 互补：那个用例覆盖五类排程任务，本用例
// 覆盖面板按钮路径——门控写在一处不够，两个入口都要钉住。
func TestEnterpriseGateSkipsCompensationEntries(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Scheduler)
	}{
		{"批量状态查询", func(s *Scheduler) { s.CompensationStatusAll() }},
		{"批量领取", func(s *Scheduler) { s.ClaimCompensationNow() }},
		{"单号领取", func(s *Scheduler) {
			if _, err := s.ClaimCompensationFor("u1"); err != nil {
				t.Errorf("单号领取不该报错: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 企业号
			recEnt := &compensationRecorder{}
			ent := &auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "企业号", ExpiresAt: 9999999999}
			ent.EnterpriseID = "ent-1"
			c.run(compensationScheduler(t, recEnt, ent))
			if got := recEnt.pathsSnapshot(); len(got) != 0 {
				t.Errorf("企业号不应发起任何上游调用，实际=%v", got)
			}

			// 个人号对照组：必须确实有动作，否则「没发请求」可能只是构造有误。
			recPer := &compensationRecorder{}
			c.run(compensationScheduler(t, recPer,
				&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "个人号", ExpiresAt: 9999999999}))
			if len(recPer.pathsSnapshot()) == 0 {
				t.Errorf("个人号对照应有上游调用，实际=%v", recPer.pathsSnapshot())
			}
		})
	}
}

// TestClaimCompensationForRejectsDisabled 单号入口必须挡住已禁用账号，
// 否则出现「批量按钮跳过禁用号、手动按钮却能领」的割裂。
func TestClaimCompensationForRejectsDisabled(t *testing.T) {
	rec := &compensationRecorder{}
	s := compensationScheduler(t, rec,
		&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "禁用号", ExpiresAt: 9999999999})

	s.cfg.Pool.Disable("u1", "测试禁用") // 置为 disabled

	res, err := s.ClaimCompensationFor("u1")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res.Skip != compSkipDisabled {
		t.Errorf("skip=%q want %q", res.Skip, compSkipDisabled)
	}
	if rec.claims() != 0 {
		t.Errorf("禁用号不应被领取，实际 claim 调用 %d 次", rec.claims())
	}
	if got := rec.pathsSnapshot(); len(got) != 0 {
		t.Errorf("禁用号不应发起任何上游调用，实际=%v", got)
	}
}

// TestClaimCompensationOnlyWhenPending 「先查后领」的核心判据：只有
// active && !claimed && credit_num>0 才真的调领取接口。
func TestClaimCompensationOnlyWhenPending(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantClaim bool
		wantSkip  string
	}{
		{"待领取", `{"active":true,"claimed":false,"credit_num":500}`, true, ""},
		{"已领取", `{"active":true,"claimed":true,"credit_num":500}`, false, compSkipClaimed},
		{"无活动", `{"active":false,"claimed":false,"credit_num":0}`, false, compSkipInactive},
		{"额度为零", `{"active":true,"claimed":false,"credit_num":0}`, false, compSkipZeroCredit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &compensationRecorder{statusBody: tc.body}
			s := compensationScheduler(t, rec,
				&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "甲", ExpiresAt: 9999999999})

			res := s.claimOne(s.cfg.Pool.AuthByUID("u1"))
			if tc.wantClaim {
				if !res.Claimed || res.Credit != 500 {
					t.Errorf("应领取 500c，实际 claimed=%v credit=%d", res.Claimed, res.Credit)
				}
				if rec.claims() != 1 {
					t.Errorf("应调用领取 1 次，实际 %d 次", rec.claims())
				}
			} else {
				if res.Claimed {
					t.Error("不应标记为已领取")
				}
				if res.Skip != tc.wantSkip {
					t.Errorf("skip=%q want %q", res.Skip, tc.wantSkip)
				}
				if rec.claims() != 0 {
					t.Errorf("不应调用领取接口，实际 %d 次", rec.claims())
				}
			}
		})
	}
}

// TestSummarizeCompensationDistinguishesError 汇总必须把「查询失败」与
// 「没有可领」分开统计——这是本 PR 最重要的一条语义：上游全挂时每个账号都走
// error 分支，若只按 claimed 计数，调用方会把「一个都没查成」说成「没有待领补偿」。
func TestSummarizeCompensationDistinguishesError(t *testing.T) {
	cases := []struct {
		name    string
		results []CompensationResult
		want    CompensationSummary
	}{
		{
			"全部查询失败",
			[]CompensationResult{
				{UID: "u1", Error: "boom"},
				{UID: "u2", Error: "boom"},
			},
			CompensationSummary{Total: 2, Errored: 2},
		},
		{
			"无待领（已领完）",
			[]CompensationResult{
				{UID: "u1", Skip: compSkipClaimed},
				{UID: "u2", Skip: compSkipClaimed},
			},
			CompensationSummary{Total: 2, Skipped: 2},
		},
		{
			"混合：两个领到、一个失败",
			[]CompensationResult{
				{UID: "u1", Claimed: true, Credit: 500},
				{UID: "u2", Claimed: true, Credit: 500},
				{UID: "u3", Error: "boom"},
			},
			CompensationSummary{Total: 3, Claimed: 2, Errored: 1, Credit: 1000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeCompensation(tc.results)
			if got != tc.want {
				t.Errorf("汇总=%+v want %+v", got, tc.want)
			}
			// 全失败时 OK() 必须为 false，否则调用方会当成「一切正常，只是没活动」。
			wantOK := tc.want.Errored == 0
			if got.OK() != wantOK {
				t.Errorf("OK()=%v want %v（errored=%d）", got.OK(), wantOK, got.Errored)
			}
		})
	}
}
