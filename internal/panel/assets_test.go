package panel

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// newPanelWithAssets 用指定的文件系统充当 dist/，让页面与资源的服务逻辑不依赖真实的前端构建。
func newPanelWithAssets(fsys fs.FS) *Panel {
	p := newTestPanel()
	p.assets = fsys
	return p
}

func get(p *Panel, method, url string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// dist 里没有产物（编译前没构建前端）：页面给出构建指引（503，不是空白页或 panic），
// 资源 404，安全头照常，网关的 API 不受影响。
func TestPanelWithoutFrontendBuild(t *testing.T) {
	p := newPanelWithAssets(fstest.MapFS{})

	rec := get(p, "GET", "/panel/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /panel/ code=%d want 503", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"面板前端还没有构建", "npm run build"} {
		if !strings.Contains(body, want) {
			t.Errorf("提示页缺少 %q", want)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("提示页不得含脚本")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type=%q", ct)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("提示页也必须带 CSP")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("提示页不该被缓存：Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}

	for _, u := range []string{"/panel/theme-init.js", "/panel/assets/index-abc.js"} {
		if c := get(p, "GET", u).Code; c != http.StatusNotFound {
			t.Errorf("%s code=%d want 404", u, c)
		}
	}
	if c := get(p, "GET", "/panel/api/overview").Code; c != http.StatusUnauthorized {
		t.Errorf("API 应照常鉴权：code=%d want 401", c)
	}
}

// 用合成的 dist 验证页面与资源的服务逻辑：类型、缓存头、gzip、HEAD、路径穿越。
func TestServeAssetsFromSyntheticDist(t *testing.T) {
	bigJS := strings.Repeat("console.log('panel');\n", 200)
	bigCSS := strings.Repeat("a{color:red}\n", 200)
	p := newPanelWithAssets(fstest.MapFS{
		"index.html":         {Data: []byte(`<!doctype html><script type="module" src="/panel/assets/app-abc.js"></script>`)},
		"theme-init.js":      {Data: []byte("(function(){})()")},
		"assets/app-abc.js":  {Data: []byte(bigJS)},
		"assets/app-abc.css": {Data: []byte(bigCSS)},
	})

	// 页面：no-cache，引用的入口脚本存在
	rec := get(p, "GET", "/panel/")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index code=%d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	// 首屏主题脚本：不带哈希，不做长缓存
	rec = get(p, "GET", "/panel/theme-init.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("theme-init code=%d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	// 带哈希的资源：类型正确、永久缓存
	for url, wantType := range map[string]string{
		"/panel/assets/app-abc.js":  "text/javascript",
		"/panel/assets/app-abc.css": "text/css",
	} {
		rec := get(p, "GET", url)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d", url, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s Content-Type=%q want %s", url, ct, wantType)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control=%q want immutable", url, cc)
		}
		if rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s 客户端没声明 gzip，不该压缩", url)
		}

		// 声明 gzip：返回压缩版本，解压后与原文一致，Content-Length 与实际一致
		gz := get(p, "GET", url, "Accept-Encoding", "gzip, deflate, br")
		if gz.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s 未返回 gzip", url)
		}
		if gz.Body.Len() >= rec.Body.Len() {
			t.Errorf("%s 压缩后 %d 字节没有比原文 %d 字节小", url, gz.Body.Len(), rec.Body.Len())
		}
		if got := gz.Header().Get("Content-Length"); got != strconv.Itoa(gz.Body.Len()) {
			t.Errorf("%s Content-Length=%s 与实际 %d 不符", url, got, gz.Body.Len())
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(plain, rec.Body.Bytes()) {
			t.Errorf("%s gzip 解压结果与原文不一致（err=%v）", url, err)
		}
	}

	// HEAD：有头无体
	head := get(p, "HEAD", "/panel/assets/app-abc.js")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD code=%d body=%d", head.Code, head.Body.Len())
	}

	// 小文件不压缩（压缩反而更大）
	small := get(p, "GET", "/panel/theme-init.js", "Accept-Encoding", "gzip")
	if small.Header().Get("Content-Encoding") != "" {
		t.Error("小文件不该压缩")
	}

	// 不存在的、路径穿越的都 404
	for _, u := range []string{"/panel/assets/nope.js", "/panel/assets/..%2findex.html", "/panel/assets/%2e%2e/index.html"} {
		if c := get(p, "GET", u).Code; c != http.StatusNotFound {
			t.Errorf("%s code=%d want 404", u, c)
		}
	}
}

// 真实的前端构建产物（CI 与本地构建过前端时才有；没构建就跳过，纯 Go 环境的 go test 不受影响）：
// 页面必须以同源外链加载入口模块，引用的每个脚本与样式都能取到、类型正确，否则页面白屏。
func TestBuiltDistIsServable(t *testing.T) {
	index, err := fs.ReadFile(embeddedDist(), "index.html")
	if err != nil {
		t.Skip("前端未构建（internal/panel/dist 为空）：cd web && npm ci && npm run build 后再跑本测试")
	}
	p := newTestPanel()
	body := string(index)

	if !strings.Contains(body, `<script type="module" crossorigin src="/panel/assets/`) {
		t.Error("index.html 必须以同源外链加载入口模块（内联脚本会被 CSP 拦掉）")
	}
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html 含内联 <script> 块，CSP 会拦截")
	}

	refs := regexp.MustCompile(`(?:src|href)="(/panel/[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(refs) < 3 {
		t.Fatalf("index.html 引用的资源只有 %d 个，dist 可能不完整", len(refs))
	}
	for _, m := range refs {
		url := m[1]
		rec := get(p, "GET", url)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code=%d want 200", url, rec.Code)
			continue
		}
		ct := rec.Header().Get("Content-Type")
		if strings.HasSuffix(url, ".js") && !strings.Contains(ct, "javascript") ||
			strings.HasSuffix(url, ".css") && !strings.Contains(ct, "text/css") {
			t.Errorf("%s: Content-Type=%q", url, ct)
		}
	}
}
