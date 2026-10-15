// bridge.go 统一网关桥接：把 zcode server 的内部能力以导出方法形式暴露，
// 供主网关（workbuddy2api-panel）做模型分流、/v1/models 合并与健康判定。
package gateway

import (
	"net/http"
	"time"
)

// DispatchChat 把请求按本服务的 /v1/chat/completions 处理
// （含 withGatewayAuth 鉴权——主网关传来的 Bearer 与 AuthToken 一致）。
func (s *Server) DispatchChat(w http.ResponseWriter, r *http.Request) {
	s.withGatewayAuth(s.handleChatCompletions)(w, r)
}

// KnownModel 判断 model（任意大小写/别名）是否命中 zcode 目录。
func (s *Server) KnownModel(model string) bool {
	if model == "" {
		return false
	}
	return s.catalog.Resolve(model) != model
}

// HealthyCount 返回可参与调度的账号数（status 空/未知 + 未冷却）。
// 上游把额度耗尽标成 cooling_until；expired/revoked 的行已被 deactivate。
func (s *Server) HealthyCount() int {
	accts, err := s.store.Accounts()
	if err != nil {
		return 0
	}
	n := 0
	now := time.Now().Unix() // CoolingUntil 是秒级 Unix 时间戳
	for _, a := range accts {
		if !a.Active || a.Status == "expired" || a.Status == "revoked" {
			continue
		}
		if a.CoolingUntil > 0 && a.CoolingUntil > now {
			continue
		}
		n++
	}
	return n
}

// AccountCount 返回池内账号总数（含停用/过期；0 = 池为空）。
func (s *Server) AccountCount() int { return s.store.Count() }

// ModelListJSON 返回 zcode 目录模型（OpenAI /v1/models 条目格式），
// 供统一网关合并进总的 /v1/models 响应。
func (s *Server) ModelListJSON() []map[string]any {
	infos := s.catalog.List()
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		entry := map[string]any{
			"id":       mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "zcode-glm",
			"name":     mi.Name,
		}
		if mi.ContextWindow > 0 {
			entry["context_length"] = mi.ContextWindow
		}
		out = append(out, entry)
	}
	return out
}

// Warm 在后台做一次启动预热：调度器 + 计费刷新（对齐 ListenAndServe 里
// 的 afterListen，但不监听端口——端口由主网关统一提供）。
func (s *Server) Warm() {
	s.afterListen()
}
