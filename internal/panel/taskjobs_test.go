package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type taskJobServerState struct {
	mu         sync.Mutex
	calls      int
	firstOnce  sync.Once
	firstCall  chan struct{}
	release    chan struct{}
	releaseOne sync.Once
	tasksJSON  string
	blockMP    bool
}

func newTaskJobTestPanel(t *testing.T, uid string, realm string, serverState *taskJobServerState) (*Panel, context.CancelFunc, string) {
	t.Helper()
	if serverState == nil {
		serverState = &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverState.mu.Lock()
		serverState.calls++
		serverState.mu.Unlock()
		if r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet {
			if serverState.blockMP && r.Header.Get("X-Client-Platform") != "miniprogram" {
				_, _ = fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"tasks":[]}}`)
				return
			}
			serverState.firstOnce.Do(func() { close(serverState.firstCall) })
			<-serverState.release // Deliberately ignore request cancellation to exercise app shutdown joining.
			w.Header().Set("Content-Type", "application/json")
			tasks := serverState.tasksJSON
			if tasks == "" {
				tasks = "[]"
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"tasks":%s}}`, tasks)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		serverState.open()
		server.Close()
	})

	account := &auth.Auth{UID: uid, Nickname: "task job test", AccessToken: "token"}
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			t.Fatal(err)
		}
	}
	accounts := pool.New("")
	accounts.Add(account)
	client := upstream.New()
	client.HTTP = &http.Client{Timeout: 5 * time.Second}
	client.ChatBaseCN, client.BillingBaseCN, client.WebBaseCN = server.URL, server.URL, server.URL
	p := New(Config{
		Pool:             accounts,
		Upstream:         client,
		AutoTasksEnabled: func() bool { return true },
	})
	appCtx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	if err := p.StartTaskJobs(appCtx, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		p.StopTaskJobs()
	})
	return p, cancel, path
}

func (s *taskJobServerState) open() {
	if s == nil {
		return
	}
	s.releaseOne.Do(func() { close(s.release) })
}

