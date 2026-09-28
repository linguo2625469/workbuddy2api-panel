package panel

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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
		{"GET", "/panel/theme-init.js"},
		{"GET", "/panel/assets/nonexistent.js"}, // 404
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
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

// 页面必须以同源外链加载入口模块（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script type="module" crossorigin src="/panel/assets/`) {
		t.Error("index.html must load the entry module from /panel/assets/ (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control=%q want no-cache（发版后要立刻拿到新文件名）", cc)
	}
}

// 页面引用的每个脚本和样式都必须能取到、类型正确（否则白屏），并且可长缓存、支持 gzip。
func TestIndexAssetsServed(t *testing.T) {
	p := newTestPanel()
	refs := regexp.MustCompile(`(?:src|href)="(/panel/[^"]+)"`).FindAllStringSubmatch(string(indexHTML), -1)
	if len(refs) < 3 {
		t.Fatalf("index.html 引用的资源太少（%d 个），dist 可能不完整", len(refs))
	}
	for _, m := range refs {
		url := m[1]
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code=%d want 200", url, rec.Code)
			continue
		}
		ct := rec.Header().Get("Content-Type")
		switch {
		case strings.HasSuffix(url, ".js") && !strings.Contains(ct, "javascript"),
			strings.HasSuffix(url, ".css") && !strings.Contains(ct, "text/css"):
			t.Errorf("%s: Content-Type=%q", url, ct)
		}
		if strings.HasPrefix(url, "/panel/assets/") && !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: Cache-Control=%q want immutable", url, rec.Header().Get("Cache-Control"))
		}

		// 同一资源的 gzip 版本解压后必须与原文一致
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		gz := httptest.NewRecorder()
		p.ServeHTTP(gz, req)
		if rec.Body.Len() < 1024 {
			continue
		}
		if gz.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: 未返回 gzip", url)
			continue
		}
		zr, err := gzip.NewReader(gz.Body)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(plain, rec.Body.Bytes()) {
			t.Errorf("%s: gzip 解压结果与原文不一致（err=%v）", url, err)
		}
	}
}

// 资源路由只认 assets/ 下的真实文件：不存在的和路径穿越都 404。
func TestAssetNotFound(t *testing.T) {
	p := newTestPanel()
	for _, u := range []string{"/panel/assets/nope.js", "/panel/assets/..%2findex.html", "/panel/app.js"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d want 404", u, rec.Code)
		}
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
