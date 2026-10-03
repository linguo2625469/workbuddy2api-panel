package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type autoTaskTestState struct {
	mu           sync.Mutex
	current      int64
	target       int64
	acceptStatus string
	listCalls    int
	failListAt   int
	claimError   bool
	claimCalls   int
	requests     int
}

func (s *autoTaskTestState) counters() (listCalls, claimCalls, requests int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.claimCalls, s.requests
}

func newAutoTaskTestPanel(t *testing.T, current, target int64, acceptStatus string) (*Panel, *auth.Auth, *autoTaskTestState) {
	t.Helper()
	state := &autoTaskTestState{current: current, target: target, acceptStatus: acceptStatus}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.requests++
		if strings.HasSuffix(r.URL.Path, "/claim") || strings.Contains(r.URL.Path, "/reward/claim") {
			state.claimCalls++
			fail := state.claimError
			state.mu.Unlock()
			if fail {
				http.Error(w, "upstream claim rejected", http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"credit":100,"energy":0}}`)
			return
		}
		if r.URL.Path == "/v2/activity/growth/tasks" {
			state.listCalls++
			fail := state.failListAt > 0 && state.listCalls >= state.failListAt
			current, target, status := state.current, state.target, state.acceptStatus
			state.mu.Unlock()
			if fail {
				http.Error(w, "upstream list failed", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"chat_5","title":"对话5次","target":%d,"current":%d,"accept_status":%q}]}}`, target, current, status)
			return
		}
		state.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	oldGap := claimPollGap
	claimPollGap = 0
	t.Cleanup(func() { claimPollGap = oldGap })
	client := upstream.New()
	client.HTTP = &http.Client{Timeout: 2 * time.Second}
	client.ChatBaseCN, client.WebBaseCN, client.BillingBaseCN = server.URL, server.URL, server.URL
	p := New(Config{Upstream: client})
	a := &auth.Auth{UID: "autotask-test", AccessToken: "token"}
	return p, a, state
}

func TestExecuteAutoTaskDoesNotMarkUncreditedProgressDone(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 0, 5, "accepted")
	actions := 0
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "action returned 200", nil
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "awaiting_progress" {
		t.Fatalf("status = %q, want awaiting_progress (progress %s)", out.Status, out.ProgressAfter)
	}
	if out.ProgressAfter != "0/5" {
		t.Fatalf("progress_after = %q, want 0/5", out.ProgressAfter)
	}
	if actions != 1 {
		t.Fatalf("action calls = %d, want 1", actions)
	}
	_, claims, _ := state.counters()
	if claims != 0 {
		t.Fatalf("claim calls = %d, want 0", claims)
	}
}

func TestExecuteAutoTaskClaimsCompletedUnclaimedWithoutRunningAction(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	actions := 0
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "must not run", nil
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "done" || !out.Claimed {
		t.Fatalf("outcome = %#v, want done and claimed", out)
	}
	if actions != 0 {
		t.Fatalf("action calls = %d, want 0", actions)
	}
	_, claims, _ := state.counters()
	if claims != 1 {
		t.Fatalf("claim calls = %d, want 1", claims)
	}
}

func TestExecuteAutoTaskReportsClaimFailureAsPending(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	state.claimError = true
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		t.Fatal("completed task action must not run")
		return "", nil
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "claim_pending" {
		t.Fatalf("status = %q, want claim_pending; outcome=%#v", out.Status, out)
	}
	if strings.TrimSpace(out.ClaimError) == "" || !strings.Contains(out.Message, out.ClaimError) {
		t.Fatalf("claim failure is not readable: %#v", out)
	}
	_, claims, _ := state.counters()
	if claims != 1 {
		t.Fatalf("claim calls = %d, want 1", claims)
	}
}

func TestExecuteAutoTaskActionErrorIsNotDone(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 0, 5, "accepted")
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		return "", fmt.Errorf("action failed")
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "error" || !strings.Contains(out.Message, "action failed") {
		t.Fatalf("outcome = %#v, want action error status", out)
	}
	listCalls, claims, _ := state.counters()
	if listCalls != 1 || claims != 0 {
		t.Fatalf("list calls=%d claim calls=%d, want 1 and 0", listCalls, claims)
	}
}

