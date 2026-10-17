package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// TestOverviewIncludesExpiryBuckets 钉住面板接线：池内写入的按日到期批次必须原样
// 出现在 /overview 响应里，否则前端快照只能退回旧的「最早一批」字段（口径错误）。
// 同时钉住余额刷新节奏/时刻两个字段——前端据此标注「每 5 分钟刷新，x 分钟前更新」。
func TestOverviewIncludesExpiryBuckets(t *testing.T) {
	pl := pool.New("")
	sch := scheduler.New(scheduler.Config{Pool: pl})
	sch.SetBalanceInterval(5 * time.Minute)
	sch.RunBalanceRefreshNow() // 空池 → 不打上游，只落完成时刻

	p := New(Config{Version: "test", Pool: pl, Scheduler: sch})
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	now := time.Now()
	at1 := now.Add(24 * time.Hour)
	at2 := now.Add(48 * time.Hour)
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 300, Total: 300, Expiring: 100,
		ExpiryBuckets: []pool.ExpiryBucket{
			{Date: at1.Format("2006-01-02"), ExpireAt: at1, Remain: 100},
			{Date: at2.Format("2006-01-02"), ExpireAt: at2, Remain: 200},
		},
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BalanceRefreshSec   int       `json:"balance_refresh_sec"`
		BalanceRefreshedAt  time.Time `json:"balance_refreshed_at"`
		Accounts            []struct {
			CreditsExpiryBuckets []pool.ExpiryBucket `json:"credits_expiry_buckets"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(resp.Accounts) != 1 || len(resp.Accounts[0].CreditsExpiryBuckets) != 2 {
		t.Fatalf("overview accounts=%+v want 2 expiry buckets", resp.Accounts)
	}
	if got := resp.Accounts[0].CreditsExpiryBuckets[0]; got.Remain != 100 || got.Date != at1.Format("2006-01-02") {
		t.Fatalf("first bucket=%+v", got)
	}
	if resp.BalanceRefreshSec != 300 {
		t.Fatalf("balance_refresh_sec=%d want 300", resp.BalanceRefreshSec)
	}
	if resp.BalanceRefreshedAt.IsZero() {
		t.Fatal("balance_refreshed_at 为零值：前端无法标注数据年龄")
	}
}
