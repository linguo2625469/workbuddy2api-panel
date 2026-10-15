package gateway

// webui.go — the dashboard SPA (internal/gateway/webui) embedded into the
// binary and served with no-store: separate files (index / proxies /
// settings / app.css / app.js) instead of the old single-string page.

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed webui
var webuiFS embed.FS

// serveWebUI serves the embedded SPA: "/" → index.html, plus /app.css,
// /app.js, /proxies.html, /settings.html. Registered as the "/" catch-all —
// exact /api/* and /v1/* routes win over it.
func serveWebUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	path := r.URL.Path
	if path == "/" || path == "" {
		path = "/index.html"
	}
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := webuiFS.ReadFile("webui" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(b)
}
