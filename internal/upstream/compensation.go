// compensation.go 官方补偿活动的状态查询（/billing/meter/compensation-status）。
//
// 背景（issue #189，2026-10-16）：官方在「套餐与积分」页挂限时补偿横幅
// （例：「DeepSeek 模型服务异常额外补偿 500 积分」，带领取截止日），账号侧看得见，
// 网关这边却是**盲领**——ClaimCompensation 只在每日签到后顺带调一次，且写法是
//
//	if credit, err := ...; err == nil { log.Printf(...) }
//
// 于是三种完全不同的情形在日志里同构：① 没有补偿 ② 已经领过 ③ **真的领取失败**
// （401/5xx/网络）。补偿带截止日，签到被禁用 / 机器关机 / 账号被禁用时就会静默
// 过期，用户全程无感知。本文件补上「先查状态再决定是否领」所缺的那一半。
//
// 上游口径（实测 2026-10-16，cn 域 CLI 侧与 Web 侧均返回一致）：
//
//	{"code":0,"msg":"OK","data":{
//	   "claimed":false,          // 是否已领
//	   "active":true,            // 当前是否有生效中的补偿活动
//	   "credit_num":500,         // 可领额度
//	   "validity_period":1,      // 发放积分的有效期（单位上游未明示，前端也不渲染）
//	   "end_time":"2026-10-22 23:59:59"}}  // 领取截止
//
// active=false 是「当前无补偿活动」的正常 200 返回，不是错误——故此接口只在
// 传输失败 / 非 2xx / 业务 code 非 0 时返回 error。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// CompensationStatus 单账号的补偿活动状态。
type CompensationStatus struct {
	// Active 当前是否有生效中的补偿活动（无活动时为 false，属正常返回）。
	Active bool `json:"active"`
	// Claimed 本次活动是否已领取。
	Claimed bool `json:"claimed"`
	// CreditNum 待领额度（积分）。
	CreditNum int64 `json:"credit_num"`
	// ValidityPeriod 发放积分的有效期倍数。上游未明示单位，且前端 UI 从不渲染
	// 该字段（仅后端返回），故此处只透传不做换算，避免臆造语义。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止时间（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的补偿——即该调用 ClaimCompensation 的前置判据。
//
// 三者与门：active 为 false 是没有活动；claimed 为 true 是已领完（再调只会拿到
// 业务错误）；credit_num <= 0 是活动存在但无额度。三种都归为「无需领取」。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// GetCompensationStatus 查补偿活动状态。
//
// 走 billingJSON（billingBase），与既有 ClaimCompensation / ClaimGift 同域同头；
// 实测 CLI 域与 Web 域对该端点返回一致，故不必像 growth 领奖那样切 webBase。
func (c *Client) GetCompensationStatus(a *auth.Auth) (CompensationStatus, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/compensation-status", map[string]any{})
	if err != nil {
		return CompensationStatus{}, err
	}
	var st CompensationStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return CompensationStatus{}, err
	}
	return st, nil
}
