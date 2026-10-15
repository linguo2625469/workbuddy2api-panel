package gateway

// scheduler.go — background keep-alive jobs:
//   - daily warm: every hour, warm accounts not yet warmed today (the
//     upstream dedups daily-active by device_mid+date, so re-sending is the
//     official client shape);
//   - quota monitor: periodic billing refresh (QUOTA_REFRESH_MIN, 0=off) so
//     the dashboard and dead-marks stay fresh without pressing Refresh.

import (
	"context"
	"log"
	"strconv"
	"time"
)

// startSchedulers launches the background loops (idempotent, guarded).
func (s *Server) startSchedulers() {
	s.warmOnce.Do(func() {
		go s.dailyWarmLoop()
	})
	if s.quotaRefreshMin > 0 {
		s.quotaOnce.Do(func() {
			go s.quotaMonitorLoop()
		})
	}
}

func (s *Server) dailyWarmLoop() {
	for {
		s.warmDueAccounts()
		time.Sleep(time.Hour)
	}
}

// warmDueAccounts warms every account whose last warm date is not today.
func (s *Server) warmDueAccounts() {
	accs, err := s.store.Accounts()
	if err != nil {
		return
	}
	today := time.Now().Format("2006-01-02")
	for _, a := range accs {
		if a.Status != "" { // expired/revoked/hidden — skip
			continue
		}
		// per-account date stored in settings: warm_last_<id>
		if s.store.Setting("warm_last_"+strconv.FormatInt(a.ID, 10), "") == today {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		rep := s.up.WarmAccount(ctx, a.JWT, a.DeviceMid)
		cancel()
		s.store.SetSetting("warm_last_"+strconv.FormatInt(a.ID, 10), today)
		log.Printf("[warm-daily] %s %s", maskJWT(a.JWT), rep.Plans)
	}
}

func (s *Server) quotaMonitorLoop() {
	interval := time.Duration(s.quotaRefreshMin) * time.Minute
	for {
		time.Sleep(interval)
		checked, dropped := s.refreshAllBilling()
		if checked > 0 || dropped > 0 {
			log.Printf("[quota-monitor] checked=%d expired=%d", checked, dropped)
		}
	}
}
