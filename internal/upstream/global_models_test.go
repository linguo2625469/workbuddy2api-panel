// global_models_test.go global 模型目录探测的 UA 家族覆盖回归。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGlobalCatalogProbesDesktopUA 锁定「/v3/config 目录按 UA 家族分档」这一上游事实：
// 仅桌面端 UA 下发的模型必须出现在并集里。
//
// 回归目标（issue #98）：gpt-6-luna 只在桌面端目录下发、实际可调用（实测 HTTP 200），
// 只探 IDE + CLI 两路时它永远进不了 /v1/models。
func TestGlobalCatalogProbesDesktopUA(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	const desktopOnly = "gpt-6-luna"
	seenUA := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			// 企业端点家族一律 500：本用例只验证 v3 多路并集，同时覆盖
			// 「家族路失败不拖累 v3 结果」的降级语义。
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ua := r.Header.Get("User-Agent")
		seenUA[ua] = true
		ids := []string{"shared-model"}
		switch {
		case strings.HasPrefix(ua, "CodeBuddyIDE/"):
			ids = append(ids, "ide-only")
		case strings.HasPrefix(ua, "CLI/"):
			ids = append(ids, "cli-only")
		case strings.HasPrefix(ua, "WorkBuddy/"):
			ids = append(ids, desktopOnly)
		default:
			t.Errorf("v3/config 探测带了非预期 UA: %q", ua)
		}
		models := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			models = append(models, map[string]any{
				"id": id, "maxInputTokens": 100000, "maxOutputTokens": 32000,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"models": models},
		})
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}

	names := c.FetchGlobalModels(a)
	got := make(map[string]bool, len(names))
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"shared-model", "ide-only", "cli-only", desktopOnly} {
		if !got[want] {
			t.Errorf("FetchGlobalModels 缺少 %q（并集=%v）", want, names)
		}
	}
	if len(seenUA) != 3 {
		t.Errorf("v3/config 应探 3 种 UA 家族，实际 %d 种: %v", len(seenUA), seenUA)
	}
}
