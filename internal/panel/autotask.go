// autotask.go 面板「一键完成」任务的动作实现。
//
// 设计依据：上游 scripts/task_*.py 实测结论 + 2026-09-12 桌面指纹协议逆向
// （data/desktop-task-protocol.md）——
//   - first_buddy（+300 分）：report（前置解锁）→ agreement → buddy/first
//   - chat_5（+100 分）：累计 5 条 chat_request_send 上报
//   - Model_chat_GLM5.2（+100 分）：accept → 用 glm-5.2 真实对话一次 → 上报（模型字段对齐）
//   - RichMeow_Chat：桌面指纹（workbuddy-desktop）完整对话事件链，纯 API 可点亮（三账号实测）
//   - Buddy_App / Buddy_App_QQ：buddyapp 五连事件，纯 API 可点亮（两账号实测）
//   - automation_1：automated_task_create_suc 事件，纯 API 可点亮（两账号实测）
//   - Library_read：web 域 web_element_click(library_doc_intro_click)（三账号实测）
//   - template_5 / playbook_prompt / create_canvas：asar 逆向出的判据事件
//     （template_used / playbook_prompt_send / wbx_design_canvas_*），纯 API 可点亮（三账号实测）
//   - expert_5 / Expert_team_use_3：真实专家列表 + 召唤链 + 真实 chat（服务端 requestId）
//   - expert_actual_use（三账号实测）
//   - Hp_Appearance：appearance/set + appearance_skin_apply 事件（两账号实测）
//
// 仍未破解：skill_1（疑似要求真实 Skill 工具调用）。
// 不做：Expert_lighthouse（需真实连接器授权）、Expert_Philanthropy（真实捐款）。
//
// 已领取任务不重复执行，已达标未领奖的任务只领奖，不重复消耗对话配额。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	run      func(p *Panel, a *auth.Auth) (string, error)
}

const globalTaskWriteMessage = "国际区任务暂仅支持查询，请在官方客户端操作"

// autoTaskOutcome 是统一任务执行器的结果。Status 是任务状态机的唯一完成判据：
// done 只表示服务端已确认领取或领奖请求已成功；未入账、领奖失败、读错均保留各自状态。
type autoTaskOutcome struct {
	Status         string
	Message        string
	ProgressBefore string
	ProgressAfter  string
	Claimable      bool
	Claimed        bool
	Credit         int64
	Energy         int64
	ClaimError     string
	Attempt        bool
	ActionRun      bool
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
// first_buddy 依赖活跃上报解锁，故 chat_5/first_buddy 的执行都自带 report 步骤。
var autoActions = []autoAction{
	{
		TaskCode: "chat_5",
		Desc:     "上报 5 条对话活跃事件（自动补足差额）",
		run:      runChat5,
	},
	{
		TaskCode: "first_buddy",
		Desc:     "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）",
		run:      runFirstBuddy,
	},
	{
		TaskCode: "Model_chat_GLM5.2",
		Desc:     "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报",
		run:      runModelChat,
	},
	{
		TaskCode: "RichMeow_Chat",
		Desc:     "桌面指纹事件链上报（已验证：纯 API 可点亮，三账号实测）",
		run:      runRichMeow,
	},
	{
		TaskCode: "Buddy_App",
		Desc:     "上报「进入 Buddy 应用」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "Buddy_App_QQ",
		Desc:     "上报「进入企鹅教师助手」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "automation_1",
		Desc:     "上报「定时任务创建」事件（已验证：纯 API 可点亮）",
		run:      runAutomationCreate,
	},
	{
		TaskCode: "Library_read",
		Desc:     "上报「读资料库介绍」事件（已验证：纯 API 可点亮）",
		run:      runLibraryRead,
	},
	{
		TaskCode: "template_5",
		Desc:     "上报「使用模板创建任务」事件组 ×5（已验证：三账号点亮）",
		run:      runTemplateUse,
	},
	{
		TaskCode: "playbook_prompt",
		Desc:     "上报「灵感案例做同款发送 Prompt」事件组（已验证：三账号点亮）",
		run:      runPlaybookPrompt,
	},
	{
		TaskCode: "create_canvas",
		Desc:     "上报「设计创意画布创建」事件组（已验证：三账号点亮，+300 分）",
		run:      runCreateCanvas,
	},
	{
		TaskCode: "expert_5",
		Desc:     "真实专家召唤+使用链 ×5（专家市场列表+真实 chat，已验证：三账号点亮）",
		run:      runExpertUse,
	},
	{
		TaskCode: "Expert_team_use_3",
		Desc:     "真实专家团召唤+使用链 ×3（已验证：三账号点亮）",
		run:      runExpertTeamUse,
	},
	{
		TaskCode: "Hp_Appearance",
		Desc:     "设置主题 API + 皮肤生效事件（已验证：两账号点亮）",
		run:      runAppearance,
	},
	{
		TaskCode: "skill_1",
		Desc:     "真实对话 + skill_info 技能加载事件（已验证：人杰2 点亮）",
		run:      runSkillFresh,
	},
	{
		TaskCode: "Expert_lighthouse",
		Desc:     "真实轻量云专家召唤+使用链（chat 链带 has_expert，已验证：两账号点亮）",
		run:      runExpertLighthouse,
	},
	{
		TaskCode: "black_cat",
		Desc:     "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示稍后再试）",
		Attempt:  true,
		run:      runBlackCat,
	},
	{
		TaskCode: "school_season",
		Desc:     "校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）",
		run:      runSchoolSeason,
	},
	{
		TaskCode: "Sequential_Tasks_1",
		Desc:     "小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）",
		run:      runSequentialChat,
	},
	{
		TaskCode: "Sequential_Tasks_2",
		Desc:     "小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）",
		run:      runMiniExpert,
	},
	{
		TaskCode: "Sequential_Tasks_3",
		Desc:     "小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）",
		run:      runSequentialChat5,
	},
	{
		TaskCode: "Sequential_Tasks_4",
		Desc:     "小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件（PC 同源）→ 领奖（判据待解锁验证）",
		run:      runSequentialAutomation,
	},
	{
		TaskCode: "Sequential_Tasks_5",
		Desc:     "小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）",
		run:      runSequentialModelChat,
	},
	{
		TaskCode: "Sequential_Tasks_6",
		Desc:     "小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖",
		run:      runSequentialChat10,
	},
	{
		TaskCode: "Sequential_Tasks_7",
		Desc:     "体验灵感功能（预留，疑 PC 口径 +500c+5e）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）",
		run:      runSequentialPlaybook,
	},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}
	return nil
}

