// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHour  int           // 每日签到小时，默认 9
	RefreshHours []int         // token 预刷新小时，默认 [3]
	RefreshSkew  time.Duration // 预刷新窗口，默认 24h
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	mu          sync.Mutex
	lastCheckin time.Time // 最近一次 RunCheckinNow 触发时间
	lastRefresh time.Time // 最近一次 RunRefreshNow 触发时间
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if s.cfg.CheckinHour == h {
				s.RunCheckinNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	s.mu.Lock()
	s.lastCheckin = time.Now()
	s.mu.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 签到（status → 未签到则 claim）
		checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a)
		if err != nil {
			log.Printf("checkin status %s: %v", st.UID, err)
		} else if !checkedIn && enable {
			if err := s.cfg.Upstream.CheckinClaim(a); err != nil {
				log.Printf("checkin claim %s: %v", st.UID, err)
			} else {
				log.Printf("checkin %s: ok", st.UID)
			}
		} else if checkedIn {
			log.Printf("checkin %s: already checked in", st.UID)
		}
		// 查积分 + 解冻
		remain, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			log.Printf("ent-usage %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
	}
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	s.mu.Lock()
	s.lastRefresh = time.Now()
	s.mu.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !a.NeedsRefresh(s.cfg.RefreshSkew) {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}

// Status 返回排程状态（面板展示用）：配置的小时点、下次触发、最近执行时间。
func (s *Scheduler) Status() map[string]any {
	now := time.Now()
	s.mu.Lock()
	lc, lr := s.lastCheckin, s.lastRefresh
	s.mu.Unlock()
	fmtT := func(t time.Time) any {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	return map[string]any{
		"checkin_hour":  s.cfg.CheckinHour,
		"refresh_hours": s.cfg.RefreshHours,
		"next_fire":     nextFire(now, all).Format(time.RFC3339),
		"last_checkin":  fmtT(lc),
		"last_refresh":  fmtT(lr),
	}
}
