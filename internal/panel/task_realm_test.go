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

func useRealmForTest(t *testing.T) {
	t.Helper()
	previous := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previous) })
}

func panelWithTestAccount(a *auth.Auth, up *upstream.Client) *Panel {
	p := pool.New("")
	p.Add(a)
	return New(Config{Pool: p, Upstream: up})
}

func testRealmAuth(t *testing.T, uid, realm string) *auth.Auth {
	t.Helper()
	a := &auth.Auth{UID: uid, Nickname: uid, AccessToken: "access-token"}
	if _, err := auth.BackfillRealmFor(a, realm); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGlobalTaskReadReturnsCapabilitiesAndNormalizedIDs(t *testing.T) {
	useRealmForTest(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v2/activity/growth/tasks" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"tasks":[{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"使用 WorkBuddy 完成至少一次对话交互","status":"available"}]}}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP, up.ChatHTTP, up.ChatBaseGlobal = srv.Client(), nil, srv.URL
	p := panelWithTestAccount(testRealmAuth(t, "global-1", "global"), up)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/global-1/tasks", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream request count=%d, want one read without MP probe", got)
	}
	var body struct {
		Realm        string                       `json:"realm"`
		Capabilities map[string]bool              `json:"capabilities"`
		Tasks        []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Realm != "global" || body.Capabilities["accept"] || body.Capabilities["claim"] || body.Capabilities["automate"] {
		t.Fatalf("realm/capabilities = %q/%v", body.Realm, body.Capabilities)
	}
	if len(body.Tasks) != 1 {
		t.Fatalf("tasks count=%d, want 1", len(body.Tasks))
	}
	task := body.Tasks[0]
	for key, want := range map[string]string{"task_code": "first_chat", "code": "first_chat", "title": "完成一次对话", "status": "available"} {
		var got string
		if err := json.Unmarshal(task[key], &got); err != nil || got != want {
			t.Errorf("task[%s]=%s, want %q", key, task[key], want)
		}
	}
	for _, key := range []string{"task_id", "progress_known", "reward_known"} {
		if _, ok := task[key]; !ok {
			t.Errorf("task missing %q: %s", key, rec.Body.String())
		}
	}
	for _, key := range []string{"target", "current", "credit", "energy"} {
		if _, ok := task[key]; ok {
			t.Errorf("unknown field %q was serialized as a value: %s", key, rec.Body.String())
		}
	}
}

func TestGlobalTaskWriteEndpointsDoNotCallUpstream(t *testing.T) {
	useRealmForTest(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{}}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP, up.ChatHTTP = srv.Client(), nil
	up.ChatBaseGlobal, up.BillingBaseGlobal = srv.URL, srv.URL
	p := panelWithTestAccount(testRealmAuth(t, "global-1", "global"), up)
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/panel/api/accounts/global-1/tasks/accept", `{"task_codes":["first_chat"]}`},
		{"/panel/api/accounts/global-1/tasks/accept_all", ""},
		{"/panel/api/accounts/global-1/tasks/claim", `{"task_code":"first_chat"}`},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status=%d body=%s, want 501", tc.path, rec.Code, rec.Body.String())
		}
		var response struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode %s response: %v", tc.path, err)
		}
		if response.OK || response.Error != globalTaskWriteMessage {
			t.Errorf("%s response=%+v", tc.path, response)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("global write routes made %d upstream requests, want 0", got)
	}
}

func TestGlobalCheckinIsSkippedWithoutUpstreamRequest(t *testing.T) {
	useRealmForTest(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{}}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP, up.BillingBaseGlobal = srv.Client(), srv.URL
	p := panelWithTestAccount(testRealmAuth(t, "global-1", "global"), up)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/global-1/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Skipped    bool   `json:"skipped"`
		Realm      string `json:"realm"`
		Done       bool   `json:"checkin_done"`
		SkipReason string `json:"skip_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Skipped || response.Realm != "global" || response.Done || response.SkipReason == "" {
		t.Fatalf("response=%+v", response)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("global check-in made %d upstream requests, want 0", got)
	}
}

func TestCNCheckinFailureStillRefreshesBalanceAndReportsFailure(t *testing.T) {
	useRealmForTest(t)
	var checkins, balances atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins.Add(1)
			_, _ = w.Write([]byte(`{"code":10001,"msg":"check-in activity not started","data":{}}`))
		case "/v2/billing/meter/get-user-resource":
			balances.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":7,"CapacitySize":12}]}}}}`))
		default:
			t.Errorf("unexpected billing path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP, up.BillingBaseCN = srv.Client(), srv.URL
	pool := pool.New("")
	pool.Add(testRealmAuth(t, "cn-1", "cn"))
	p := New(Config{Pool: pool, Upstream: up})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/cn-1/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK           bool   `json:"ok"`
		Done         bool   `json:"checkin_done"`
		CheckinErr   string `json:"checkin_error"`
		Credits      int64  `json:"credits"`
		BalanceError string `json:"balance_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Done || response.CheckinErr == "" || response.Credits != 7 || response.BalanceError != "" {
		t.Fatalf("response=%+v body=%s", response, rec.Body.String())
	}
	if checkins.Load() != 1 || balances.Load() != 1 {
		t.Fatalf("check-ins=%d balance reads=%d, want 1 each", checkins.Load(), balances.Load())
	}
}

func TestTaskScanAndQueueExposeSkippedAccountsAndScanErrors(t *testing.T) {
	useRealmForTest(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer srv.Close()

	up := upstream.New()
	up.HTTP, up.ChatHTTP = srv.Client(), nil
	up.ChatBaseCN, up.ChatBaseGlobal = srv.URL, srv.URL
	accounts := pool.New("")
	accounts.Add(testRealmAuth(t, "cn-1", "cn"))
	accounts.Add(testRealmAuth(t, "global-1", "global"))
	p := New(Config{Pool: accounts, Upstream: up})

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/panel/api/tasks/scan_all", "{}"},
		{"/panel/api/tasks/run_queue", `{"growth":true,"concurrency":1}`},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		accountsKey := "accounts"
		if tc.path == "/panel/api/tasks/run_queue" {
			accountsKey = "scan_accounts"
			if response["ok"] != false || response["started"] != false {
				t.Errorf("queue scan error incorrectly reported as success: %v", response)
			}
			if !strings.Contains(response["message"].(string), "扫描失败") {
				t.Errorf("queue message does not explain scan failure: %v", response["message"])
			}
			if response["scan_error_count"] != float64(1) {
				t.Errorf("scan_error_count=%v, want 1", response["scan_error_count"])
			}
		} else {
			if response["pending_count"] != float64(0) || response["skipped_count"] != float64(1) || response["error_count"] != float64(1) {
				t.Errorf("scan counts = %v", response)
			}
		}
		rows, ok := response[accountsKey].([]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("%s %s = %v, want two account rows", tc.path, accountsKey, response[accountsKey])
		}
		byUID := map[string]map[string]any{}
		for _, value := range rows {
			row := value.(map[string]any)
			byUID[row["uid"].(string)] = row
		}
		global := byUID["global-1"]
		if global["realm"] != "global" || global["skipped"] != true || global["skip_reason"] == "" {
			t.Errorf("global scan row=%v", global)
		}
		cn := byUID["cn-1"]
		if cn["realm"] != "cn" || cn["growth_error"] == "" {
			t.Errorf("CN scan row=%v", cn)
		}
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("upstream requests=%d, want two CN paths per scan and no global calls", got)
	}
}
