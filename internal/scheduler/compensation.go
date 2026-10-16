// compensation.go 补偿活动的可观测化执行器（issue #189）。
//
// 承接 internal/upstream/compensation.go 补上的状态查询，把签到排程里那次「盲领」
// 改成「先查状态 → 待领才领」。解决三个原本无法区分的情形：
//
//	① 没有补偿        ② 已经领过        ③ 真的领取失败
//
// 旧写法 `if credit, err := ...; err == nil { log }` 让三者同构，补偿带截止日
// （实测 2026-10-16 那次是 10/22），签到停摆就会静默过期。本文件让「可领未领」
// 在日志和面板上都能显式看见。
package scheduler

import (
	"errors"
	"log"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 补偿任务的跳过原因（面板据此渲染，勿随意改字面量）。
//
// 注意没有「paused」：暂停选号**不**跳过补偿——补偿是纯领奖 RPC，与「让位防风控」
// 的选号语义无关，旅行/成长任务亦然。这里显式不设该枚举，免得后人手滑补一个
// 把暂停号错误地排除掉。
const (
	compSkipDisabled   = "disabled"    // 账号已禁用，等人工 revive
	compSkipNoAuth     = "no_auth"     // 无凭证
	compSkipGlobal     = "global"      // D4 门控：global 无 CN 任务体系
	compSkipEnterprise = "enterprise"  // 企业版门控：claim-compensation 返回 10001
	compSkipInactive   = "no_active"   // 当前无生效中的补偿活动
	compSkipClaimed    = "claimed"     // 本次活动已领过
	compSkipZeroCredit = "zero_credit" // 活动在，但额度为 0
)

// CompensationRow 单账号的补偿状态行（面板 GET /panel/api/compensation）。
type CompensationRow struct {
	UID      string                      `json:"uid"`
	Nickname string                      `json:"nickname,omitempty"`
	Status   upstream.CompensationStatus `json:"status"`
	Skip     string                      `json:"skip,omitempty"`     // 非空 = 本次未发起上游查询/领取
	SkipMsg  string                      `json:"skip_msg,omitempty"` // 跳过的中文说明
	Error    string                      `json:"error,omitempty"`
}

// CompensationResult 单账号的领取结果（面板 POST /panel/api/compensation_all）。
type CompensationResult struct {
	UID      string                      `json:"uid"`
	Nickname string                      `json:"nickname,omitempty"`
	Status   upstream.CompensationStatus `json:"status"`
	Claimed  bool                        `json:"claimed"`
	Credit   int64                       `json:"credit,omitempty"`
	Skip     string                      `json:"skip,omitempty"`
	SkipMsg  string                      `json:"skip_msg,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

// compTarget 一次遍历里某个账号的处理目标。
//
// 参与账号与被门控跳过的账号合成**同一个有序切片**（而不是 jobs + skips 两个
// 容器）——后者会让跳过行只能从 map 取出，既丢掉昵称，又让面板行序随机漂移
// （池顺序是按 UID 稳定排序的，面板逐行对齐依赖它）。
type compTarget struct {
	uid  string
	nick string
	a    *auth.Auth // nil = 被门控跳过
	skip string
	msg  string
}

// compTargets 按池顺序枚举全部账号并完成门控判定。
//
// 门控与 streakBonusAccount 同口径：D4 global 跳过；企业版跳过（上游对
// claim-compensation 一律 400「企业账号不支持该操作」，与 claim-gift 同批实测）；
// disabled 跳过（凭证可能已失效，发起无意义）。paused **不跳过**（见上方枚举说明）。
func (s *Scheduler) compTargets() []compTarget {
	list := s.cfg.Pool.List()
	out := make([]compTarget, 0, len(list))
	for _, st := range list {
		t := compTarget{uid: st.UID, nick: st.Nickname}
		if st.Disabled {
			t.skip, t.msg = compSkipDisabled, "账号已禁用"
			out = append(out, t)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			t.skip, t.msg = compSkipNoAuth, "无凭证"
			out = append(out, t)
			continue
		}
		if a.IsGlobal() {
			t.skip, t.msg = compSkipGlobal, "国际版无 CN 补偿体系"
			out = append(out, t)
			continue
		}
		if a.IsEnterprise() {
			t.skip, t.msg = compSkipEnterprise, "企业版无补偿体系"
			out = append(out, t)
			continue
		}
		t.a = a
		out = append(out, t)
	}
	return out
}

// CompensationStatusAll 并发查全部账号的补偿状态（只读，不领取）。
//
// 输出与池顺序一致：先按 compTargets 预分配等长切片，每个 goroutine 只写自己那
// 一个下标——不同下标是互不重叠的内存，无需互斥锁即可避免竞态（也避免了
// 「goroutine 完成顺序 = 返回顺序」导致的行序漂移）。并发上限 3 与既有
// /panel/api/packages 的账号遍历一致。
func (s *Scheduler) CompensationStatusAll() []CompensationRow {
	targets := s.compTargets()
	rows := make([]CompensationRow, len(targets))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, t := range targets {
		if t.a == nil {
			rows[i] = CompensationRow{UID: t.uid, Nickname: t.nick, Skip: t.skip, SkipMsg: t.msg}
			continue
		}
		wg.Add(1)
		go func(i int, t compTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := CompensationRow{UID: t.uid, Nickname: t.nick}
			st, err := s.cfg.Upstream.GetCompensationStatus(t.a)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.Status = st
			}
			rows[i] = row
		}(i, t)
	}
	wg.Wait()
	return rows
}

// ClaimCompensationNow 逐号「查状态 → 待领则领」，返回逐号结果。
//
// 供面板手动按钮与签到排程共用；单号失败不影响其余账号。
//
// 这里刻意**串行**而非并发：ClaimCompensation 对同一活动每号仅一次（重复领会拿到
// 业务错误），串行让「前一个号失败」不影响后一个号的判定，且账号量是个位数。
func (s *Scheduler) ClaimCompensationNow() []CompensationResult {
	targets := s.compTargets()
	out := make([]CompensationResult, 0, len(targets))
	for _, t := range targets {
		if t.a == nil {
			out = append(out, CompensationResult{UID: t.uid, Nickname: t.nick, Skip: t.skip, SkipMsg: t.msg})
			continue
		}
		out = append(out, s.claimOne(t.a))
	}
	return out
}

// ClaimCompensationFor 单号领取补偿（面板单号按钮）；账号不在池中返回 error。
//
// 门控与 compTargets 同一套判据，但直接查 Pool.Status/AuthByUID（均为 O(1) 的
// 加锁查询，无副作用），不扫全表——pool 本来就提供单号查询，没必要造 O(n)。
func (s *Scheduler) ClaimCompensationFor(uid string) (CompensationResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CompensationResult{UID: uid}, errors.New("account not found")
	}
	skip := func(reason, msg string) (CompensationResult, error) {
		return CompensationResult{UID: uid, Nickname: st.Nickname, Skip: reason, SkipMsg: msg}, nil
	}
	if st.Disabled {
		return skip(compSkipDisabled, "账号已禁用")
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.AccessTokenValue() == "" {
		return skip(compSkipNoAuth, "无凭证")
	}
	if a.IsGlobal() {
		return skip(compSkipGlobal, "国际版无 CN 补偿体系")
	}
	if a.IsEnterprise() {
		return skip(compSkipEnterprise, "企业版无补偿体系")
	}
	return s.claimOne(a), nil
}

// claimOne 单账号「查状态 → 待领才领」的全部分支与日志。
func (s *Scheduler) claimOne(a *auth.Auth) CompensationResult {
	r := CompensationResult{UID: a.UID, Nickname: a.Nickname}

	st, err := s.cfg.Upstream.GetCompensationStatus(a)
	if err != nil {
		// 查状态失败是真问题（传输/鉴权/上游），必须显式记日志——
		// 旧盲领写法会把这类失败静默吞成「无事发生」。
		r.Error = err.Error()
		log.Printf("compensation %s: 查状态失败: %v", logfmt.Label(a.UID, a.Nickname), err)
		return r
	}
	r.Status = st

	// 判据用 upstream 的 Pending()（唯一权威实现），不要再在本文件复刻一份
	// active/claimed/credit 的与门——此前三处拷贝（这里 / Pending() / 前端
	// compPending）已经开始漂移。下方 switch 只为「不领」的三种形态给出不同文案，
	// 不参与判定。
	if !st.Pending() {
		switch {
		case !st.Active:
			r.Skip, r.SkipMsg = compSkipInactive, "当前无生效补偿"
		case st.Claimed:
			r.Skip, r.SkipMsg = compSkipClaimed, "本次活动已领取"
		default:
			r.Skip, r.SkipMsg = compSkipZeroCredit, "活动在但额度为 0"
		}
		return r
	}

	credit, err := s.cfg.Upstream.ClaimCompensation(a)
	if err != nil {
		r.Error = err.Error()
		log.Printf("compensation %s: 待领 %dc 但领取失败: %v",
			logfmt.Label(a.UID, a.Nickname), st.CreditNum, err)
		return r
	}
	r.Claimed, r.Credit = true, credit
	log.Printf("compensation %s: 🎊 补偿领取 +%dc（截止 %s）",
		logfmt.Label(a.UID, a.Nickname), credit, st.EndTime)
	return r
}

// CompensationSummary 批量领取的汇总计数。
//
// 单独统计 Errored 是为了让调用方能区分「没有可领」与「一个都没查成」：上游故障
// 或 token 全过期时，每个账号都会走 claimOne 的 error 分支。若只按 Claimed 计数，
// 面板会把「全部查询失败」渲染成「当前没有待领补偿」——正是本 PR 要消灭的那类
// 静默失败，只是换了个位置。调用方必须先看 Errored 再看 Claimed。
type CompensationSummary struct {
	Total   int   `json:"total"`
	Claimed int   `json:"claimed"`
	Skipped int   `json:"skipped"`
	Errored int   `json:"errored"`
	Credit  int64 `json:"credit"`
}

// OK 是否全部账号都拿到了确定结论（无错误）。
func (s CompensationSummary) OK() bool { return s.Errored == 0 }

// SummarizeCompensation 汇总逐号结果。
func SummarizeCompensation(results []CompensationResult) CompensationSummary {
	var s CompensationSummary
	s.Total = len(results)
	for _, r := range results {
		switch {
		case r.Error != "":
			s.Errored++
		case r.Claimed:
			s.Claimed++
			s.Credit += r.Credit
		default:
			s.Skipped++
		}
	}
	return s
}

// claimCompensationQuiet 签到排程用：待领才领，无待领不记日志（避免每日刷屏），
// 但**真实失败一律记日志**——这正是旧盲领实现丢掉的信息。
//
// 与面板手动按钮共用 claimOne，避免两条路径的分支/日志漂移。
func (s *Scheduler) claimCompensationQuiet(a *auth.Auth) {
	_ = s.claimOne(a)
}