// autoActionIndex 任务在 autoActions 中的顺序（队列执行按依赖序排；未知返回大值）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}
	return 1 << 20
}

// mpTaskCodes 小程序口径专属下发的成长任务：默认（无 mp 头）列表不出现，
// accept/claim 均要求 X-Client-Platform: miniprogram。新任务出现时在此登记。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日（mini chat + activityId）
	"Sequential_Tasks_1": true, // 小程序首对话（mini chat，无 activityId）
	"Sequential_Tasks_2": true, // 小程序选中专家并完成有效对话（mp 指纹 expert_actual_use）
	"Sequential_Tasks_3": true, // 小程序完成 5 次对话（与 Tasks_1 同形状，target=5 逐条累加）
	// Tasks_4..7 存在性已实测（accept 返回 prerequisite not met 链式依赖；Tasks_8 not
	// found 封顶）。链条每日零点解锁一环（task locked until 次日），判据为 issue #42
	// 描述 + mpsrc 事件形状预置，解锁后逐个实测校正。
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTaskCode 报告任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
// 双口径：mp 专属任务在默认列表查不到，自动回落 mp 列表（仅对已登记的 mp 码，
// 未知码不多打一次上游）。
func (p *Panel) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测 Model_chat
// 对话完成后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (p *Panel) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil {
			return t, err2 // 回读失败不能把旧进度当作本轮已确认结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskByCodeMP 以小程序口径拉取任务列表并定位单个任务；未找到返回 nil。
// 小程序限定任务（school_season / Sequential_Tasks_1）在默认口径列表不出现。
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptWithVerifyMP accept 并回读验证登记生效：上游存在 200+OK 但 accept 未真正
// 登记的形态（此时上报事件全部不归账，任务永远点不亮，上游 task_runner c793ae3
// 实测）——判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerifyMP(a *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(a, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v", logfmt.Label(a.UID, a.Nickname), code, attempt, err)
			continue
		}
		time.Sleep(mpActionGap)
		t, err := p.taskByCodeMP(a, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept 尝试%d 未登记生效（回读=%q）", logfmt.Label(a.UID, a.Nickname), code, attempt, acceptStatusOr(t))
	}
	return false
}

// acceptStatusOr 安全读取任务 accept_status（nil 任务返回 "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// mpActionGap mp 任务写动作间隔（accept/上报/领奖之间，防频控）。
var mpActionGap = 2 * time.Second

