// tasks.go 面板「积分任务」接口：查询任务进度、接受任务、领取奖励。
//
// 上游能力（internal/upstream/tasks.go）的三层薄封装；前端表格展示
// current/target 进度与可领取状态，运维点按钮即可，无需外部 Python 脚本。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// acceptBatchGap 批量接受的批间节流（对齐脚本 1.05s 口径，避免上游风控）。
var acceptBatchGap = 1050 * time.Millisecond

// accountByUID 取账号凭证；不存在时写 404 并返回 nil。
func (p *Panel) accountByUID(w http.ResponseWriter, uid string) *auth.Auth {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil
	}
	return a
}

// accountTasks 查询单账号全量任务（进度/状态/可领取）。
func (p *Panel) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	realm := a.Realm()
	capabilities := map[string]bool{"accept": true, "claim": true, "automate": true}
	response := map[string]any{"ok": true, "realm": realm, "capabilities": capabilities, "tasks": tasks}
	if a.IsGlobal() {
		// Global task data has a read-only schema with code/task_id/status but no
		// verified accept, claim, or automation contract. Do not probe the MP API.
		capabilities["accept"], capabilities["claim"], capabilities["automate"] = false, false, false
		response["message"] = globalTaskWriteMessage
	} else {
		// 合并小程序口径任务（school_season / Sequential_Tasks_1 等仅在 mp 头列表下发）。
		// mp 列表是默认口径的超集（实测含常规任务），按 task_code 去重；失败不影响默认任务。
		if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
			seen := map[string]bool{}
			for _, t := range tasks {
				seen[t.TaskCode] = true
			}
			for _, t := range mpTasks {
				if !seen[t.TaskCode] {
					tasks = append(tasks, t)
					seen[t.TaskCode] = true
				}
			}
		}
	}
	response["tasks"] = tasks
	writeJSON(w, http.StatusOK, response)
}

// accountTaskAccept 接受任务（报名；幂等）。
func (p *Panel) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}
	if err := p.cfg.Upstream.AcceptTasks(a, body.TaskCodes); err != nil {
		writeErr(w, http.StatusBadGateway, "accept: "+err.Error())
		return
	}
	log.Printf("panel: 接受任务 uid=%s codes=%v", uid, body.TaskCodes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// taskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed 的）。
//
// 为什么值得做：accept 不产生进度（进度靠行为事件点亮），但让状态机规范
// （not_accepted → accepted → completed），也便于后续筛选"我报过名的任务"。
// 上游 scripts 的注释同样建议"先 accept"。
// 批量提交会分片（上游对 task_codes 数组长度无公开上限，保守每批 20 个）。
func (p *Panel) taskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	var codes []string
	for _, t := range tasks {
		// 跳过已完成/已领取/已接受的；locked 的也不碰（上游未开放）。
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "所有任务均已接受"})
		return
	}
	const batch = 20
	accepted := 0
	var failed []string
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		if err := p.cfg.Upstream.AcceptTasks(a, codes[i:end]); err != nil {
			log.Printf("panel: 批量接受失败 uid=%s codes=%v err=%v", uid, codes[i:end], err)
			failed = append(failed, codes[i:end]...)
			continue
		}
		accepted += end - i
		time.Sleep(acceptBatchGap) // 批间节流（对齐脚本 1.05s 口径）
	}
	// 小程序口径任务单独批量接受（默认列表不含 mp 码，accept 也要求 mp 头）。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			mpCodes = append(mpCodes, t.TaskCode)
		}
		if len(mpCodes) > 0 {
			if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
				log.Printf("panel: mp 批量接受失败 uid=%s err=%v", uid, err)
				failed = append(failed, mpCodes...)
			} else {
				accepted += len(mpCodes)
				time.Sleep(acceptBatchGap)
			}
		}
	}
	log.Printf("panel: 全部接受 uid=%s 接受=%d 失败=%d", uid, accepted, len(failed))
	resp := map[string]any{"ok": true, "accepted": accepted, "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "部分任务接受失败（上游拒绝），可重试"
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskClaim 领取任务奖励（未达标时上游返回业务错误，原样透出给前端提示）。
func (p *Panel) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	// 小程序口径任务走 chat 域 mp 头领奖（缺头实测不可领）；其余 Web 端接口。
	var credit, energy int64
	var err error
	if isMPTaskCode(body.TaskCode) {
		credit, energy, err = p.cfg.Upstream.ClaimRewardMP(a, body.TaskCode)
	} else {
		credit, energy, err = p.cfg.Upstream.ClaimReward(a, body.TaskCode)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+err.Error())
		return
	}
	if credit == 0 && energy == 0 {
		log.Printf("panel: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"})
		return
	}
	log.Printf("panel: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}
