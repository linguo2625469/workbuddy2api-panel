package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIndexHTMLNoInlineScript 构建产物 index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
//
// 说明：旧 app.js 时代的 node --check 已退役——vite 构建本身即语法校验
// （构建失败则无产物，ServeHTTP 直接 500），比事后 node 校验更强。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /panel/ code=%d want 200", rec.Code)
	}
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}
