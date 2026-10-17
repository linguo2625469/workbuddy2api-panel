// tasksweep.go 周期幂等「补领清扫」。
//
// 职责单一——只 accept 尚未接受的任务、只 claim 已达标未领的任务，**不跑任何
// 行为链路**（不真实对话、不上报事件）。来源见 issue #174：
//
//   - growthPending 要求任务必须有自动动作（25 个 autoActions 白名单），不在
//     白名单的任务即使达标也永远扫不进队列、永远不领奖；
//   - 队列路径要先重跑一遍行为动作（可能失败/前置条件已变），且一天只跑一次；
//   - 「一键完成」对已达标任务直接跳过不领。
//
// 三条链路都不负责「把已达标的奖励领掉」，于是积分卡在 2000/2100/2300 上不去 4000。
// 本文件补的就是这条缺失链路：分钟级周期、幂等、可开关。
//
// 设计要点（与 issue 一致）：
//   - 判据只用 progress 口径 Task.Claimable（!claimed && target>0 && current>=target），
//     **不用 accept_status == "completed"**：它在回读时会短暂转 completed 后整体回滚，
//     据此领奖会误领。
//   - claim 幂等（重复领返回 already_claimed → 0 分 0 能），宁可多试一轮也不漏领。
//   - 与单任务 / 一键完成 / 成长队列共用 per-account 锁：正在跑动作的账号本轮跳过，
//     不打断正在进行的动作链路。
//   - global（无 CN 成长体系）与企业版（上游 403 growth system is only available
//     for personal users）账号都不发起任何上游请求。
package panel

import (
	"log"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TaskSweepStats 一轮补领清扫的汇总（供日志与潜在的未来 HTTP 回执）。
type TaskSweepStats struct {
	Accounts int   `json:"accounts"` // 参与清扫的账号数
	Skipped  int   `json:"skipped"`  // 因正在执行任务动作而跳过的账号数
	Accepted int   `json:"accepted"` // 本轮接受成功的任务数
	Claimed  int   `json:"claimed"`  // 本轮领奖成功的任务数
	Credits  int64 `json:"credits"`  // 本轮到账积分
	Energy   int64 `json:"energy"`   // 本轮到账能量
	Errors   int   `json:"errors"`   // 本轮出错（不中断）的次数
}

// RunTaskSweepOnce 对所有账号做一轮幂等补领清扫（账号间并发、账号内串行）。
// 由 scheduler 的 ticker 调用（详见 scheduler.StartTaskSweep）。
func (p *Panel) RunTaskSweepOnce() TaskSweepStats {
	var st TaskSweepStats
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue // 禁用账号不参与（与任务中心扫描同口径）
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.IsGlobal() || a.IsEnterprise() {
			continue // 无个人成长体系：零上游请求
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			r := p.sweepAccount(a, uid)
			mu.Lock()
			defer mu.Unlock()
			st.Accounts += r.accounts
			st.Skipped += r.skipped
			st.Accepted += r.accepted
			st.Claimed += r.claimed
			st.Credits += r.credit
			st.Energy += r.energy
			st.Errors += r.errors
		}(a, ps.UID)
	}
	wg.Wait()

	if st.Claims() > 0 || st.Errors > 0 || st.Accepted > 0 {
		log.Printf("panel: 任务补领清扫：%d 个账号（跳过 %d）· 接受 %d · 领奖 %d · +%d 分 +%d 能 · 出错 %d",
			st.Accounts, st.Skipped, st.Accepted, st.Claimed, st.Credits, st.Energy, st.Errors)
	}
	return st
}

// Claims 本轮实际到账奖励的任务数（与 Claimed 一致，语义化别名）。
func (s TaskSweepStats) Claims() int { return s.Claimed }

// sweepResult 单账号清扫结果（RunTaskSweepOnce 汇总用，非导出）。
type sweepResult struct {
	accounts, skipped, accepted, claimed, errors int
	credit, energy                               int64
}

// sweepAccount 单账号一轮清扫。命中账号锁（正在跑任务动作）时整账号跳过：
// 不打断进行中的链路，下一轮自然补上。
func (p *Panel) sweepAccount(a *auth.Auth, uid string) (res sweepResult) {
	res.accounts = 1
	if !p.tryLockAccount(uid) {
		res.skipped = 1
		return res
	}
	defer p.unlockAccount(uid)

	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		res.errors++
		return res
	}
	// 小程序口径列表（school_season / Sequential_Tasks_1 等仅在 mp 头下发）：
	// 与默认口径按 task_code 去重合并；mp 列表失败静默（活动结束 / 无 mp 任务）。
	mpOnly := map[string]bool{}
	if mpTasks, mErr := p.cfg.Upstream.ListTasksMP(a); mErr == nil {
		seen := map[string]bool{}
		for _, t := range tasks {
			seen[t.TaskCode] = true
		}
		for _, t := range mpTasks {
			if seen[t.TaskCode] {
				continue
			}
			mpOnly[t.TaskCode] = true
			tasks = append(tasks, t)
		}
	}

	// 阶段一：接受未接受的任务。默认口径与 mp 口径必须**分开提交**（端标记不同，
	// 混提交会让 mp 任务报 task not found）。
	var codes, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if isMPTaskCode(t.TaskCode) || mpOnly[t.TaskCode] {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			codes = append(codes, t.TaskCode)
		}
	}
	acceptCodes := func(cs []string, isMP bool) {
		if len(cs) == 0 {
			return
		}
		var aErr error
		if isMP {
			aErr = p.cfg.Upstream.AcceptTasksMP(a, cs)
		} else {
			aErr = p.cfg.Upstream.AcceptTasks(a, cs)
		}
		if aErr != nil {
			res.errors++
			return
		}
		res.accepted += len(cs)
	}
	acceptCodes(codes, false)
	acceptCodes(mpCodes, true)

	// 阶段二：领取已达标的奖励。claim 幂等（重复领 already_claimed → 0 分 0 能），
	// 单项出错只记录、不中断后续任务。
	for _, t := range tasks {
		if !t.Claimable {
			continue
		}
		// mp 口径任务必须走 mp 变体领奖端点（缺 mp 头上游返回 task not found）；
		// 其余走 Web 域端点——与 taskcenter / autotask 的领奖口径一致。
		var c, e int64
		var cErr error
		if isMPTaskCode(t.TaskCode) || mpOnly[t.TaskCode] {
			c, e, cErr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			c, e, cErr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cErr != nil {
			res.errors++
			continue
		}
		res.credit += c
		res.energy += e
		res.claimed++
	}
	return res
}
