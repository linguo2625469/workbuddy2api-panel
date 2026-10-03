package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// newKeyring 构建测试用多 Key 表。
func newKeyring(t *testing.T, specs ...httpauth.KeySpec) *httpauth.Keyring {
	t.Helper()
	kr := httpauth.NewKeyring(specs)
	if kr == nil {
		t.Fatal("keyring is nil")
	}
	return kr
}

// TestMultiKeyModelLimit 覆盖模型白名单：命中放行，越权本地 400 且不打上游。
func TestMultiKeyModelLimit(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Keyring: newKeyring(t, httpauth.KeySpec{
			Key: "sk-codex", Name: "codex", Models: []string{"glm-*"}, Enabled: true,
		}),
	})

	// 1) 白名单内：放行。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-codex")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("allowed model code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls)
	}

	// 2) 白名单外：本地 400，且不打上游、不消耗额度。
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer sk-codex")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 400 {
		t.Fatalf("blocked model code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream called for blocked model (calls=%d)", calls)
	}
	var errResp map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("error body not json: %v", err)
	}
	if !strings.Contains(rec2.Body.String(), "model_not_allowed") {
		t.Errorf("error code missing: %s", rec2.Body)
	}
}

// TestMultiKeyResponsesModelLimit /v1/responses 同样受白名单约束。
func TestMultiKeyResponsesModelLimit(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Keyring: newKeyring(t, httpauth.KeySpec{
			Key: "sk-codex", Name: "codex", Models: []string{"gpt-6-astra"}, Enabled: true,
		}),
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-codex")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestMultiKeyAuthIsolation 未登记的密钥一律 401（多 Key 模式下不再回落顶层 api_key）。
func TestMultiKeyAuthIsolation(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "sk-legacy",
		Keyring: newKeyring(t, httpauth.KeySpec{
			Key: "sk-codex", Name: "codex", Enabled: true,
		}),
	})
	for _, tc := range []struct {
		header string
		want   int
	}{
		{"", 401},
		{"Bearer sk-legacy", 401}, // 多 Key 模式下顶层 api_key 不再是有效凭证
		{"Bearer sk-nope", 401},
		{"Bearer sk-codex", 200},
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("header=%q code=%d, want %d (body=%s)", tc.header, rec.Code, tc.want, rec.Body)
		}
	}
}

// TestMultiKeyRealmBinding 出口绑定：Key 指定 global 时只选 global 账号。
func TestMultiKeyRealmBinding(t *testing.T) {
	var usedAuth string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		usedAuth = authz
		return 200, sseOK, true
	})
	globalAcct := &auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(globalAcct, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn", ExpiresAt: 9999999999},
		globalAcct,
	)
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Keyring: newKeyring(t, httpauth.KeySpec{
			Key: "sk-intl", Name: "intl", Realm: "global", Enabled: true,
		}),
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-intl")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if usedAuth != "Bearer at-global" {
		t.Errorf("upstream auth=%q, want global account", usedAuth)
	}
}
