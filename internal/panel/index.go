// index.go 面板静态资源与安全响应头。
//
// 前端源码在仓库根目录 web/（React + HeroUI），构建产物输出到本目录的 dist/ 并提交进仓库，
// 经 go:embed 打进二进制：只装了 Go 的用户照样 go build 就能得到完整面板。
// 改了 web/ 必须重新 npm run build 并把 dist/ 一起提交（CI 会重新构建并比对）。
//
//   - dist/index.html      页面骨架（无内联脚本）
//   - dist/theme-init.js   首屏前套上明暗主题
//   - dist/assets/*        打包后的脚本与样式，文件名带内容哈希，可永久缓存
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed all:dist
var distFS embed.FS

// indexHTML 页面骨架（dist 缺失时编译期就会失败，这里不会读不到）。
var indexHTML = mustReadDist("index.html")

func mustReadDist(name string) []byte {
	b, err := distFS.ReadFile("dist/" + name)
	if err != nil {
		panic("panel: 前端产物缺失 dist/" + name + "：在 web/ 下运行 npm run build 后重新编译")
	}
	return b
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（dist/assets/*.js、theme-init.js）；页面无内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：组件库与图表会写 style 属性（进度条宽度、图表尺寸），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// 不缓存：发版后浏览器要立刻拿到引用新哈希文件名的页面。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	serveBytes(w, r, "index.html", indexHTML)
}

// themeInit 首屏主题脚本（文件名不带哈希，不做长缓存）。
func (p *Panel) themeInit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	serveBytes(w, r, "theme-init.js", mustReadDist("theme-init.js"))
}

// asset 输出打包后的脚本与样式。文件名带内容哈希，可永久缓存。
func (p *Panel) asset(w http.ResponseWriter, r *http.Request) {
	name := "assets/" + r.PathValue("file")
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	b, err := distFS.ReadFile("dist/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	serveBytes(w, r, name, b)
}

// contentTypes 常见扩展名写死：mime.TypeByExtension 在 Windows 上读注册表，
// 被别的软件改过时 .js 会变成 text/plain，模块脚本会因严格 MIME 检查被浏览器拒绝加载（白屏）。
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

// gzipped 压缩结果缓存（嵌入文件内容不变，每个文件只压一次）。
var gzipped sync.Map // name → []byte

// serveBytes 按扩展名写 Content-Type；客户端接受 gzip 且文件不小时返回压缩版本
// （打包后的脚本与样式压缩后只剩三成左右，面板常经远程 http 访问）。
func serveBytes(w http.ResponseWriter, r *http.Request, name string, b []byte) {
	ct := contentTypes[path.Ext(name)]
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(name))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Add("Vary", "Accept-Encoding")
	if len(b) >= 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		b = gzipOnce(name, b)
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func gzipOnce(name string, b []byte) []byte {
	if v, ok := gzipped.Load(name); ok {
		return v.([]byte)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	out := buf.Bytes()
	gzipped.Store(name, out)
	return out
}
