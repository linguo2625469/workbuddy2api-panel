// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// nowForNightWindow 夜猫子窗口判定所用时钟（生产恒为 time.Now）。
//
// 之所以留一个可替换点：窗口守卫**先于**账号循环，若不钉住时钟，「企业号门控是否生效」
// 这条断言的判别力就完全取决于运行环境的墙钟——本机白天跑等于该用例什么都没测，
// 只有 CI 恰好落在 UTC 夜里才真正执行。属典型的「随环境静默失效」的测试。
var nowForNightWindow = time.Now

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(nowForNightWindow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		// 暂停选号（paused）账号跳过：夜猫子是全任务体系中唯一「整任务都是
		// 真实模型对话」的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），
		// 与「让位防风控」正面冲突。旅行/成长任务都是纯上报或领奖 RPC，照常跑。
		if st.Disabled || st.Paused {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if a.IsEnterpriseSession() {
			continue // 企业版 / 企业切换会话门控：无成长体系（growth 403 / claim-gift 400「企业账号不支持该操作」；切换会话不计分）
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
		time.Sleep(activityAccountDelay)
	}
}
