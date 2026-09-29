package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 前端逻辑（积分包分组、到期汇总、配置表单、账号状态等）的单元测试在 web/src/lib/*.test.ts，
// 由 vitest 运行（npm run build 的一部分，CI 在 go-binaries 的 web 任务里跑）；这里只测 Go 端输出的页面。
// 本测试在前端已构建（真实 index.html）和未构建（构建指引页）两种状态下都要成立。

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
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
