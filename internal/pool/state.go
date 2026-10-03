// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.disableLocked(e, reason)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.sessionDeadFails = 0
	p.disableLocked(e, sessionDeadReason)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// Revive 运维口径的"无条件恢复"：清禁用、冷却（含软退避计数）与熔断运行态。
// 与 ReviveDisabled（只清禁用）和 ReenableIfCredits（只清冷却、不动熔断）的区别：
// 本方法清除全部惩罚状态，供管理面板"解冻"按钮使用——人工判断该号可用时一键恢复。
// uid 不存在返回 false（供调用方区分"账号不存在"与"已复活"）。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 模型级限流豁免随冷却一并清（防泄漏到后续账号级限流）
	e.sessionDeadFails = 0
	e.fails = 0
	e.retryCount = 0
	e.breakerUntil = time.Time{}
	p.dirty.Store(true)
	return true
}

// reviveCoolingLocked 只解冻余额耗尽冷却（CoolHard 的 until/coolKind/reason）并更新
// credits，不动熔断器（fails/retryCount/breakerUntil）、软限流退避（CoolSoft/softStreak）
// 与模型级台账（modelCooldowns）——限流冷却的恢复证据是重置墙钟到期，不是余额恢复。
// 签到/余额刷新解冻走这里：余额恢复只证明 billing 通道健康，不证明 chat 通道健康，
// 熔断（连续 5xx 信号）与限流冷却均不应被余额刷新覆盖。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain, total)
		} else {
			e.credits = remain
			e.creditsTotal = total
		}
		e.creditsRemainder = 0
		// ReenableIfCredits 只有聚合余额上下文；到期明细必须由 SetCreditsDetailed
		// 重新写入，不能沿用旧窗口/旧批次的缓存。
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemaining = 0
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails 与连败降权计数（consecutiveFails/degradeUntil，issue #114）：
// 成功证明账号当前可用，连败计数与临时出池截止一并归零。
// **不碰 modelCooldowns**：6004 模型级 limit 每模型独立计时，其他模型成功不得抹掉
// 本模型的冷却截止（这正是"每模型独立"的语义）。模型级冷却只由到期/复活/账号级
// 冷却（Cooldown/reviveCoolingLocked）清除。11102 条目的成功清理由 handler 在
// 成功且模型命中时显式调 BlockModelClear（6004 不清，语义不同）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		p.dirty.Store(true)
	}
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本，并顺带
// 扣减账号余额（credits/creditsExpiring/最早到期批次）。credit 为上游 usage.credit（本次真实
// 扣费=消耗量），tokens 为本次请求的 token 总数（prompt+completion，用于折算单位
// 成本）。tokens<=0 时不记录：无法折算单价，记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本持久化到 state.json（stateAccount.ModelCosts）：重启后成本知识保留，
// 限免/夜间免费的跨重启窗口不再重新付学费探测；落盘/恢复均按 modelCostTTL
// 惰性过滤——陈旧价格（时段性优惠）不跨 TTL 复活。
// 限免结束事件：tier 0 观测（per1k≤0）被 credit>0 观测覆盖时打一条明确日志
// （运维据此知道"免费午餐结束了"），判定在写入口做、只看覆盖前值。
//
// credits 签到外回写：credit 是本次请求的**消耗量**，不是剩余余额。顺手扣减
// credits 与到期快照，让选号余额因子随消耗实时收敛——旧口径只在签到
// （每天 09:00/21:00 两次）刷新，两次签到之间（最长 12h）高消耗号持续高权重直到
// 打空撞 402；global 账号不签到，credits 曾是终身冻结。签到仍定期覆盖
// （ReenableIfCredits/SetCreditsDetailed 以 authoritative 余额重置），扣减只是
// 两次签到之间的内插估计；credit=0（免费请求）不动余额。
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 || math.IsNaN(credit) || math.IsInf(credit, 0) || credit < 0 {
		return
	}
	// 单价按每千 token 归一，消除请求长度差异。
	per1k := credit / float64(tokens) * 1000
	if math.IsInf(per1k, 0) {
		per1k = math.MaxFloat64
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if credit > 0 {
		oldCredits := e.credits
		e.setExactCredits(e.exactCredits() - credit)
		d := oldCredits - e.credits // 到期快照仍以既有整数口径递减
		if e.creditsExpiring > 0 {
			if d > e.creditsExpiring {
				e.creditsExpiring = 0
			} else {
				e.creditsExpiring -= d
			}
		}
		if e.creditsEarliestRemaining > 0 {
			if d >= e.creditsEarliestRemaining {
				e.creditsEarliestRemaining = 0
				e.creditsEarliestExpiry = time.Time{}
			} else {
				e.creditsEarliestRemaining -= d
			}
		}
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1, CreditVerified: true}
	} else {
		// 限免结束事件（判定在写入口，只看覆盖前值）：此前 tier 0（实测免费，
		// per1k≤0）且本次实测收费（per1k>0）——账号在该模型上的免费窗口结束。
		if prev.CostPer1k <= 0 && per1k > 0 {
			log.Printf("[pool] model %s on uid %s: free tier ended, now %.3f credits/1k", model, logfmt.UID8(uid), per1k)
		}
		e.modelCost[model] = modelCostEntry{
			CostPer1k:      prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:       time.Now(),
			Samples:        prev.Samples + 1,
			CreditVerified: true,
		}
	}
	p.dirty.Store(true) // 账本已持久化：写入口统一置脏
}

