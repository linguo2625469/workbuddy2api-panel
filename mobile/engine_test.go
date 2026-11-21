package mobile

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	lastAuth    string
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
		f.lastAuth = r.Header.Get("Authorization")
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
	if f.lastAuth != "Bearer at-1" {
		t.Fatalf("authorization=%q", f.lastAuth)
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

	for _, uid := range []string{"ent", "glb"} {
		raw, err := e.Checkin(uid)
		if err != nil {
			t.Fatalf("checkin %s: %v", uid, err)
		}
		r := decodeResult(t, raw)
		if r.Status != "skipped" {
			t.Fatalf("%s result=%+v", uid, r)
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
