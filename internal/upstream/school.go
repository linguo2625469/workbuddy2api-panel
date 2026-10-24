// school.go 开学季活动（school-season，活动期 2026-09-13 ~ 09-24）纯 API 自动化。
//
// 判据（2026-09-13 小程序 MCP 逆向 + 三账号实测，protocol.md §7.11）：
//   - share_invite（每日 +100c +1抽奖）：POST /tasks/share-complete {channel:"wechat"}
//     即点亮——纯前端上报，服务端不校验真实分享回执。本模块的主目标。
//   - chat_3_times / expert_use：判据绑定小程序原生沙箱会话（e2b runtime），
//     webchat 普通会话不计数，纯 API 不做（需小程序内人工对话）。
//   - 抽奖：POST /wheel/draw {draw_uuid}（前端生成 uuid，消耗 1 chance）。
//
// 端点基址 www.codebuddy.cn（billing 同域）；信封 {code,msg,data}，code=0 成功。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const schoolBase = "/portal/activity/school"

// schoolJSON 学院活动 API 请求（剥信封，业务 code≠0 返回带 msg 的 error）。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body map[string]any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+schoolBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

//   - chat_3_times：3 条 chat_request_send 即 3/3（conversationId 任意、桌面/mp
//     头族均可计数，无需真实沙箱会话）。
//   - expert_use：mp 指纹事件链 expert_summon_click + expert_summoned +
//     expert_actual_use + chat_request_send（开学季分类专家）即点亮。

const mpReportPath = "/v2/report"

// schoolOpenDayActivityID 开学季/校园日活动 id（事件 activityId 字段值，两域共用）。
const schoolOpenDayActivityID = "school_open_day_2026"

// mpEventBase 小程序埋点公共指纹（appservice wQ()+Ao() 对齐）。
func mpEventBase(a *auth.Auth) map[string]any {
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone":     "Asia/Shanghai",
		"userId":       a.UID,
		"userNickname": a.Nickname,
	}
}

// ReportMPEvent 以小程序指纹向 www.codebuddy.cn/v2/report 批量上报事件。
func (c *Client) ReportMPEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.BillingBaseCN+mpReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = c.doJSON(req)
	return err
}

// SchoolChatTimesEvents 构造一条 chat_request_send 事件（chat_3_times 计数）。
func SchoolChatTimesEvents(conversationID string) map[string]any {
	rid := clientToken()
	return map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// SchoolSeasonChatEvent 构造 growth 域「校园日」（school_season）判据事件：
// mini 指纹 chat_request_send + activityId=school_open_day_2026（与 school 域
// 开学季同 activityId 关联；实测无 activityId 的事件不点亮）。事件形状与
// SchoolChatTimesEvents 同构（school 域 chat_3_times 同款），仅追加 activityId。
func SchoolSeasonChatEvent(conversationID string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["activityId"] = schoolOpenDayActivityID
	return ev
}

// MiniExpertUseEvent 构造 growth 域 Sequential_Tasks_2「在小程序内选中专家并完成
// 有效对话」的判据事件：mp 指纹 expert_actual_use。形状对齐小程序源码
// app-service.js 的真实发射点（上游 task_runner 实测 2026-09-23：上报即 completed，
// claim +200c+5e）。与 school 域的 SchoolExpertUseEvents 是**两套口径**，勿照抄：
//   - 不带 conversationId/activityId——真实事件就是这两个字段都不带；
//   - extVersion 用小程序自身版本 2.2.8（覆盖 mpEventBase 的 2.4.0）；
//   - source=mini_program + type 固定 "send_message"（小程序恒发此值）。
//
// expertID 必须是专家市场真实 ex_ id（ListMarketExperts），空 id 服务端不入账。
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniChatModelEvent mp 对话事件 + 模型字段（Sequential_Tasks_5「使用 GLM5.2」判据
// 载体）：小程序 chat_request_send 真实发射点（mpsrc main 32904 模块）带
// requestModelId / requestModelName——Tasks_1/3 的裸对话事件不带模型，模型任务
// 须用本形态（判据待解锁实测验证）。
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniPlaybookEvents mp 指纹灵感事件组（Sequential_Tasks_7「体验灵感功能」判据
// 载体，形状对齐 mpsrc main 73640/73665 发射点：playbook_cta_click →
// playbook_prompt_send）。issue #42 称该任务为 PC 口径（+500c+5e）——PC 序列
// （DesktopPlaybookPromptSequence）已实测点亮 playbook_prompt，本组作为 mp 形态
// 补充（任务在 mp 链上，判据究竟认哪侧待解锁实测）。
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	cta := map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": "2.2.8",
	}
	for k, v := range base {
		cta[k] = v
	}
	send := map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": clientToken(),
		"extVersion":     "2.2.8",
	}
	for k, v := range base {
		send[k] = v
	}
	return []map[string]any{cta, send}
}

// ---- 我的券码（#/prizes?tab=vouchers，2026-09-16 接入）----

// SchoolVoucher 开学季抽奖抽中的第三方券（KFC/瑞幸/酷狗等）。
// 字段结构按真实响应样本：GET /vouchers 单次拉全（无分页），data.items[]。
type SchoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	DrawUUID  string `json:"draw_uuid,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`   // kfc_ice_cream / voucher_luckin / voucher_kugou …
	PrizeName string `json:"prize_name,omitempty"` // 肯德基冰淇淋
	Code      string `json:"code"`                 // 券码本体（复制给店员核销）
	ValidFrom string `json:"valid_from,omitempty"` // 上游常为空
	ValidTo   string `json:"valid_to,omitempty"`   // "2026-10-24"
	GrantedAt string `json:"granted_at,omitempty"` // RFC3339
}

// SchoolVouchers 查询账号的开学季券码列表（只读）。
// 抽到积分的记录不在此端点（那是 /rewards 的 type=credit 条目）。
func (c *Client) SchoolVouchers(a *auth.Auth) ([]SchoolVoucher, error) {
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/vouchers", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
