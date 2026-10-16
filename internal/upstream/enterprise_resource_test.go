package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestEnterpriseResourceMapsAllocatedQuota 企业版额度走 get-enterprise-user-usage：
// credit 是**已用**（与个人口径相反），故 remain = limitNum - credit、total = limitNum。
func TestEnterpriseResourceMapsAllocatedQuota(t *testing.T) {
	end := time.Now().Add(10 * 24 * time.Hour).Format(packageEndLayout)
	var gotPath, gotEnt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEnt = r.Header.Get("X-Enterprise-Id")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":812.45,"limitNum":2000,` +
			`"cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"` + end + `",` +
			`"cycleResetTime":"2026-10-27 00:00:00"}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	// soon=0：不分桶，但最早到期批次仍要记录。
	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasSuffix(gotPath, "/v2/billing/meter/get-enterprise-user-usage") {
		t.Errorf("path=%s want .../get-enterprise-user-usage", gotPath)
	}
	if gotEnt != "ent-1" {
		t.Errorf("X-Enterprise-Id=%q want ent-1", gotEnt)
	}
	// 2000 - round(812.45) = 1188。
	if remain != 1188 || total != 2000 {
		t.Errorf("remain=%d total=%d want 1188/2000", remain, total)
	}
	if expiring != 0 {
		t.Errorf("soon=0 不应分桶，expiring=%d", expiring)
	}
	if earliestAt.IsZero() || earliestRemaining != 1188 {
		t.Errorf("最早到期批次未记录：at=%v remaining=%d", earliestAt, earliestRemaining)
	}

	// soon 覆盖周期末 → 整份剩余额度计入 expiring（prefer_expiring 据此优先消耗）。
	_, _, expiring2, _, _, err := c.UserResourceDetailedWithExpiry(a, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if expiring2 != 1188 {
		t.Errorf("周期内 expiring=%d want 1188", expiring2)
	}
}

// TestEnterpriseResourceUnlimited 上游 limitNum=-1 表示不限量：池内用大剩余量表示，
// total 记 -1（面板显示「不限」），且不参与周期分桶（不限量没有"未用完作废"语义）。
func TestEnterpriseResourceUnlimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":0,"limitNum":-1}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(a, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != enterpriseUnlimitedRemain || total != enterpriseUnlimitedTotal {
		t.Errorf("remain=%d total=%d want %d/-1", remain, total, enterpriseUnlimitedRemain)
	}
	if expiring != 0 || !earliestAt.IsZero() || earliestRemaining != 0 {
		t.Errorf("不限量不应有周期批次：expiring=%d at=%v remaining=%d", expiring, earliestAt, earliestRemaining)
	}
}

// TestEnterpriseResourceSnakeCase 兼容上游第二套字段命名（桌面端另一条解析路径读
// limit_num/used_num）。
func TestEnterpriseResourceSnakeCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"used_num":500,"limit_num":1500}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}

	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 1000 || total != 1500 {
		t.Errorf("remain=%d total=%d want 1000/1500", remain, total)
	}
}

// TestPersonalResourceStaysOnPackageEndpoint 个人号零回归：仍走资源包端点，
// 企业端点一次都不发。
func TestPersonalResourceStaysOnPackageEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "get-enterprise-user-usage") {
			t.Errorf("个人号不应调用企业额度端点")
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
			`[{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}]}}}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 900 || total != 1000 {
		t.Errorf("remain=%d total=%d want 900/1000", remain, total)
	}
	if len(paths) == 0 {
		t.Fatal("应至少请求一次资源包端点")
	}
}

// TestAPIKeyRoutesToEnterpriseBilling 静态 API Key 与企业版同口径：额度查询分流到
// get-enterprise-user-usage（个人 get-user-resource 对二者均返回空 Accounts），
// 且不注入 X-Enterprise-Id（无企业身份）。
func TestAPIKeyRoutesToEnterpriseBilling(t *testing.T) {
	var paths []string
	var gotEnt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		gotEnt = r.Header.Get("X-Enterprise-Id")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"limitNum":500}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "ck_9f3a2b1c.deadbeef"}

	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(paths) == 0 || !strings.HasSuffix(paths[0], "/v2/billing/meter/get-enterprise-user-usage") {
		t.Errorf("paths=%v want .../get-enterprise-user-usage", paths)
	}
	if gotEnt != "" {
		t.Errorf("API Key 不应注入 X-Enterprise-Id，实际=%q", gotEnt)
	}
	// 500 - 100 = 400。
	if remain != 400 || total != 500 {
		t.Errorf("remain=%d total=%d want 400/500", remain, total)
	}
}
