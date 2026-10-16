package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// panelWithUpstream 构造带 mock 上游与临时 auth 目录的面板（APIKey 留空 = 不鉴权）。
func panelWithUpstream(t *testing.T, handler http.HandlerFunc) *Panel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{
		Version:  "test",
		Pool:     pool.New(""),
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL},
		AuthDir:  t.TempDir(),
	})
}

// postJSON 发一次 JSON POST 到面板。
func postJSON(t *testing.T, p *Panel, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// authFiles 返回 auth 目录下的文件名列表（断言"未落盘"用）。
func authFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestAddAPIKeyRejectsBadPrefix 非 ck_/pt_ 前缀直接 400，且不打上游、不落盘。
func TestAddAPIKeyRejectsBadPrefix(t *testing.T) {
	upstreamCalled := false
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": "eyJhbGciOiJIUzI1NiJ9.x.y"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("前缀非法时不应探测上游")
	}
	if files := authFiles(t, p.cfg.AuthDir); len(files) != 0 {
		t.Errorf("前缀非法时不应落盘，实际=%v", files)
	}
}

// TestAddAPIKeyRejectsInvalidKey 上游报错（key 无效）时 400 并带上游文案，不落盘。
func TestAddAPIKeyRejectsInvalidKey(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":10001,"msg":"invalid api key"}`))
	})
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": "ck_bad.secret"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "invalid api key") {
		t.Errorf("应带上游错误文案，实际 error=%q", msg)
	}
	if files := authFiles(t, p.cfg.AuthDir); len(files) != 0 {
		t.Errorf("key 无效时不应落盘，实际=%v", files)
	}
}

// TestAddAPIKeySuccess 正常路径：探测通过 → 落盘（auth.LoadDir 兼容）→ 热加载进池
// 并写入企业口径额度 → 返回 uid/nickname/api_key。
func TestAddAPIKeySuccess(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage") {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":812.45,"limitNum":2000,` +
				`"cycleEndTime":"2099-10-26 23:59:59","cycleResetTime":"2099-10-27 00:00:00"}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})
	const key = "ck_9f3a2b1c.deadbeef"
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": key, "nickname": "钥匙"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["uid"] != "apikey-9f3a2b1c" || resp["nickname"] != "钥匙" || resp["api_key"] != true {
		t.Errorf("响应异常：%v", resp)
	}

	// 落盘形态必须与 auth.LoadDir/Parse 兼容。
	fp := filepath.Join(p.cfg.AuthDir, "workbuddy-apikey-9f3a2b1c.json")
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if a.AccessToken != key || a.RefreshToken != "" || a.ExpiresAt != 0 || a.EnterpriseID != "" {
		t.Errorf("落盘字段异常：%+v", a)
	}
	if a.UID != "apikey-9f3a2b1c" || !a.IsAPIKey() {
		t.Errorf("落盘 uid/判定异常：uid=%q isAPIKey=%v", a.UID, a.IsAPIKey())
	}
	// Windows 的权限位只有只读位有语义（os.WriteFile 的 0600 落成 0666），
	// 该断言只在 POSIX 平台有意义——容器与 CI 都是 Linux，仍能守住 0600。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(fp); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("auth 文件权限=%v want 0600", fi.Mode().Perm())
		}
	}
	loaded, err := auth.LoadDir(p.cfg.AuthDir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadDir 应加载 1 个账号：n=%d err=%v", len(loaded), err)
	}

	// 热加载进池 + 企业口径额度写入。
	st, ok := p.cfg.Pool.Status("apikey-9f3a2b1c")
	if !ok {
		t.Fatal("账号未热加载进池")
	}
	if !st.APIKey {
		t.Error("Status.APIKey 应为 true")
	}
	if st.Credits != 1188 || st.CreditsTotal != 2000 {
		t.Errorf("额度映射错误：credits=%d total=%d want 1188/2000", st.Credits, st.CreditsTotal)
	}
}

// TestAddAPIKeyExplicitUID 显式 uid 走字符白名单校验：合法值原样使用，非法值 400。
func TestAddAPIKeyExplicitUID(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":0,"limitNum":100}}`))
	})
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{
		"access_token": "pt_abc.secret", "uid": "my-uid_1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 uid 应 200，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := p.cfg.Pool.Status("my-uid_1"); !ok {
		t.Error("显式 uid 未生效")
	}

	rec2 := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{
		"access_token": "ck_abc.secret", "uid": "../../evil",
	})
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("非法 uid 应 400，实际 code=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
