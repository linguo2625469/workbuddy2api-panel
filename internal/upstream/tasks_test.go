package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestListGlobalTasksPreservesObservedSchemaAndUnknownFields(t *testing.T) {
	previous := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previous) })

	const envelope = `{"code":0,"message":"OK","data":{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"使用 WorkBuddy 完成至少一次对话交互","icon_url":"","level_name":"养虾尝试","status":"available","url":""},
		{"task_id":2,"code":"skill_installed","title":"完成技能安装","description":"成功安装至少一个 WorkBuddy 技能","icon_url":"","level_name":"养虾入门","status":"available","url":""},
		{"task_id":3,"code":"wechat_linked","title":"链接微信","description":"完成 WorkBuddy 设置，成功链接到微信","icon_url":"","level_name":"养虾熟手","status":"available","url":""},
		{"task_id":4,"code":"expert_summoned","title":"召唤一次专家","description":"使用 WorkBuddy 召唤并完成一次专家交互","icon_url":"","level_name":"玩虾老手","status":"available","url":""},
		{"task_id":5,"code":"template_used","title":"使用一次模板","description":"使用至少一个 WorkBuddy 模板完成任务","icon_url":"","level_name":"控虾大神","status":"available","url":""}
	]}}`
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(envelope))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatHTTP = nil
	c.ChatBaseGlobal = srv.URL
	a := &auth.Auth{AccessToken: "at", UID: "u-global"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatal(err)
	}

	tasks, err := c.ListTasks(a)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != tasksListPath {
		t.Fatalf("request = %s %s, want GET %s", gotMethod, gotPath, tasksListPath)
	}
	if len(tasks) != 5 {
		t.Fatalf("got %d tasks, want 5", len(tasks))
	}
	wantCodes := []string{"first_chat", "skill_installed", "wechat_linked", "expert_summoned", "template_used"}
	for i, task := range tasks {
		if task.TaskCode != wantCodes[i] || task.Code != wantCodes[i] || task.TaskID != int64(i+1) || task.Status != "available" {
			t.Errorf("task[%d] identifiers/status = (%q, %q, %d, %q)", i, task.TaskCode, task.Code, task.TaskID, task.Status)
		}
		if task.ProgressKnown || task.RewardKnown {
			t.Errorf("task[%d] unknown progress/reward marked known", i)
		}
		encoded, err := json.Marshal(task)
		if err != nil {
			t.Fatalf("marshal task[%d]: %v", i, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"target", "current", "credit", "energy", "has_reward"} {
			if _, ok := fields[key]; ok {
				t.Errorf("task[%d] should omit unknown %q: %s", i, key, encoded)
			}
		}
	}
	if got := []string{tasks[0].TaskCode, tasks[1].TaskCode, tasks[2].TaskCode, tasks[3].TaskCode, tasks[4].TaskCode}; !reflect.DeepEqual(got, wantCodes) {
		t.Fatalf("task codes = %v, want %v", got, wantCodes)
	}
}

func TestParseCNTaskKeepsExplicitZeroProgressFields(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(`{"tasks":[{"task_code":"chat_5","target":5,"current":0,"reward_credit":0,"reward_energy":0,"has_reward":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !tasks[0].ProgressKnown || !tasks[0].RewardKnown {
		t.Fatalf("explicit zero task should retain known flags: %+v", tasks)
	}
	encoded, err := json.Marshal(tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"target", "current", "progress_known", "reward_known"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("CN task lost %q: %s", key, encoded)
		}
	}
	if strings.Contains(string(encoded), `"target":0`) {
		t.Errorf("target should preserve explicit value 5, got %s", encoded)
	}
}

// TestClaimRewardWebEndpoint 领奖走 Web 域（workbuddy.cn）、任务码在路径里、无 body。
// 这是与 CLI 域（copilot.tencent.com/v2/.../reward/claim，task_code 在 body）的关键区别——
// 后者路径不存在，曾导致长期 400 "task not completed" 误判为"上游不支持领取"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	var gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（任务码必须在路径里）", gotPath, want)
	}
	if gotBody != "" {
		t.Errorf("claim 不应携带 body，got %q", gotBody)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}
	if !strings.Contains(gotReferer, "workbuddy.cn") {
		t.Errorf("Referer=%q 应指向 workbuddy.cn", gotReferer)
	}
}

// TestClaimRewardAlreadyClaimed 重复领取：上游返回 already_claimed=true，
// 本地应视为"无新增奖励但不报错"（幂等语义）。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted 未达标：上游 400 + task not completed 应作为错误透出。
func TestClaimRewardNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}