func TestExecuteAutoTaskReadErrorIsNotDone(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 0, 5, "accepted")
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		state.mu.Lock()
		state.failListAt = state.listCalls + 1
		state.mu.Unlock()
		return "action returned 200", nil
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "error" || !strings.Contains(out.Message, "回读进度失败") {
		t.Fatalf("outcome = %#v, want read error status", out)
	}
	_, claims, _ := state.counters()
	if claims != 0 {
		t.Fatalf("claim calls = %d, want 0", claims)
	}
}

func TestExecuteAutoTaskDoesNotRepeatClaimedTask(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "claimed")
	actions := 0
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "must not run", nil
	}}

	out := p.executeAutoTask(a, act)
	if out.Status != "done" || !out.Claimed {
		t.Fatalf("outcome = %#v, want already claimed done", out)
	}
	_, claims, _ := state.counters()
	if actions != 0 || claims != 0 {
		t.Fatalf("action calls=%d claim calls=%d, want both 0", actions, claims)
	}
}

func TestAutoTaskGlobalWriteEntryPointsDoNotCallUpstream(t *testing.T) {
	state := &autoTaskTestState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.requests++
		state.mu.Unlock()
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	a := &auth.Auth{UID: "global-test", Domain: "api.workbuddy.ai", AccessToken: "token"}
	accounts := pool.New("")
	accounts.Add(a)
	client := upstream.New()
	client.HTTP = &http.Client{Timeout: time.Second}
	client.ChatBaseCN, client.WebBaseCN, client.BillingBaseCN = server.URL, server.URL, server.URL
	p := New(Config{Pool: accounts, Upstream: client})

	for _, handler := range []http.HandlerFunc{p.accountTaskAuto, p.accountTaskAutoAll} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"task_code":"chat_5"}`))
		r.SetPathValue("uid", a.UID)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), globalTaskWriteMessage) {
			t.Fatalf("response = %d %s, want 501 with global task explanation", w.Code, w.Body.String())
		}
	}
	results := p.runAutoAll(a)
	if len(results) != 1 || results[0]["status"] != "error" {
		t.Fatalf("runAutoAll(global) = %#v, want one guarded error", results)
	}
	_, _, requests := state.counters()
	if requests != 0 {
		t.Fatalf("upstream requests = %d, want 0", requests)
	}
}

func TestRunAutoAllDoesNotMarkUncreditedTaskDone(t *testing.T) {
	p, a, _ := newAutoTaskTestPanel(t, 0, 5, "accepted")
	oldAction, oldGap := autoActions[0], reportGap
	autoActions[0].run = func(*Panel, *auth.Auth) (string, error) { return "action returned 200", nil }
	reportGap = 0
	t.Cleanup(func() {
		autoActions[0] = oldAction
		reportGap = oldGap
	})

	found := false
	for _, item := range p.runAutoAll(a) {
		if item["task_code"] == "chat_5" && item["status"] == "done" {
			t.Fatalf("0/5 task was reported done: %#v", item)
		}
		if item["task_code"] == "chat_5" {
			found = true
			if item["status"] != "awaiting_progress" {
				t.Fatalf("chat_5 result = %#v, want awaiting_progress", item)
			}
		}
	}
	if !found {
		t.Fatal("runAutoAll did not report chat_5")
	}
}

func TestRunAutoAllClaimsCompletedUnclaimedTaskWithoutAction(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	actions := 0
	oldAction := autoActions[0]
	autoActions[0].run = func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "must not run", nil
	}
	t.Cleanup(func() { autoActions[0] = oldAction })

	found := false
	for _, item := range p.runAutoAll(a) {
		if item["task_code"] == "chat_5" {
			found = true
			if item["status"] != "done" || item["claimed"] != true {
				t.Fatalf("chat_5 result = %#v, want done and claimed", item)
			}
		}
	}
	if !found {
		t.Fatal("runAutoAll did not report chat_5")
	}
	_, claims, _ := state.counters()
	if actions != 0 || claims != 1 {
		t.Fatalf("action calls=%d claim calls=%d, want 0 and 1", actions, claims)
	}
}
