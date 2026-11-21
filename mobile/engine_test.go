package mobile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// testAuthDoc 生成与插件 OAuth 输出同形的账号文件内容。
func testAuthDoc(t *testing.T, uid, nick, realm, accessToken, refreshToken string, expiresAt int64, enterpriseID string) string {
	t.Helper()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt,
			"domain":       "codebuddy.cn",
			"realm":        realm,
		},
		"account": map[string]any{
			"uid":          uid,
			"nickname":     nick,
			"enterpriseId": enterpriseID,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal auth doc: %v", err)
	}
	return string(raw)
}

func TestImportAndList(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)

	out, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", 9999999999, ""))
	if err != nil {
		t.Fatalf("import nested: %v", err)
	}
	var acc accountJSON
	if err := json.Unmarshal([]byte(out), &acc); err != nil {
		t.Fatalf("decode account json: %v", err)
	}
	if acc.UID != "u1" || acc.Nickname != "一号" || acc.Realm != "cn" || acc.Enterprise || acc.File != "workbuddy-u1.json" {
		t.Fatalf("unexpected account: %+v", acc)
	}

	flat := `{"accessToken":"at-2","refreshToken":"rt-2","expiresAt":9999999999,"uid":"u2","nickname":"二号"}`
	if _, err := e.ImportAccount(flat); err != nil {
		t.Fatalf("import flat: %v", err)
	}

	raw, err := e.ListAccounts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []accountJSON
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 2 || list[0].UID != "u1" || list[1].UID != "u2" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestImportRejectsInvalid(t *testing.T) {
	e := NewEngine(t.TempDir())
	cases := []string{
		`not json`,
		`{"auth":{"accessToken":""},"account":{"uid":"u1"}}`,
		`{"accessToken":"at","refreshToken":"rt"}`,
		`{"accessToken":"at","refreshToken":"rt","uid":"../x"}`,
		`{"accessToken":"at","refreshToken":"rt","uid":"` + strings.Repeat("a", 65) + `"}`,
	}
	for _, c := range cases {
		if _, err := e.ImportAccount(c); err == nil {
			t.Fatalf("want error for %s", c)
		}
	}

	if _, err := e.ImportAccount(`not json`); err == nil {
		t.Fatalf("want parse error for malformed json")
	} else if !strings.Contains(err.Error(), "账号文件解析失败") {
		t.Fatalf("parse error missing Chinese context: %v", err)
	}
}

func TestImportOverwritesSameUID(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "旧", "cn", "at-old", "rt-old", 9999999999, "")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "新", "cn", "at-new", "rt-new", 9999999999, "")); err != nil {
		t.Fatalf("second import: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-u1.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode file: %v", err)
	}
	if doc.Auth.AccessToken != "at-new" {
		t.Fatalf("accessToken=%q want at-new", doc.Auth.AccessToken)
	}
}

type fakeUpstream struct {
	srv         *httptest.Server
	checkinHits atomic.Int32
	refreshHits atomic.Int32
	checkinBody string
	refreshCode int
	refreshBody string

	authMu   sync.Mutex
	lastAuth string
	lastUID  string
}

func (f *fakeUpstream) setLastAuth(v string) {
	f.authMu.Lock()
	f.lastAuth = v
	f.authMu.Unlock()
}

func (f *fakeUpstream) lastAuthValue() string {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	return f.lastAuth
}

func (f *fakeUpstream) setLastUID(v string) {
	f.authMu.Lock()
	f.lastUID = v
	f.authMu.Unlock()
}