func (s *taskJobServerState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func waitTaskJobRequest(t *testing.T, s *taskJobServerState) {
	t.Helper()
	select {
	case <-s.firstCall:
	case <-time.After(3 * time.Second):
		t.Fatal("background task did not make its first upstream request")
	}
}

func TestAccountTaskAutoAllRespondsImmediatelyAndIgnoresDisconnect(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, cancelApp, _ := newTaskJobTestPanel(t, "task-job-disconnect", "cn", state)
	requestCtx, disconnect := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/task-job-disconnect/tasks/auto_all", nil).WithContext(requestCtx)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var response struct {
		OK      bool    `json:"ok"`
		Started bool    `json:"started"`
		Job     TaskJob `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !response.Started || response.Job.UID != "task-job-disconnect" {
		t.Fatalf("unexpected immediate response: %+v", response)
	}
	waitTaskJobRequest(t, state)
	disconnect()

	duplicate, started, err := p.StartAccountTaskJob(response.Job.UID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if started || duplicate.ID != response.Job.ID {
		t.Fatalf("duplicate start = started %v, job id %q; want false and %q", started, duplicate.ID, response.Job.ID)
	}
	if job, ok := p.GetTaskJob(response.Job.UID); !ok || job.ID != response.Job.ID || job.Status != "running" {
		t.Fatalf("request disconnect canceled the job: ok=%v job=%+v", ok, job)
	}

	cancelApp()
	stopped := make(chan struct{})
	go func() {
		p.StopTaskJobs()
		close(stopped)
	}()
	state.open()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopTaskJobs did not wait for the in-flight upstream call")
	}
	if got := state.count(); got != 1 {
		t.Fatalf("upstream calls after cancellation = %d, want only the in-flight request", got)
	}
	job, ok := p.GetTaskJob(response.Job.UID)
	if !ok || job.Status != "interrupted" {
		t.Fatalf("stopped job = %+v, want interrupted", job)
	}
}

func TestStartAccountTaskJobSharesExistingAccountLock(t *testing.T) {
	p, _, _ := newTaskJobTestPanel(t, "task-job-lock", "cn", nil)
	if !p.tryLockAccount("task-job-lock") {
		t.Fatal("could not take test account lock")
	}
	_, _, err := p.StartAccountTaskJob("task-job-lock", "manual")
	p.unlockAccount("task-job-lock")
	if !errors.Is(err, errTaskJobBusy) {
		t.Fatalf("start with existing task lock error = %v, want errTaskJobBusy", err)
	}
}

func TestStartGlobalTaskJobDoesNotCallUpstream(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, _, _ := newTaskJobTestPanel(t, "task-job-global", "global", state)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/task-job-global/tasks/auto_all", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if got := state.count(); got != 0 {
		t.Fatalf("global task endpoint made %d upstream requests, want 0", got)
	}
	if _, _, err := p.StartAccountTaskJob("task-job-global", "daily"); !errors.Is(err, errTaskJobGlobal) {
		t.Fatalf("global start error = %v, want errTaskJobGlobal", err)
	}
	p.StartNewAccountTasks("task-job-global") // Automatic global accounts are silently ignored.
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("global auto-start created jobs: %+v", jobs)
	}
	if got := state.count(); got != 0 {
		t.Fatalf("global job start made %d upstream requests, want 0", got)
	}
}

func TestPendingTaskJobStopsIfAccountBecomesDisabled(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, _, _ := newTaskJobTestPanel(t, "task-job-disabled", "cn", state)
	m := p.getTaskJobManager()
	m.sem <- struct{}{}
	m.sem <- struct{}{}
	job, started, err := p.StartAccountTaskJob("task-job-disabled", "manual")
	if err != nil || !started {
		t.Fatalf("start = started %v, err %v", started, err)
	}
	p.cfg.Pool.Disable(job.UID, "test disabled")
	<-m.sem // Let the job leave pending and perform its local eligibility check.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := p.GetTaskJob(job.UID)
		if current.Status == "error" {
			if got := state.count(); got != 0 {
				t.Fatalf("disabled pending job made %d upstream requests", got)
			}
			<-m.sem
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pending job did not stop after account was disabled")
}

func TestTaskJobsRecoverInterruptedWorkAndStopBetweenRequests(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, cancelApp, path := newTaskJobTestPanel(t, "task-job-resume", "cn", state)
	now := time.Now().UTC()
	job := TaskJob{
		ID:        "persisted-job-id",
		UID:       "task-job-resume",
		Nickname:  "saved name",
		Realm:     "cn",
		Trigger:   "manual",
		Status:    "running",
		StartedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Second),
		Total:     len(autoActions),
		Completed: 1,
		Results:   []map[string]any{{"task_code": "chat_5", "status": "done", "message": "已领取"}},
	}
	// Recreate the panel/manager so StartTaskJobs exercises disk recovery.
	p.StopTaskJobs()
	cancelApp()
	raw, err := json.Marshal(taskJobFile{Version: 1, Jobs: []TaskJob{job}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := pool.New("")
	accounts.Add(&auth.Auth{UID: job.UID, Nickname: job.Nickname, AccessToken: "token"})
	client := upstream.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.calls++
		state.mu.Unlock()
		if r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet {
			state.firstOnce.Do(func() { close(state.firstCall) })
			<-state.release
			_, _ = fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"tasks":[]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() { state.open(); server.Close() })
	client.HTTP = &http.Client{Timeout: 5 * time.Second}
	client.ChatBaseCN, client.BillingBaseCN, client.WebBaseCN = server.URL, server.URL, server.URL
	restarted := New(Config{Pool: accounts, Upstream: client, AutoTasksEnabled: func() bool { return true }})
	appCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); restarted.StopTaskJobs() })
	if err := restarted.StartTaskJobs(appCtx, path); err != nil {
		t.Fatal(err)
	}
	recovered, ok := restarted.GetTaskJob(job.UID)
	if !ok || recovered.Status != "interrupted" {
		t.Fatalf("recovered job = %+v, want interrupted", recovered)
	}
	if len(recovered.Results) != 1 || recovered.Results[0]["task_code"] != "chat_5" {
		t.Fatalf("recovered result snapshot = %#v, want persisted chat_5 result", recovered.Results)
	}
	// Returned snapshots own their result maps and cannot mutate manager state.
	recovered.Results[0]["status"] = "corrupted"
	again, _ := restarted.GetTaskJob(job.UID)
	if again.Results[0]["status"] != "done" {
		t.Fatalf("mutating returned job changed stored result: %#v", again.Results[0])
	}

	restarted.ResumeTaskJobs()
	waitTaskJobRequest(t, state)
	running, ok := restarted.GetTaskJob(job.UID)
	if !ok || running.ID != job.ID || (running.Status != "running" && running.Status != "pending") {
		t.Fatalf("resumed job = %+v, want same job running or queued", running)
	}
	cancel()
	stopped := make(chan struct{})
	go func() { restarted.StopTaskJobs(); close(stopped) }()
	state.open()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopTaskJobs did not finish after the in-flight request returned")
	}
	if got := state.count(); got != 1 {
		t.Fatalf("upstream calls after cancellation = %d, want only the in-flight request", got)
	}
	finished, ok := restarted.GetTaskJob(job.UID)
	if !ok || finished.Status != "interrupted" {
		t.Fatalf("job after stop = %+v, want interrupted", finished)
	}
}

func TestResumedJobDoesNotClaimPreviouslyClaimedTask(t *testing.T) {
	const uid = "task-job-no-repeat-claim"
	var mu sync.Mutex
	claimCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"chat_5","target":5,"current":5,"accept_status":"claimed"}]}}`)
			return
		}
		if r.Method == http.MethodPost && (r.URL.Path == "/v2/activity/growth/tasks/reward/claim" || len(r.URL.Path) > len("/claim") && r.URL.Path[len(r.URL.Path)-len("/claim"):] == "/claim") {
			mu.Lock()
			claimCalls++
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"credit":100,"energy":0}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	accounts := pool.New("")
	accounts.Add(&auth.Auth{UID: uid, AccessToken: "token"})
	client := upstream.New()
	client.HTTP = &http.Client{Timeout: 5 * time.Second}
	client.ChatBaseCN, client.BillingBaseCN, client.WebBaseCN = server.URL, server.URL, server.URL
	p := New(Config{Pool: accounts, Upstream: client, AutoTasksEnabled: func() bool { return true }})
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	now := time.Now().UTC()
	raw, err := json.Marshal(taskJobFile{Version: 1, Jobs: []TaskJob{{
		ID: "resume-no-claim", UID: uid, Realm: "cn", Trigger: "manual", Status: "running",
		StartedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Second), Total: len(autoActions),
		Completed: 1, Results: []map[string]any{{"task_code": "chat_5", "status": "done"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.StopTaskJobs() })
	if err := p.StartTaskJobs(ctx, path); err != nil {
		t.Fatal(err)
	}
	p.ResumeTaskJobs()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := p.GetTaskJob(uid)
		if ok && job.Status == "finished" {
			if len(job.Results) == 0 || job.Results[0]["task_code"] != "chat_5" {
				t.Fatalf("finished results lost the already-claimed task: %#v", job.Results)
			}
			mu.Lock()
			gotClaims := claimCalls
			mu.Unlock()
			if gotClaims != 0 {
				t.Fatalf("resumed run issued %d claims for an already-claimed task", gotClaims)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := p.GetTaskJob(uid)
	t.Fatalf("resumed job did not finish: %+v", job)
}

func TestTaskJobStatusReturnsNullForExistingAccountWithoutJob(t *testing.T) {
	p, _, _ := newTaskJobTestPanel(t, "task-job-empty", "cn", nil)
	r := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/task-job-empty/tasks/job", nil)
	w := httptest.NewRecorder()
	r.SetPathValue("uid", "task-job-empty")
	p.taskJobStatus(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Job json.RawMessage `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if string(response.Job) != "null" {
		t.Fatalf("job = %s, want null", response.Job)
	}
}

func TestCloneTaskJobCopiesNonEmptyResults(t *testing.T) {
	original := TaskJob{Results: []map[string]any{{"task_code": "chat_5", "status": "done"}}}
	clone := cloneTaskJob(original)
	if len(clone.Results) != 1 || clone.Results[0]["task_code"] != "chat_5" || clone.Results[0]["status"] != "done" {
		t.Fatalf("clone results = %#v, want preserved task result", clone.Results)
	}
	clone.Results[0]["status"] = "changed"
	if original.Results[0]["status"] != "done" {
		t.Fatalf("clone shares result map with original: %#v", original.Results[0])
	}
}