// mpChatEventGap mp 对话事件（chat_request_send）的真人节奏间隔。上游对
// Sequential_Tasks_3「5 次有效对话」有反作弊校验：数秒级连发的事件会先被计入
// 进度（回读 5/5、accept_status 甚至短暂转 completed），随后被判定无效整体回滚
// （进度回落、claim 返回 400 "task not completed"）——2026-09-26 实测 2s 连发
// 4 条全灭，45s 间隔逐条上报全存活且 claim +300c+5e 成功。每条上报前
// sleep gap + 0~10s 抖动；首条也等（上一轮残留进度被回滚后立即重报同样无效）。
var mpChatEventGap = 45 * time.Second

// runMPMiniChatTask growth 域小程序限定任务通用闭环：
// mp 查询 → accept（带登记回读验证）→ mini chat 事件上报（withActivityId 决定
// 是否带开学季 activityId：school_season 必带，Sequential_Tasks_1 不带——服务端按
// source=mini_program 指纹关联）→ 回读 → 达标即领奖。
func (p *Panel) runMPMiniChatTask(a *auth.Auth, code string, withActivityId bool) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "", fmt.Errorf("accept 未登记生效，上游未确认任务已接受")
		}
		// accept 前的任务进度为 null（target 下发 0），兜底 target=1 会少报——
		// Tasks_6 首轮实测：accept 后真实 target=10，只补 1 条就误判达标去领奖
		// （claim 400 task not completed）。接受后回读一次拿真实 target/current。
		if t2, err := p.taskByCodeMP(a, code); err == nil && t2 != nil {
			t = t2
		}
	}
	// 已达标（含 completed 未领）：直接领奖。
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据上报：按差额补 mini chat 事件。每条前 sleep mpChatEventGap+抖动——
	// 连发会被上游反作弊判无效（见 mpChatEventGap 注释），宁可慢不可白报。
	need := target - t.Current
	for i := int64(0); i < need; i++ {
		time.Sleep(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second))))
		conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), i)
		var ev map[string]any
		if withActivityId {
			ev = upstream.SchoolSeasonChatEvent(conv)
		} else {
			ev = upstream.SchoolChatTimesEvents(conv)
		}
		if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
			return "", fmt.Errorf("完成 %d/%d 次上报后中断: %w", i, need, err)
		}
	}
	// 回读（异步计分，有界轮询复用 claimPoll 预算的紧凑版：两轮各隔 3s）。
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target), nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSchoolSeason 完成 school_season「校园日」（growth 域小程序限定）。
// 判据 = mini chat_request_send + activityId=school_open_day_2026（无 activityId
// 不点亮，与 school 域开学季同活动关联；上游 task_runner e2e 实测 +100c+5e）。
func runSchoolSeason(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "school_season", true)
}

// runSequentialChat 完成 Sequential_Tasks_1「小程序内完成 1 次有效对话」。
// 判据 = mini chat_request_send（无 activityId，服务端按 source=mini_program
// 指纹关联；上游 task_runner 实测 +100c+5e）。
func runSequentialChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
}

// runSequentialChat5 完成 Sequential_Tasks_3「在小程序内完成 5 次有效对话」。
// 判据与 Sequential_Tasks_1 同形状（mini 指纹 chat_request_send，无 activityId），
// 仅 target=5——服务端按上报条数累加进度，但**要求真人节奏**：连发事件先计数
// 后被反作弊回滚（claim 400 "task not completed"），由 mpChatEventGap 间隔保证
// （2026-09-26 实测：45s 间隔补满 5/5 → claim +300c+5e 成功，领后 accept_status
// =claimed 稳定不回滚）。
func runSequentialChat5(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
}

// runSequentialChat10 完成 Sequential_Tasks_6「在小程序内完成 10 次有效对话」（预留）。
// 判据假定与 Tasks_1/3 同形状（mini chat_request_send），target 由任务自带（回读），
// runMPMiniChatTask 按差额补报——issue #42 称 target=10，以解锁后实际下发为准。
// 真人节奏间隔同样适用（mpChatEventGap）：9 条 × ~50s ≈ 8 分钟/账号，夜间队列可接受。
func runSequentialChat10(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_6", false)
}

