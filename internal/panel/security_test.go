package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/assets/index.js"}, // 不存在也走同一 handler（FileServer 前已写头）
		{"GET", "/panel/api/overview"},    // 401（未提供 key）
		{"POST", "/panel/api/config"},     // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `/panel/assets/`) || !strings.Contains(body, ".js") {
		t.Error("index.html must load the vite bundle externally (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// 构建产物必须能作为同源脚本取到且类型正确（否则页面白屏）。
// bundle 文件名带 hash：从页面里解析出真实 URL 再回放（端到端接线校验）。
func TestPanelAssetsServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	idx := strings.Index(body, "/panel/assets/")
	if idx < 0 {
		t.Fatal("index.html 未引用 /panel/assets/ 构建产物")
	}
	end := strings.Index(body[idx:], `"`)
	if end < 0 {
		t.Fatal("无法解析产物 URL")
	}
	asset := body[idx : idx+end]
	if !strings.HasSuffix(asset, ".js") {
		// 首个命中可能是 css；找下一个 js
		rest := body[idx+end:]
		idx2 := strings.Index(rest, "/panel/assets/")
		if idx2 < 0 {
			t.Fatal("index.html 未引用 js 产物")
		}
		end2 := strings.Index(rest[idx2:], `"`)
		if end2 < 0 {
			t.Fatal("无法解析 js 产物 URL")
		}
		asset = rest[idx2 : idx2+end2]
	}
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest("GET", asset, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET %s code=%d want 200", asset, rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if rec2.Body.Len() == 0 {
		t.Error("bundle body 为空")
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}