func (f *fakeUpstream) lastUIDValue() string {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	return f.lastUID
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		checkinBody: `{"code":0,"msg":"ok","data":{}}`,
		refreshCode: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		f.checkinHits.Add(1)
		f.setLastAuth(r.Header.Get("Authorization"))
		f.setLastUID(r.Header.Get("X-User-Id"))
		if got := r.Header.Get("X-CodeBuddy-Request"); got != "1" {
			t.Errorf("X-CodeBuddy-Request=%q want 1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.checkinBody)
	})
	mux.HandleFunc("/v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		f.refreshHits.Add(1)
		if got := r.Header.Get("X-Refresh-Token"); got == "" {
			t.Errorf("missing X-Refresh-Token")
		}
		w.Header().Set("Content-Type", "application/json")
		if f.refreshCode != http.StatusOK {
			w.WriteHeader(f.refreshCode)
		}
		_, _ = io.WriteString(w, f.refreshBody)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) engine(dir string) *Engine {
	up := upstream.New()
	up.HTTP = f.srv.Client()
	up.ChatBaseCN = f.srv.URL
	up.BillingBaseCN = f.srv.URL
	return newEngineWith(dir, up)
}

func decodeResult(t *testing.T, raw string) checkinResult {
	t.Helper()
	var r checkinResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return r
}

func TestCheckinSuccess(t *testing.T) {
	f := newFakeUpstream(t)
	e := f.engine(t.TempDir())
	future := time.Now().Add(24 * time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", future, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "ok" || r.Message != "签到成功" {
		t.Fatalf("result=%+v", r)
	}
	if f.checkinHits.Load() != 1 || f.refreshHits.Load() != 0 {
		t.Fatalf("hits checkin=%d refresh=%d", f.checkinHits.Load(), f.refreshHits.Load())
	}
	if f.lastAuthValue() != "Bearer at-1" {
		t.Fatalf("authorization=%q", f.lastAuthValue())
	}
	if f.lastUIDValue() != "u1" {
		t.Fatalf("X-User-Id=%q want u1", f.lastUIDValue())
	}
}

func TestCheckinAlready(t *testing.T) {
	f := newFakeUpstream(t)
	f.checkinBody = `{"code":10001,"msg":"今天已签到","data":{}}`
	e := f.engine(t.TempDir())
	future := time.Now().Add(24 * time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", future, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "already" || r.Message != "今天已签到" {
		t.Fatalf("result=%+v", r)
	}
	if f.checkinHits.Load() != 1 {
		t.Fatalf("checkin hits=%d want 1", f.checkinHits.Load())
	}
}

func TestCheckinBusinessFailure(t *testing.T) {
	f := newFakeUpstream(t)
	f.checkinBody = `{"code":1,"msg":"服务开小差了","data":{}}`
	e := f.engine(t.TempDir())
	future := time.Now().Add(24 * time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", future, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "failed" || !strings.Contains(r.Message, "服务开小差了") {
		t.Fatalf("result=%+v", r)
	}
	if f.checkinHits.Load() != 1 {
		t.Fatalf("checkin hits=%d want 1", f.checkinHits.Load())
	}
}

func TestCheckinErrorMessage(t *testing.T) {
	if got := checkinErrorMessage(fmt.Errorf("dial tcp: i/o timeout")); !strings.HasPrefix(got, "网络错误：") {
		t.Fatalf("transport error=%q want 网络错误： prefix", got)
	}
	upErr := &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}
	got := checkinErrorMessage(upErr)
	if strings.HasPrefix(got, "网络错误：") {
		t.Fatalf("upstream error=%q should not carry network prefix", got)
	}
	if !strings.Contains(got, "boom") {
		t.Fatalf("upstream error=%q missing msg", got)
	}
}

func TestShortLimit(t *testing.T) {
	long := strings.Repeat("签", 200)
	got := short(long)
	if len([]rune(got)) != 160 {
		t.Fatalf("short rune len=%d want 160", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("short=%q missing ellipsis", got)
	}
}

func TestCheckinSkipsEnterpriseAndGlobal(t *testing.T) {
	f := newFakeUpstream(t)
	e := f.engine(t.TempDir())
	future := time.Now().Add(24 * time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "ent", "企业", "cn", "at-ent", "rt-ent", future, "ent-1")); err != nil {
		t.Fatalf("import enterprise: %v", err)
	}
	if _, err := e.ImportAccount(testAuthDoc(t, "glb", "国际", "global", "at-glb", "rt-glb", future, "")); err != nil {
		t.Fatalf("import global: %v", err)
	}

	want := map[string]string{
		"ent": "企业账号无签到体系（已跳过）",
		"glb": "国际版账号无签到体系（已跳过）",
	}
	for _, uid := range []string{"ent", "glb"} {
		raw, err := e.Checkin(uid)
		if err != nil {
			t.Fatalf("checkin %s: %v", uid, err)
		}
		r := decodeResult(t, raw)
		if r.Status != "skipped" || r.Message != want[uid] {
			t.Fatalf("%s result=%+v want message %q", uid, r, want[uid])
		}
	}
	if f.checkinHits.Load() != 0 || f.refreshHits.Load() != 0 {
		t.Fatalf("unexpected upstream hits checkin=%d refresh=%d", f.checkinHits.Load(), f.refreshHits.Load())
	}
}