// runSequentialEventTask Sequential 链预留任务通用骨架：mp 查询 → accept（带验证）
// → 判据事件上报（primary；未点亮且 fallback 非空时补一轮）→ 回读 → 达标领奖。
// 每日零点解锁一环：locked 期间 accept 不落账，返回等下次调度（无需人工干预）。
func (p *Panel) runSequentialEventTask(a *auth.Auth, code string, primary, fallback func() error) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "", fmt.Errorf("accept 未登记生效，任务可能处于每日锁定窗口")
		}
	}
	if err := primary(); err != nil {
		return "", fmt.Errorf("判据上报失败: %w", err)
	}
	// 回读（两轮各隔 3s）；未点亮且有 fallback 时补报一轮再读。
	for round := 0; round < 2; round++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return "", fmt.Errorf("备选判据上报失败: %w", err)
			}
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未点亮（判据形态待解锁后校正，下次重试）", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSequentialAutomation 完成 Sequential_Tasks_4「创建定时任务」（预留）。
// mp 源码无 automation 事件发射点 → 判据疑为 PC 口径：复用 automation_1 同源
// 事件（DesktopAutomationCreateEvent，PC 任务三账号实测点亮）。
func runSequentialAutomation(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_4",
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("wb2api 自动化"))
		}, nil)
}

// runSequentialModelChat 完成 Sequential_Tasks_5「使用 GLM5.2」（预留）。
// primary：mp 对话事件带 requestModelId/requestModelName=glm-5.2（mpsrc main 32904
// 发射点实测形状）；fallback：PC 域模型活跃上报（Model_chat_GLM5.2 同源）。
func runSequentialModelChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_5",
		func() error {
			conv := fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli())
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli()), "", "glm-5.2", "GLM-5.2")
		})
}

// runSequentialPlaybook 完成 Sequential_Tasks_7「体验灵感功能」（预留，疑 PC 口径）。
// primary：PC 灵感事件组（DesktopPlaybookPromptSequence，playbook_prompt 三账号
// 实测点亮）；fallback：mp 指纹灵感事件组（MiniPlaybookEvents，mpsrc 形状）。
func runSequentialPlaybook(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	return p.runSequentialEventTask(a, "Sequential_Tasks_7",
		func() error {
			conv := fmt.Sprintf("wb2api-pb-%d", ms)
			req := fmt.Sprintf("wb2api-pb-req-%d", ms)
			return p.cfg.Upstream.ReportDesktopEvent(a,
				upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
		},
		func() error {
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
		})
}

// runMiniExpert 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
// 判据 = mp 指纹 expert_actual_use（**不带** activityId/conversationId、
// extVersion=2.2.8、type=send_message——小程序源码实测形状，与 school 域 expert
// 事件两套口径勿混；上游 task_runner 实测上报即 completed，claim +200c+5e）。
// 专家 id 必须是市场真实 ex_ id（空 id 服务端不入账）→ **accept 之前**先解析市场
// 列表：拉不到就整任务不动作，避免留下「已登记未上报」的半程态（上游 9a26ae7
// 的 ids 前置判定同款）。复用既有 MarketExpertList（expert_5 任务同源，实测可用）。
func runMiniExpert(p *Panel, a *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_2"
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1 // 未 accept 的 mp 任务 progress 为 null，target 兜底（上游实测）
	}
	// 已达标（含 completed 未领）：直接领奖。
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据载体前置（accept 之前）：市场真实专家 id。
	experts, merr := p.cfg.Upstream.MarketExpertList(a, "")
	if merr != nil {
		return "", fmt.Errorf("专家市场不可用，跳过以防半程态: %w", merr)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空，跳过以防半程态")
	}
	e := experts[0]
	name := e.DisplayNameZH
	if name == "" {
		name = e.ProfessionZH
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "", fmt.Errorf("accept 未登记生效，上游未确认任务已接受")
		}
	}
	ev := upstream.MiniExpertUseEvent(e.ExpertID, name, e.ExpertType)
	if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
		return "", fmt.Errorf("上报 expert_actual_use 失败: %w", err)
	}
	// 回读（异步计分，两轮各隔 3s——与 runMPMiniChatTask 同预算）。
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未归账（异步计分，下次重试）", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 汇报结果。
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
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
	act := autoActionFor(body.TaskCode)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作")
		return
	}
	// per-account 互斥：同账号的任务动作正在跑（单任务或全量）时直接 409，
	// 不并发重跑（动作幂等但 expert/skill 系含真实对话，重跑浪费配额）。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	outcome := p.executeAutoTask(a, act)
	log.Printf("panel: 任务动作 uid=%s code=%s status=%s progress %s -> %s claimed=%v",
		uid, act.TaskCode, outcome.Status, outcome.ProgressBefore, outcome.ProgressAfter, outcome.Claimed)
	writeJSON(w, http.StatusOK, outcome.resultMap(act))
}

