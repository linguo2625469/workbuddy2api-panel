package panel

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestTaskJobsAutomaticTriggersRespectSwitchAndRealm(t *testing.T) {
	state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{})}
	p, cancel, _ := newTaskJobTestPanel(t, "cn-daily", "cn", state)
	var enabled atomic.Bool
	p.cfg.AutoTasksEnabled = enabled.Load
	p.cfg.Pool.Add(&auth.Auth{UID: "cn-new", AccessToken: "token"})
	p.cfg.Pool.Add(&auth.Auth{UID: "cn-disabled", AccessToken: "token"})
	p.cfg.Pool.Disable("cn-disabled", "test")
	global := &auth.Auth{UID: "global-new", AccessToken: "token"}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(global)

	p.StartNewAccountTasks("cn-new")
	p.RunGrowthQueueOnce()
	if jobs := p.ListTaskJobs(); len(jobs) != 0 || state.count() != 0 {
		t.Fatalf("disabled automatic tasks ran: %+v", jobs)
	}
	enabled.Store(true)
	p.StartNewAccountTasks("cn-new")
	newJob, found := p.GetTaskJob("cn-new")
	if !found || newJob.Trigger != "new_account" {
		t.Fatalf("new-account trigger missing: %+v", newJob)
	}
	p.StartNewAccountTasks("global-new")
	p.RunGrowthQueueOnce()
	dailyJob, found := p.GetTaskJob("cn-daily")
	if !found || dailyJob.Trigger != "daily" {
		t.Fatalf("daily trigger missing: %+v", dailyJob)
	}
	if jobs := p.ListTaskJobs(); len(jobs) != 2 {
		t.Fatalf("disabled/global accounts must be excluded: %+v", jobs)
	}
	if reused, _ := p.GetTaskJob("cn-new"); reused.ID != newJob.ID {
		t.Fatal("daily trigger replaced a running new-account job")
	}

	enabled.Store(false)
	manual, started, err := p.StartAccountTaskJob("cn-disabled", "manual")
	if err != nil || !started || manual.Trigger != "manual" {
		t.Fatalf("switch must allow manual initiation: started=%v err=%v", started, err)
	}
	cancel()
	state.open()
	p.StopTaskJobs()
}

func TestRunningTaskJobStopsAfterAccountChange(t *testing.T) {
	for _, scenario := range []struct {
		change string
		mp     bool
	}{{"disable", false}, {"remove", false}, {"reauth", false}, {"disable", true}, {"remove", true}, {"reauth", true}} {
		name := scenario.change
		if scenario.mp {
			name += "-mp"
		}
		t.Run(name, func(t *testing.T) {
			state := &taskJobServerState{firstCall: make(chan struct{}), release: make(chan struct{}),
				tasksJSON: `[{"task_code":"chat_5","current":0,"target":5,"accept_status":"not_accepted"}]`, blockMP: scenario.mp}
			p, _, _ := newTaskJobTestPanel(t, "cn-changing", "cn", state)
			if _, _, err := p.StartAccountTaskJob("cn-changing", "manual"); err != nil {
				t.Fatal(err)
			}
			waitTaskJobRequest(t, state)
			switch scenario.change {
			case "disable":
				p.cfg.Pool.Disable("cn-changing", "test")
			case "remove":
				p.cfg.Pool.Remove("cn-changing")
			case "reauth":
				p.cfg.Pool.Add(&auth.Auth{UID: "cn-changing", AccessToken: "new-token"})
			}
			state.open()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				job, _ := p.GetTaskJob("cn-changing")
				if job.Status != "running" && job.Status != "pending" {
					wantCalls := 1
					if scenario.mp {
						wantCalls = 2
					}
					if job.Status != "interrupted" || state.count() != wantCalls {
						t.Fatalf("account change did not stop subsequent requests: job=%+v calls=%d", job, state.count())
					}
					return
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("job did not stop after account changed")
				}
			}
		})
	}
}
