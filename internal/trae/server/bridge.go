// bridge.go 合并网关桥接：把 trae handler 的内部能力以导出方法形式暴露，
// 供统一网关（workbuddy2api-panel）做模型分流与 /v1/models 合并。
package server

import "net/http"

// KnownModel 判断 model 是否在 trae 模型表（动态列表优先，失败回退静态表）。
// 统一网关用它判定 chat 请求是否分流到 trae 上游。
func (h *Handler) KnownModel(model string) bool {
	return h.knownModel(model)
}

// ModelList 返回 trae 模型列表（OpenAI /v1/models 条目格式，owned_by=trae-solo），
// 供统一网关合并进总的 /v1/models 响应。
func (h *Handler) ModelList() []map[string]any {
	return h.modelList()
}

// adminSchedule 返回排程状态（小时点 / 下次触发 / 最近执行）。
func (h *Handler) adminSchedule(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cfg.Sched.Status())
}

// adminScheduleCheckin 手动触发一轮签到 + 积分刷新 + 解冻（异步执行，立即返回）。
func (h *Handler) adminScheduleCheckin(w http.ResponseWriter, r *http.Request) {
	go h.cfg.Sched.RunCheckinNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": "checkin"})
}

// adminScheduleRefresh 手动触发一轮 Token 刷新（异步执行，立即返回）。
func (h *Handler) adminScheduleRefresh(w http.ResponseWriter, r *http.Request) {
	go h.cfg.Sched.RunRefreshNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": "refresh"})
}