// taskProgressText 任务进度的可读表示（回读对比用）。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

func (o autoTaskOutcome) resultMap(act *autoAction) map[string]any {
	result := map[string]any{
		"ok":               o.Status != "error",
		"status":           o.Status,
		"message":          o.Message,
		"progress_before":  o.ProgressBefore,
		"progress_after":   o.ProgressAfter,
		"claimable":        o.Claimable,
		"claimed":          o.Claimed,
		"attempt":          o.Attempt,
		"verify_supported": true,
	}
	if act != nil {
		result["task_code"] = act.TaskCode
		result["desc"] = act.Desc
	}
	if o.Credit != 0 || o.Energy != 0 {
		result["credit"] = o.Credit
		result["energy"] = o.Energy
	}
	if o.ClaimError != "" {
		result["claim_error"] = o.ClaimError
	}
	return result
}

func taskProgressReached(t *upstream.Task) bool {
	return t != nil && (t.Target > 0 && t.Current >= t.Target ||
		t.AcceptStatus == "completed" || strings.EqualFold(t.Status, "complete"))
}

// executeAutoTask 统一执行任务：只在服务端确认进度达标后领奖，只有服务端确认已领
// 或领奖接口成功才返回 done。已达标未领取时先领奖，避免重复执行真实对话动作。
func (p *Panel) executeAutoTask(a *auth.Auth, act *autoAction) (out autoTaskOutcome) {
	taskCode := "?"
	uid := "?"
	if act != nil {
		taskCode = act.TaskCode
	}
	if a != nil {
		uid = a.UID
	}
	defer func() {
		log.Printf("panel: task auto uid=%s code=%s status=%s progress=%s->%s claimed=%v claim_error=%q",
			uid, taskCode, out.Status, truncateStr(out.ProgressBefore, 64),
			truncateStr(out.ProgressAfter, 64), out.Claimed, truncateStr(out.ClaimError, 180))
	}()
	if a == nil {
		return autoTaskOutcome{Status: "error", Message: "账号凭证不可用"}
	}
	if a.IsGlobal() {
		return autoTaskOutcome{Status: "error", Message: globalTaskWriteMessage}
	}
	if act == nil {
		return autoTaskOutcome{Status: "skipped", Message: "未配置该任务的自动动作"}
	}
	out = autoTaskOutcome{Attempt: act.Attempt}
	before, err := p.taskByCode(a, act.TaskCode)
	if err != nil {
		out.Status = "error"
		out.Message = "查询任务失败: " + err.Error()
		return out
	}
	if before == nil {
		out.Status = "skipped"
		out.Message = "该账号没有此任务"
		return out
	}
	out.ProgressBefore = taskProgressText(before)
	if before.Locked {
		out.Status = "skipped"
		out.Message = "任务尚未解锁"
		return out
	}
	if before.Claimed {
		out.Status = "done"
		out.Claimed = true
		out.Message = "该任务已领取过奖励"
		out.ProgressAfter = taskProgressText(before)
		return out
	}
	if taskProgressReached(before) {
		return p.claimAutoTask(a, act, "任务已达标", before, before)
	}
	if act.run == nil {
		out.Status = "skipped"
		out.Message = "该任务没有可执行的自动动作"
		return out
	}
	message, err := act.run(p, a)
	out.ActionRun = true
	if err != nil {
		out.Status = "error"
		out.Message = "执行失败: " + err.Error()
		return out
	}
	after, err := p.taskByCodeWaiting(a, act.TaskCode)
	if err != nil {
		out.Status = "error"
		out.Message = message + "；回读进度失败: " + err.Error()
		if after != nil {
			out.ProgressAfter = taskProgressText(after)
			out.Claimable = taskProgressReached(after)
		}
		return out
	}
	if after == nil {
		out.Status = "awaiting_progress"
		out.Message = message + "；回读时未找到该任务，暂不能确认完成"
		return out
	}
	out.ProgressAfter = taskProgressText(after)
	out.Claimable = taskProgressReached(after)
	if after.Claimed {
		out.Status = "done"
		out.Claimed = true
		out.Message = message + "；服务端已确认领取"
		return out
	}
	if !out.Claimable {
		out.Status = "awaiting_progress"
		out.Message = message + "；服务端进度尚未达标，等待入账后再确认"
		return out
	}
	return p.claimAutoTask(a, act, message, before, after)
}

