// search.go 联网搜索的轮转内核。
//
// 定位：把上游 /agenttool/v1/search 的调用接进网关的账号池语义（选号 / 在途租约 /
// token 刷新 / 错误策略 / 轮转退避），与 chat 路径共享同一套账号健康语义——搜索
// 失败同样会按 Kind 冷却或熔断对应账号。
//
// 对外只暴露一个协议入口：POST /v1/search/messages（Anthropic Messages 兼容形态，
// 见 messages.go）。不再提供自创的 OpenAI 风格 /v1/search——OpenAI 没有该端点，
// 任何 OpenAI 系客户端都不会调用它，多一个入口只是多一份要维护的契约。
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchToolLabel 搜索流量在请求日志「模型」列里的标识。
// 搜索不是模型调用，但该列留空会让面板无法区分这类流量与异常请求。
const searchToolLabel = "WorkBuddy搜索工具"

// searchFailure 轮转全部失败后的出口状态（由调用方渲染成对应协议的错误形态）。
type searchFailure struct {
	status int
	msg    string
	hint   string
}

// searchMetaFromRequest 从入站请求头提取会话头族并补全聚合主键。
//
// 搜索通常由某个会话内的 agent 发起，入站若带 X-Conversation-ID /
// X-Conversation-Request-ID / X-Trace-ID 必须原样透传给上游——上游按会话聚合
// 搜索流量，缺失会被视为请求不完整。客户端没带时才本地生成聚合主键；
// ConversationID 保持空串（不伪造一个假会话，避免污染后台真实会话聚合）。
func searchMetaFromRequest(r *http.Request) upstream.ChatMeta {
	meta := upstream.ChatMeta{
		ConversationID:        strings.TrimSpace(r.Header.Get("X-Conversation-ID")),
		ConversationRequestID: strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID")),
		TraceID:               strings.TrimSpace(r.Header.Get("X-Trace-ID")),
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = session.NewMessageID()
	}
	return meta
}

// runSearch 带账号轮转执行一次搜索，成功返回结果，失败返回出口状态。
//
// 轮转语义与 chat 完全一致：PickExcluding → 在途租约 → token 预刷新 →
// 上游调用 → 按 ErrKind 走 applyErrorPolicy → 抖动退避换号。
func (h *Handler) runSearch(ctx context.Context, query string, maxResults int, meta upstream.ChatMeta) (*upstream.SearchOutcome, *searchFailure) {
	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastKind upstream.ErrKind

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		// 在途租约：与 chat 共用同一名额池。搜索也是真实上游调用，不占名额会让
		// 搜索流量绕过 MaxInFlight 限流。
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, ctx) {
				break
			}
			continue
		}
		// token 临近过期先刷新：搜索与 chat 共用同一 accessToken，把过期 token
		// 打到搜索端点会换回 401，被 applyErrorPolicy 当账号故障处置、误伤健康号。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				h.cfg.Pool.Release(acct.UID)
				lastErr = rerr
				var ue *upstream.Error
				if errors.As(rerr, &ue) {
					lastStatus, lastKind = ue.Status, ue.Kind
					h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, "", ue)
				} else {
					h.cfg.Pool.NoteFailures(acct.UID)
					lastStatus = http.StatusServiceUnavailable
				}
				if !rotateBackoff(i, ctx) {
					break
				}
				continue
			}
			if serr := acct.SaveAtomic(); serr != nil {
				// 刷新成功但落盘失败：下次启动会拿旧 token，必须暴露。
				log.Printf("ERR: [server] search refresh acct=%s: save auth failed: %v",
					logfmt.Label(acct.UID, acct.Nickname), serr)
			}
		}

		// 记账：搜索没有 chatStat，账号信息直接写进 trace，面板才能看出「这次搜索用了哪个号」。
		if tr := requestTraceFromContext(ctx); tr != nil {
			tr.account = logfmt.Label(acct.UID, acct.Nickname)
		}
		// meta 在循环外构造（会话头透传 + 轮级聚合键），此处直接复用：
		// 同一次搜索的换号重试属于同一请求，必须复用同一组会话 ID。
		out, serr := h.cfg.Upstream.Search(ctx, acct, query, maxResults, meta)
		h.cfg.Pool.Release(acct.UID)
		if serr == nil {
			return out, nil
		}
		lastErr = serr
		var ue *upstream.Error
		if errors.As(serr, &ue) {
			lastStatus, lastKind = ue.Status, ue.Kind
			// 与 chat 复用同一错误策略：余额不足/限流/WAF/账号故障各自冷却或熔断，
			// model 传空——搜索没有模型维度，不写模型级冷却。
			h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, "", ue)
		} else {
			// 传输层故障（非上游业务返回）：只喂连败计数，不按业务错误罚。
			h.cfg.Pool.NoteFailures(acct.UID)
			lastStatus = http.StatusServiceUnavailable
		}
		if !rotateBackoff(i, ctx) {
			break
		}
	}

	// 末端出口（与 chat 同口径）：
	//   - 选不到号 / 传输层失败 → 503 no_healthy_account
	//   - 上游错误 → 原文透传；限流语义映射 429，其余保留上游状态码（4xx 透传）
	// gateway_hint 同样与 chat 对齐：本地调度错误给固定提示，上游错误按 Kind 判定，
	// 让运维在一个字段里就能分清「池子没号」和「上游报错」。
	hint := upstream.NoHealthyAccountHint()
	if lastErr == nil {
		return nil, &searchFailure{
			status: http.StatusServiceUnavailable,
			msg:    "all accounts are temporarily unavailable, please retry later",
			hint:   hint,
		}
	}
	status := http.StatusServiceUnavailable
	if lastStatus >= 400 && lastStatus < 500 {
		status = lastStatus
	}
	if lastKind == upstream.ErrSoftRate {
		status = http.StatusTooManyRequests
	}
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		// 搜索无模型维度（bareModel 传空、无图片判定），其余判定口径与 chat 一致。
		return nil, &searchFailure{
			status: status,
			msg:    ue.Msg,
			hint:   h.hintOf(ue.Kind, ue.Msg, "", false, ue),
		}
	}
	return nil, &searchFailure{
		status: status,
		msg:    lastErr.Error(),
		hint:   hint,
	}
}
