package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// compensationServer 起一个只回固定 body 的 billing 桩，返回指向它的 Client。
func compensationServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/compensation-status" {
			t.Errorf("path=%s want /billing/meter/compensation-status", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// TestGetCompensationStatusParsesAllFields 全字段解析（issue #189 实测口径）。
func TestGetCompensationStatusParsesAllFields(t *testing.T) {
	c := compensationServer(t, `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,`+
		`"credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}`)

	st, err := c.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GetCompensationStatus: %v", err)
	}
	if !st.Active {
		t.Error("Active=false want true")
	}
	if st.Claimed {
		t.Error("Claimed=true want false")
	}
	if st.CreditNum != 500 {
		t.Errorf("CreditNum=%d want 500", st.CreditNum)
	}
	if st.ValidityPeriod != 1 {
		t.Errorf("ValidityPeriod=%d want 1", st.ValidityPeriod)
	}
	if st.EndTime != "2026-10-22 23:59:59" {
		t.Errorf("EndTime=%q want 2026-10-22 23:59:59", st.EndTime)
	}
	if !st.Pending() {
		t.Error("Pending()=false want true（active 且未领且有额度）")
	}
}

// TestGetCompensationStatusNoActivityIsNotError 「当前无补偿活动」是 200 正常返回，
// 不是错误——这条语义一旦搞错，日常无活动的账号会把排程日志刷成一片错误。
func TestGetCompensationStatusNoActivityIsNotError(t *testing.T) {
	c := compensationServer(t, `{"code":0,"msg":"OK","data":{"claimed":false,"active":false,"credit_num":0}}`)

	st, err := c.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("无活动不应返回 error，实际: %v", err)
	}
	if st.Active {
		t.Error("Active=true want false")
	}
	if st.Pending() {
		t.Error("Pending()=true want false（无活动）")
	}
}

// TestGetCompensationStatusServerError HTTP 非 2xx → error。
func TestGetCompensationStatusServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on 500")
	}
}

// TestGetCompensationStatusBusinessCode 业务 code 非 0 → error。
func TestGetCompensationStatusBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":10001,"msg":"企业账号不支持该操作"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on non-zero business code")
	}
}

// TestCompensationStatusPendingGate 三条与门各自独立成立，避免出现
// 「active 但已领」「活动在但额度为 0」被误判成待领而反复调领取接口。
func TestCompensationStatusPendingGate(t *testing.T) {
	cases := []struct {
		name string
		st   CompensationStatus
		want bool
	}{
		{"无活动", CompensationStatus{Active: false, Claimed: false, CreditNum: 500}, false},
		{"已领取", CompensationStatus{Active: true, Claimed: true, CreditNum: 500}, false},
		{"额度为零", CompensationStatus{Active: true, Claimed: false, CreditNum: 0}, false},
		{"待领取", CompensationStatus{Active: true, Claimed: false, CreditNum: 500}, true},
		{"全零值", CompensationStatus{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.st.Pending(); got != tc.want {
				t.Errorf("Pending()=%v want %v（%+v）", got, tc.want, tc.st)
			}
		})
	}
}