func (p *Panel) claimAutoTask(a *auth.Auth, act *autoAction, message string, before, after *upstream.Task) autoTaskOutcome {
	out := autoTaskOutcome{
		Status:         "done",
		Message:        message,
		ProgressBefore: taskProgressText(before),
		ProgressAfter:  taskProgressText(after),
		Claimable:      true,
		Attempt:        act.Attempt,
	}
	var credit, energy int64
	var err error
	if isMPTaskCode(act.TaskCode) {
		credit, energy, err = p.cfg.Upstream.ClaimRewardMP(a, act.TaskCode)
	} else {
		credit, energy, err = p.cfg.Upstream.ClaimReward(a, act.TaskCode)
	}
	if err != nil {
		out.Status = "claim_pending"
		out.ClaimError = err.Error()
		out.Message = message + "；任务已达标，但领奖失败: " + err.Error()
		return out
	}
	out.Claimed = true
	out.Credit, out.Energy = credit, energy
	if credit > 0 || energy > 0 {
		out.Message = message + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
	} else {
		out.Message = message + "；领奖接口已确认（奖励此前可能已领取）"
	}
	return out
}

// truncateStr 截断错误文本（避免把上游长响应原样透给前端）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// 各任务动作实现
// ---------------------------------------------------------------------------

// reportGap 连续上报之间的间隔（对齐上游脚本实测的 1.05s 口径，避免风控）。
var reportGap = 1050 * time.Millisecond

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(p *Panel, a *auth.Auth) (string, error) {
	t, err := p.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := p.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			return "", fmt.Errorf("上报第 %d/%d 条失败: %w", i+1, need, err)
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func runFirstBuddy(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap) // 给上游事件处理留时间（脚本实测口径）
	if err := p.cfg.Upstream.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := p.cfg.Upstream.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(p *Panel, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	// 1. accept（报名；失败不阻塞——行为事件才是判据）
	if err := p.cfg.Upstream.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（继续走行为链路）", code, err)
	}
	time.Sleep(reportGap)
	// 2. 真实对话一次（判据的最直接证据）
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := p.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{})
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	// 读干 SSE（网关对上游强制 flow：不读完会残留连接）
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	// 3. 对齐模型的上报（触发进度）
	if err := p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "", fmt.Errorf("对话已完成，但进度上报失败: %w", err)
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次）。
// 2026-09-12 三账号实测验证：以桌面指纹（extName=workbuddy-desktop）向
// copilot.tencent.com/v2/report 上报完整对话事件链（agent_task_created →
// chat_message_response isSuccessful=true 等 6 事件），纯 API 即可点亮并领奖
// （紫川/人杰2 两账号无桌面客户端登录状态下 3 秒内 0/1 → 1/1）。
// Hp_Appearance 的纯 API set 不计分（需客户端在主题下活跃），区别对待。
func runRichMeow(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链（agent_task_created→chat_response）", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用）。
// 2026-09-12 两账号实测：buddyapp 五连事件（discover→show→enter→auth_confirm→
// bind_skip）以 workbuddy-desktop 指纹上报即点亮，服务端不校验真实授权。
// 用企鹅教师助手（Buddy_App_QQ 判据应用）作载体，同一组事件同时满足
// Buddy_App「进入任一应用」——两个表项共用本 run，幂等由任务状态跳过兜底。
func runBuddyApp(p *Panel, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（设置自动化任务）。
// 2026-09-12 两账号实测：automated_task_create_suc 事件纯 API 上报即点亮，
// 无需真实创建定时任务。
func runAutomationCreate(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopAutomationCreateEvent("wb2api 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（体验资料库）。
// 2026-09-12 三账号实测：web 域 /v2/report 上报 web_element_click
// (elementId=library_doc_intro_click) 即点亮（space 的 open/WS/inlong 都不是判据）。
func runLibraryRead(p *Panel, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.cfg.Upstream.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子，夜间 23:00–08:00 计数）。
// 判据 = 夜间窗口内 glm-5.2 真实对话 + chat 事件上报（WorkBuddy-Daily 实测口径）。
// 窗口外不做（提示等排程）；网关 blackcat_hours（默认 23 点）排程会自动补足。
func runBlackCat(p *Panel, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；网关会在每日 23 点自动补足", nil
	}
	need, err := p.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := p.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		return "", fmt.Errorf("完成 %d/%d 次后中断: %w", ok, need, err)
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（尝鲜热门技能）。
// 2026-09-12 判据（紫川手动完成抓包 row 209）：`skill_info` 事件（桌面指纹）——
// {id:<技能名>, skillId, skillVersion, toolStatus:"success", fileCount,
// source:"workbuddy-desktop"} JOIN 真实会话（conversationId/requestId=服务端
// id）。此前的 skill_request_send/skill_installed/skill_action 全是错误方向。
// 人杰2 实测 0/1 → 1/1 点亮。
func runSkillFresh(p *Panel, a *auth.Auth) (string, error) {
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
// 2026-09-12 判据（真实样本 Sunny row 868）：与 expert_5 同构，但两处差异——
// chat 链的 agent_task_created 需带 has_expert:true + expert_id（我们默认 false），
// expert_actual_use 的 mode 为 "LOCAL"（非 craft）。id 固定为轻量云专家
// ex_2cvvUZQhDyeJ；requestId 必须是真实 chat 的服务端 id。紫川/人杰2 实测点亮。
func runExpertLighthouse(p *Panel, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表若命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := p.cfg.Upstream.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	// 对齐真实样本细节：轻量云专家 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runAppearance 完成 Hp_Appearance（换主题）。
// 2026-09-12 紫川/人杰2 实测（desktopverify12）：判据是 appearance_skin_apply
// 事件 {action:"apply",source:"settings_close",id:<resourceKey>,...}（客户端在
// 主题生效状态下离开设置页时上报）——早期"纯 API set 不计分"的结论不准确，
// 真相是当时只调了 appearance/set 没发事件。组合：set API 留痕 + 事件上报。
func runAppearance(p *Panel, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // 和平精英激战金秋（Hp_Appearance 判据主题）
	if err := p.cfg.Upstream.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（使用 5 个模板创建任务）。
// 2026-09-12 三账号实测：agent_task_created_with_template + template_used 事件组
// （JOIN chat 链）一次上报 5 组即 5/5 点亮。template_id 服务端不校验真实性。
func runTemplateUse(p *Panel, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return "", fmt.Errorf("第 %d 组模板事件上报失败: %w", i+1, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 Dialog 中发送 Prompt）。
// 判据是 playbook_prompt_send（Dialog 发送）而非卡片曝光/点击——asar 逆向确认。
func runPlaybookPrompt(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计创意模式创建画布，+300 分）。
// 判据是 wbx_design_canvas_task_create/open（Ardot create_design 工具完成遥测）。
func runCreateCanvas(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链的间隔（真实使用节奏，v11 实测 8s 成功率 100%）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（使用 5 个平台专家）。
// 2026-09-12 三账号实测公式：真实专家列表（id 必须真实存在）→ 召唤链
// （summon_click/summoned）→ 真实 chat 拿服务端 requestId → expert_actual_use。
// 自造专家 id 或自造 requestId 均不计数。
func runExpertUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（使用 3 个专家团，expertType=team）。
func runExpertTeamUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

// runExpertBatch 专家召唤+使用的公共实现。失败逐个继续，返回汇总信息。
func runExpertBatch(p *Panel, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := p.cfg.Upstream.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		// 召唤链（web_element_click + summon_click + summoned）。
		summonEvents := upstream.DesktopExpertSummonSequence(e)
		if err := p.cfg.Upstream.ReportDesktopEvent(a, summonEvents...); err != nil {
			fail++
			continue
		}
		// 真实 chat（带 X-Expert-Id）→ 服务端 requestId。
		conv, req, cerr := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		// 使用事件（JOIN 服务端 requestId）+ chat 链。
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	if ok < count {
		return "", fmt.Errorf("专家召唤+使用链仅成功 %d/%d 位（%d 位失败）", ok, count, fail)
	}
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// ---------------------------------------------------------------------------
// 全量自动完成
// ---------------------------------------------------------------------------

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。
// 供「一键完成全部可自动任务」使用；单项失败不影响后续项。
//
// 流程：先把所有未接受的任务批量 accept（规范状态机；上游脚本建议"先 accept"），
// 再逐项执行行为链路。accept 不是进度产生的必要条件，但让后续状态流转规范。
func (p *Panel) runAutoAll(a *auth.Auth) []map[string]any {
	return p.runAutoAllObserved(context.Background(), a, nil)
}

type autoTaskRunEvent struct {
	Kind      string // phase, task_start, result
	TaskCode  string
	Result    map[string]any
	Completed bool // true only for an autoAction result, not batch acceptance
}

// runAutoAllObserved 对全量任务执行流程提供进度事件。ctx 在每个网络阶段、
// 自动任务项开始前检查；已有上游调用本身保持原超时语义，取消后不再发起下一项。
func (p *Panel) runAutoAllObserved(ctx context.Context, a *auth.Auth, observe func(autoTaskRunEvent)) []map[string]any {
	var out []map[string]any
	if ctx == nil {
		ctx = context.Background()
	}
	emit := func(event autoTaskRunEvent) {
		if observe != nil {
			observe(event)
		}
	}
	if a == nil {
		return []map[string]any{{"task_code": "(全部)", "status": "error", "message": "账号凭证不可用"}}
	}
	if a.IsGlobal() {
		return []map[string]any{{"task_code": "(全部)", "status": "error", "message": globalTaskWriteMessage}}
	}
	if ctx.Err() != nil {
		return out
	}

	// 阶段 0：批量接受尚未接受的任务（失败不阻塞——行为事件才是进度唯一判据）。
	emit(autoTaskRunEvent{Kind: "phase", TaskCode: "(批量接受)"})
	if ctx.Err() == nil {
		if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil && ctx.Err() == nil {
			var codes []string
			for _, t := range tasks {
				if !t.Claimed && !taskProgressReached(&t) && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
					codes = append(codes, t.TaskCode)
				}
			}
			if len(codes) > 0 && ctx.Err() == nil {
				emit(autoTaskRunEvent{Kind: "phase", TaskCode: "(批量接受)"})
				if ctx.Err() != nil {
					return out
				}
				if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
					result := map[string]any{
						"task_code": "(批量接受)", "status": "error",
						"message": "接受任务失败（不阻塞后续）: " + err.Error(),
					}
					out = append(out, result)
					emit(autoTaskRunEvent{Kind: "result", TaskCode: "(批量接受)", Result: result})
				} else {
					result := map[string]any{
						"task_code": "(批量接受)", "status": "accepted",
						"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
					}
					out = append(out, result)
					emit(autoTaskRunEvent{Kind: "result", TaskCode: "(批量接受)", Result: result})
					waitForContext(ctx, reportGap)
				}
			}
		}
	}

	// 阶段 0b：小程序口径任务单独接受（默认列表不含 mp 码；失败不阻塞）。
	if ctx.Err() == nil {
		emit(autoTaskRunEvent{Kind: "phase", TaskCode: "(批量接受-mp)"})
		if ctx.Err() == nil {
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil && ctx.Err() == nil {
				var mpCodes []string
				for _, t := range mpTasks {
					if !t.Claimed && !taskProgressReached(&t) && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
						mpCodes = append(mpCodes, t.TaskCode)
					}
				}
				if len(mpCodes) > 0 && ctx.Err() == nil {
					emit(autoTaskRunEvent{Kind: "phase", TaskCode: "(批量接受-mp)"})
					if ctx.Err() != nil {
						return out
					}
					if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
						result := map[string]any{
							"task_code": "(批量接受-mp)", "status": "error",
							"message": "接受小程序任务失败（不阻塞后续）: " + err.Error(),
						}
						out = append(out, result)
						emit(autoTaskRunEvent{Kind: "result", TaskCode: "(批量接受-mp)", Result: result})
					} else {
						result := map[string]any{
							"task_code": "(批量接受-mp)", "status": "accepted",
							"message": fmt.Sprintf("已接受 %d 个小程序任务", len(mpCodes)),
						}
						out = append(out, result)
						emit(autoTaskRunEvent{Kind: "result", TaskCode: "(批量接受-mp)", Result: result})
						waitForContext(ctx, reportGap)
					}
				}
			}
		}
	}

	for _, act := range autoActions {
		if ctx.Err() != nil {
			break
		}
		emit(autoTaskRunEvent{Kind: "task_start", TaskCode: act.TaskCode})
		if ctx.Err() != nil {
			break
		}
		outcome := p.executeAutoTask(a, &act)
		result := outcome.resultMap(&act)
		out = append(out, result)
		emit(autoTaskRunEvent{Kind: "result", TaskCode: act.TaskCode, Result: result, Completed: true})
		if outcome.ActionRun {
			if !waitForContext(ctx, reportGap) {
				break
			}
		}
	}
	return out
}

func waitForContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// accountTaskAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	job, started, err := p.StartAccountTaskJob(uid, "manual")
	if err != nil {
		writeTaskJobError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": started, "job": job})
}
