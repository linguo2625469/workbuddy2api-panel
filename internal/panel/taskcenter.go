// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID        string          `json:"uid"`
	Nickname   string          `json:"nickname"`
	Realm      string          `json:"realm"`
	Skipped    bool            `json:"skipped"`
	SkipReason string          `json:"skip_reason,omitempty"`
	Growth     []upstream.Task `json:"growth,omitempty"`
	GrowthErr  string          `json:"growth_error,omitempty"`
}

func (p *Panel) scanAccountGrowth(a *auth.Auth) scanAccountItem {
	it := scanAccountItem{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm()}
	if a.IsGlobal() {
		it.Skipped = true
		it.SkipReason = "国际区任务仅支持查询，不执行国内区成长任务"
		return it
	}

	var errors []string
	if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
		errors = append(errors, "list tasks: "+err.Error())
	} else {
		for _, t := range tasks {
			if growthPending(t) {
				it.Growth = append(it.Growth, t)
			}
		}
	}
	// 小程序任务与默认任务分别读取。即使其中一路失败，也保留另一路结果并
	// 明确标记扫描不完整，避免“没有待办”被误解成所有任务都已完成。
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err != nil {
		errors = append(errors, "list MP tasks: "+err.Error())
	} else {
		seen := make(map[string]bool, len(it.Growth))
		for _, t := range it.Growth {
			seen[t.TaskCode] = true
		}
		for _, t := range mpTasks {
			if growthPending(t) && !seen[t.TaskCode] {
				it.Growth = append(it.Growth, t)
				seen[t.TaskCode] = true
			}
		}
	}
	if len(errors) > 0 {
		it.GrowthErr = strings.Join(errors, "; ")
	}
	return it
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		// 达标未领：也入队（队列执行后会自动领）——但仅限有自动化动作的任务，
		// 否则队列执行时会因 autoActionFor 为 nil 直接报错。
		return autoActionFor(t.TaskCode) != nil
	}
	return autoActionFor(t.TaskCode) != nil
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		items[i] = scanAccountItem{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
		if st.Disabled {
			items[i].Skipped = true
			items[i].SkipReason = "账号已禁用"
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				items[i].GrowthErr = "account credentials not found"
				return
			}
			items[i] = p.scanAccountGrowth(a)
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	skipped := 0
	errors := 0
	for _, it := range items {
		pending += len(it.Growth)
		if it.Skipped {
			skipped++
		}
		if it.GrowthErr != "" {
			errors++
		}
	}
	log.Printf("panel: 队列扫描完成：可自动化待办 %d 项，跳过 %d 账号，扫描错误 %d 账号", pending, skipped, errors)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": items, "pending_count": pending,
		"skipped_count": skipped, "error_count": errors,
	})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg, scanAccounts, scanErrorCount := p.startGrowthQueue(body.Concurrency, body.Growth)
	result := map[string]any{"scan_accounts": scanAccounts, "scan_error_count": scanErrorCount}
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		result["ok"], result["started"], result["message"] = scanErrorCount == 0, false, msg
		writeJSON(w, http.StatusOK, result)
	default:
		result["ok"], result["started"], result["total"], result["seq"] = true, true, total, seq
		if scanErrorCount > 0 {
			result["message"] = fmt.Sprintf("队列已启动；%d 个账号扫描失败，队列只包含已成功读取的待办", scanErrorCount)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回基础队列结果及扫描明细。seq==-1 表示队列已在
// 执行（冲突）；started=false 时 msg 为无已确认可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string, scanAccounts []scanAccountItem, scanErrorCount int) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）", nil, 0
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()
	scanAccounts = make([]scanAccountItem, len(states))
	scanned := make([]queueAccount, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		scanAccounts[i] = scanAccountItem{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
		if st.Disabled {
			scanAccounts[i].Skipped = true
			scanAccounts[i].SkipReason = "账号已禁用"
			continue
		}
		if !growth {
			scanAccounts[i].Skipped = true
			scanAccounts[i].SkipReason = "成长任务扫描已关闭"
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			scanAccounts[i].GrowthErr = "account credentials not found"
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			it := p.scanAccountGrowth(a)
			scanAccounts[i] = it
			if len(it.Growth) > 0 {
				sort.Slice(it.Growth, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
					return autoActionIndex(it.Growth[i].TaskCode) < autoActionIndex(it.Growth[j].TaskCode)
				})
				scanned[i] = queueAccount{a: a, grow: it.Growth}
			}
		}(i, a)
	}
	wg.Wait()
	var accts []queueAccount
	for _, one := range scanned {
		if one.a != nil {
			accts = append(accts, one)
		}
	}
	for _, it := range scanAccounts {
		if it.GrowthErr != "" {
			scanErrorCount++
		}
	}

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：没有已确认可执行的成长待办（扫描错误 %d 个账号）", scanErrorCount)
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		msg = "没有已确认可自动执行的成长任务"
		if scanErrorCount > 0 {
			msg = fmt.Sprintf("扫描失败：%d 个账号读取任务失败，无法确认待办", scanErrorCount)
		}
		return false, 0, 0, msg, scanAccounts, scanErrorCount
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, "", scanAccounts, scanErrorCount
}

// RunGrowthQueueOnce 每日为启用的国区账号启动后台任务。账号内正在执行时
// 复用现有任务，网络查询与动作均由后台处理，调度器不等待完整扫描。
func (p *Panel) RunGrowthQueueOnce() {
	if p.cfg.AutoTasksEnabled == nil || !p.cfg.AutoTasksEnabled() {
		return
	}
	started := 0
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || st.Realm == "global" {
			continue
		}
		if _, fresh, err := p.StartAccountTaskJob(st.UID, "daily"); err != nil {
			log.Printf("panel: 定时后台任务 uid=%s: %v", st.UID, err)
		} else if fresh {
			started++
		}
	}
	if started > 0 {
		log.Printf("panel: 定时后台任务已启动（%d 个账号）", started)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				outcome := autoTaskOutcome{Status: "error", Message: "未知的任务类型"}
				switch kind {
				case "growth":
					outcome = p.runGrowthQueued(one.a, code)
				}
				p.queueMarkAt(i, outcome.Status, outcome.Message)
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 与单任务和 auto_all 共用完成判据，保留未入账/领奖待重试状态。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) autoTaskOutcome {
	act := autoActionFor(code)
	if act == nil {
		return autoTaskOutcome{Status: "error", Message: fmt.Sprintf("任务 %s 无自动动作", code)}
	}
	return p.executeAutoTask(a, act)
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