// RecordTokenUsage 记录一次实际发起的聊天账号尝试及上游返回的 usage 增量。
// usage 字段缺失时仍累计请求次数，但只累计明确存在的 token 字段。
func (p *Pool) RecordTokenUsage(uid string, delta TokenUsageDelta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	usage := &e.tokenUsage
	usage.RequestCount++
	usage.LastUsedAt = time.Now()
	if delta.Model != "" {
		usage.LastModel = delta.Model
	}
	known := false
	if delta.HasPromptTokens && delta.PromptTokens >= 0 {
		usage.PromptTokens += delta.PromptTokens
		known = true
	}
	if delta.HasCompletionTokens && delta.CompletionTokens >= 0 {
		usage.CompletionTokens += delta.CompletionTokens
		known = true
	}
	if delta.HasTotalTokens && delta.TotalTokens >= 0 {
		usage.TotalTokens += delta.TotalTokens
		known = true
	}
	if known {
		usage.UsageCount++
	}
	if delta.HasLatencyMs && delta.LatencyMs >= 0 {
		usage.LastLatencyMs = delta.LatencyMs
	}
	if delta.HasTokensPerSecond && delta.TokensPerSecond >= 0 {
		speed := delta.TokensPerSecond
		usage.LastTokensPerSecond = &speed
	} else {
		// 失败或缺少 completion_tokens 时不展示上一次请求的旧吞吐速度。
		usage.LastTokensPerSecond = nil
	}
	p.dirty.Store(true)
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入（模型豁免）。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 同 PickByUID，但用 healthyForModel 校验：绑定号在当前模型被
// 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换。
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	// 积分保底（粘性路径）：与 pick 的 floorBlocked 同判据——触底 + 收费即拦
	// （判据含上游目录倍率兜底，realm 取账号所属域——粘性号已确定，无需外部传入）。
	// 返回 nil 后 handler 侧解绑粘性（unbindSticky）走普通轮换换号，粘性号回血
	// 后下次会话重新绑定。
	// 日志频次：天然每请求至多一条——首次返回 nil 即解绑，后续轮转不再调入本路径
	// （无需额外节流）；粘性续期中每个新请求一条，恰好是「余额仍在线下」的持续提醒。
	if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
		log.Printf("WARN: [pool] credit floor: sticky acct=%s model=%s credits=%d < floor=%d, unbind (paid model held out)",
			logfmt.Label(e.a.UID, e.a.Nickname), model, e.credits, p.creditFloor)
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm 同 CountsDetailed，但仅统计 Realm()==realm 的账号；
// realm=="" 不加谓词（= CountsDetailed）。供 /status 按域分组透出。
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm 是两函数共用的遍历实现；realm=="" 不加谓词。
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个 healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
func (p *Pool) ServableNow() bool {
	return p.ServableForRealm("")
}

// ServableForRealm 报告某 realm 是否可服务：存在至少一个该 realm 的 healthy 且未占满在途名额的账号。
// 与 ServableNow 同口径（healthy 或模型豁免、排除 inFlightFull），仅叠加 Realm()==realm 谓词。
// realm=="" 退化为 ServableNow（现状语义）。供 /healthz 按 realm 暴露 CN/global 各自可达性。
func (p *Pool) ServableForRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		// 存在性语义：账号级 healthy，或处于模型级豁免形态（6004 单模型软冷却——
		// 对触发模型不可用，对其他模型仍可选）。探活无请求模型上下文，取"存在可服务
		// 模型"与 chat 实际可达性等价（issue #31 探活侧补齐）。
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID: uid,
		// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空，
		// 每模型一行（modelCooldowns 内未到期的条目），多模型同时限流全部展示。
		// 到期判据 = 该模型的独立冷却 until 未过；条件满足才输出，随到期自然消失，
		// 普通软冷却（无模型级表）/硬冷却不产生台账（零回归）。
		RateLimitedModels:        p.rateLimitedModelsLocked(e, now),
		Realm:                    e.a.Realm(),
		Nickname:                 e.a.Nickname,
		Credits:                  e.credits,
		CreditsTotal:             e.creditsTotal,
		CreditsExpiring:          e.creditsExpiring,
		CreditsEarliestExpiry:    e.creditsEarliestExpiry,
		CreditsEarliestRemaining: e.creditsEarliestRemaining,
		Cooling:                  now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:                   e.reason,
		Disabled:                 e.disabled,
		SuccessCount:             e.successCount,
		ErrTotal:                 e.errTotal,
		CheckinDone:              e.lastCheckinDay == now.Format("2006-01-02"),
		TokenUsage:               e.tokenUsage,
		LastSuccessTime:          e.lastSuccess,
		LastErrTime:              e.lastErr,
		Until:                    e.until,
		SoftStreak:               e.softStreak,
		ModelCosts:               p.modelCostsStatusLocked(e, now),
		ConsecutiveFails:         e.consecutiveFails,
		DegradeUntil:             e.degradeUntil,
		InFlight:                 int(e.inFlight.Load()),
		BreakerFails:             e.fails,
		BreakerUntil:             e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		// 常规冷却（until）与熔断期（breakerUntil）可能只有其一在生效，
		// 取仍在未来且更晚截止的那个，避免仅熔断期时误报 0 / unknown。
		remaining := int64(0)
		if now.Before(e.until) {
			if r := int64(time.Until(e.until).Seconds() + 0.999); r > remaining {
				remaining = r
			}
		}
		if now.Before(e.breakerUntil) {
			if r := int64(time.Until(e.breakerUntil).Seconds() + 0.999); r > remaining {
				remaining = r
				st.CoolKind = "breaker"
			}
		}
		st.CoolRemaining = remaining
		if st.CoolKind == "" {
			st.CoolKind = e.coolKind.String()
		}
	}
	return st
}

