package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// postImport 以 multipart 表单发一次 cockpit 导入。
func postImport(t *testing.T, p *Panel, payload string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestImportCockpitAllowsEmptyRefreshToken 必填判据收敛为 uid + access_token：
// refresh_token 为空的记录不再被跳过（issue #165 的 access-token-only 场景，
// 如静态 API Key 凭证）。缺 access_token 的记录仍应被跳过。
func TestImportCockpitAllowsEmptyRefreshToken(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[]}}}}`))
	})
	payload := `[` +
		`{"id":"a1","uid":"u-no-rt","access_token":"at-1","refresh_token":""},` +
		`{"id":"a2","uid":"u-no-at","access_token":""}` +
		`]`
	rec := postImport(t, p, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["imported"] != float64(1) || resp["skipped"] != float64(1) {
		t.Fatalf("imported/skipped=%v/%v want 1/1 body=%s", resp["imported"], resp["skipped"], rec.Body.String())
	}
	a := p.cfg.Pool.AuthByUID("u-no-rt")
	if a == nil {
		t.Fatal("refresh_token 为空的记录应被导入")
	}
	if a.RefreshTokenValue() != "" {
		t.Errorf("refreshToken 应为空，实际=%q", a.RefreshTokenValue())
	}
	if _, ok := p.cfg.Pool.Status("u-no-at"); ok {
		t.Error("缺 access_token 的记录不应被导入")
	}
}