func TestCheckinUnknownUID(t *testing.T) {
	f := newFakeUpstream(t)
	e := f.engine(t.TempDir())
	if _, err := e.Checkin("missing"); err == nil {
		t.Fatalf("want error for missing uid")
	}
}

func TestCheckinRefreshesExpiringToken(t *testing.T) {
	f := newFakeUpstream(t)
	f.refreshBody = `{"code":0,"msg":"ok","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":5184000,"domain":"codebuddy.cn"}}`
	dir := t.TempDir()
	e := f.engine(dir)
	soon := time.Now().Add(time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "old-at", "old-rt", soon, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "ok" {
		t.Fatalf("result=%+v", r)
	}
	if f.refreshHits.Load() != 1 {
		t.Fatalf("refresh hits=%d want 1", f.refreshHits.Load())
	}
	if f.lastAuthValue() != "Bearer new-at" {
		t.Fatalf("checkin authorization=%q want Bearer new-at", f.lastAuthValue())
	}

	saved, err := os.ReadFile(filepath.Join(dir, "workbuddy-u1.json"))
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	a, err := auth.Parse(saved)
	if err != nil {
		t.Fatalf("parse saved: %v", err)
	}
	if a.AccessTokenValue() != "new-at" {
		t.Fatalf("saved accessToken=%q", a.AccessTokenValue())
	}
	if a.RefreshTokenValue() != "new-rt" {
		t.Fatalf("saved refreshToken=%q", a.RefreshTokenValue())
	}
}

func TestCheckinAuthInvalidOnDeadRefresh(t *testing.T) {
	f := newFakeUpstream(t)
	f.refreshCode = http.StatusUnauthorized
	f.refreshBody = `{"code":12153,"msg":"Offline user session not found"}`
	e := f.engine(t.TempDir())
	soon := time.Now().Add(time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "old-at", "old-rt", soon, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "auth_invalid" {
		t.Fatalf("result=%+v", r)
	}
	if f.checkinHits.Load() != 0 {
		t.Fatalf("checkin hits=%d want 0", f.checkinHits.Load())
	}
}

func TestCheckinAuthInvalidWithoutRefreshToken(t *testing.T) {
	f := newFakeUpstream(t)
	e := f.engine(t.TempDir())
	soon := time.Now().Add(time.Hour).Unix()
	doc := `{"auth":{"accessToken":"old-at","expiresAt":` + strconv.FormatInt(soon, 10) + `,"domain":"codebuddy.cn"},"account":{"uid":"u1","nickname":"一号"}}`
	if _, err := e.ImportAccount(doc); err != nil {
		t.Fatalf("import: %v", err)
	}

	raw, err := e.Checkin("u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r := decodeResult(t, raw)
	if r.Status != "auth_invalid" {
		t.Fatalf("result=%+v", r)
	}
	if f.refreshHits.Load() != 0 || f.checkinHits.Load() != 0 {
		t.Fatalf("unexpected hits refresh=%d checkin=%d", f.refreshHits.Load(), f.checkinHits.Load())
	}
}

func TestConcurrentCheckinSerialized(t *testing.T) {
	f := newFakeUpstream(t)
	e := f.engine(t.TempDir())
	future := time.Now().Add(24 * time.Hour).Unix()
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", future, "")); err != nil {
		t.Fatalf("import: %v", err)
	}

	// 钩子在持锁期间执行：若 Checkin 未串行化，两个 goroutine 会同时进入钩子，
	// maxActive 升至 2；sleep 扩大重叠窗口让竞争稳定复现。
	var (
		hookMu    sync.Mutex
		active    int
		maxActive int
	)
	e.testHook = func() {
		hookMu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		hookMu.Unlock()

		time.Sleep(50 * time.Millisecond)

		hookMu.Lock()
		active--
		hookMu.Unlock()
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.Checkin("u1"); err != nil {
				t.Errorf("checkin: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if maxActive != 1 {
		t.Fatalf("maxActive=%d want 1 (Checkin 未串行化)", maxActive)
	}
	if f.checkinHits.Load() != 2 {
		t.Fatalf("checkin hits=%d want 2", f.checkinHits.Load())
	}
}