// modelCostsStatusLocked 收集账号的有效成本台账行（P1-anti-monopoly 可观测性）。
// 仅 modelCostTTL 内的观测进台账（过期/零值跳过，与选号读取侧同口径）；
// 模型名稳定排序。调用方必须已持有锁。
func (p *Pool) modelCostsStatusLocked(e *entry, now time.Time) []ModelCostStatus {
	if len(e.modelCost) == 0 {
		return nil
	}
	models := make([]string, 0, len(e.modelCost))
	for m, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			continue // 过期/零值：不进台账（与选号读取侧同口径）
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil
	}
	sort.Strings(models)
	rows := make([]ModelCostStatus, 0, len(models))
	for _, m := range models {
		mc := e.modelCost[m]
		rows = append(rows, ModelCostStatus{
			Model:     m,
			CostPer1k: mc.CostPer1k,
			LastSeen:  mc.LastSeen,
			Samples:   mc.Samples,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// rateLimitedModelsLocked 收集账号当前仍在限额的模型台账行（issue #36）。
// modelCooldowns 未到期条目按模型名稳定排序输出；全部到期/空表返回 nil。
// 调用方必须已持有锁（statusOf 只读路径持 RLock，本函数只读不写）。
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// 先排序模型名，保证输出稳定（map 遍历无序）。
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			kind := "rate_limit"
			if strings.HasPrefix(mc.Reason, "11102") {
				kind = "model_unavailable"
			}
			row := RateLimitedModel{
				Model:  m,
				Kind:   kind,
				Until:  mc.Until,
				Reason: mc.Reason,
			}
			// 上游原始重置墙钟：截断后 until==resetAt 时省略（omitempty），台账只显示真实恢复时刻。
			if !mc.ResetAt.IsZero() && !mc.ResetAt.Equal(mc.Until) {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}
