// index.go 面板静态资源与安全响应头。
//
// 前端是 Svelte + Tailwind + Vite 工程（internal/panel/frontend），构建产物
// 落在 internal/panel/dist（index.html + assets/，hash 文件名），经 go:embed
// 打进二进制，与服务同体部署、无运行时外部依赖。产物提交进库，Docker 构建
// 时由 node 阶段重新生成（见 Dockerfile），两者内容一致。
//
// 本地开发：frontend/ 下 npm install && npm run build；联调用 npm run dev
// （/panel/api 代理到本地 127.0.0.1:7863 网关）。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"embed"
	"io/fs"
	"net/http"
)

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（vite 构建产物）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：Svelte 动态样式（进度条宽度、占比条）经 style 属性
//     输出，允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
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

//go:embed dist
var distFS embed.FS

// distSub 去掉 dist 前缀的子文件系统（供页面读取与 FileServer 直接服务）。
func distSub() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// index.html 由 vite 构建生成：只含外链脚本/样式（无内联 <script>），
// 与 CSP script-src 'self' 兼容。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	raw, err := fs.ReadFile(distSub(), "index.html")
	if err != nil {
		http.Error(w, "panel assets missing (rebuild frontend)", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// assets 输出构建产物（hash 文件名，immutable 长缓存）。
func (p *Panel) assets(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.StripPrefix("/panel/", http.FileServer(http.FS(distSub()))).ServeHTTP(w, r)
}
