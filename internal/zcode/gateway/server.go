package gateway

import (
	"log"
	"net/http"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.withAuth(s.apiStatus))
	mux.HandleFunc("/api/accounts", s.withAuth(s.apiAccounts))
	mux.HandleFunc("/api/accounts/add", s.withAuth(s.apiAccountsAdd))
	mux.HandleFunc("/api/accounts/remove", s.withAuth(s.apiAccountsRemove))
	mux.HandleFunc("/api/accounts/select", s.withAuth(s.apiAccountsSelect))
	mux.HandleFunc("/api/accounts/import", s.withAuth(s.apiAccountsImport))
	mux.HandleFunc("/api/quota/refresh", s.withAuth(s.apiQuotaRefresh))
	mux.HandleFunc("/api/accounts/refresh", s.withAuth(s.apiAccountsRefreshOne))
	mux.HandleFunc("/api/accounts/claim-all", s.withAuth(s.apiAccountsClaimAll))
	mux.HandleFunc("/api/accounts/claim-one", s.withAuth(s.apiAccountsClaimOne))
	mux.HandleFunc("/api/accounts/edit", s.withAuth(s.apiAccountsEdit))
	mux.HandleFunc("/api/accounts/toggle", s.withAuth(s.apiAccountsToggle))
	mux.HandleFunc("/api/login/start", s.withAuth(s.apiLoginStart))
	mux.HandleFunc("/api/login/poll/", s.withAuth(s.apiLoginPoll))
	mux.HandleFunc("/api/settings", s.withAuth(s.apiSettings))
	mux.HandleFunc("/api/accounts/export", s.withAuth(s.apiAccountsExport))
	mux.HandleFunc("/api/monitor", s.withAuth(s.apiMonitor))
	mux.HandleFunc("/api/proxies", s.withAuth(s.apiProxyList))
	mux.HandleFunc("/api/proxies/save", s.withAuth(s.apiProxySave))
	mux.HandleFunc("/api/proxies/delete", s.withAuth(s.apiProxyDelete))
	mux.HandleFunc("/api/proxies/probe", s.withAuth(s.apiProxyProbe))
	mux.HandleFunc("/api/accounts/restore", s.withAuth(s.apiAccountsImportJSON))
	mux.HandleFunc("/v1/models", s.withGatewayAuth(s.handleModels))
	mux.HandleFunc("/v1/usage", s.withGatewayAuth(s.handleUsage))
	mux.HandleFunc("/v1/messages", s.withGatewayAuth(s.handleMessages))
	mux.HandleFunc("/async/v1/messages", s.withGatewayAuth(s.handleAsyncMessages))
	mux.HandleFunc("/v1/chat/completions", s.withGatewayAuth(s.handleChatCompletions))
	return cors(mux)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe() error {
	addr := s.cfg.Host + ":" + s.cfg.Port
	log.Printf("zcode2api http://%s/  (dashboard)", addr)
	log.Printf("  POST /v1/chat/completions  (OpenAI)")
	log.Printf("  POST /v1/messages          (Anthropic)")
	log.Printf("  GET  /v1/models")
	log.Printf("[pool] %d accounts (quota refresh is the dashboard button)", s.store.Count())
	go s.afterListen()
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) afterListen() {
	s.importKeysFile()
	s.startSchedulers()
	if n := s.store.MarkEmptyFreeExpired(); n > 0 {
		log.Printf("[pool] marked %d stored empty Free as expired (hidden, kept)", n)
	}
	// pull the actual model list + quotas + model metadata from the origin in
	// the background so /v1/models reflects the server, not the fallback
	go func() {
		s.refreshAllBilling()
		s.enrichCatalogFromConfigs()
	}()
}
