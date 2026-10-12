package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 余额查询成功必须通知观察者一次（uid + 余额）：这是积分历史留痕的唯一数据入口。
func TestCreditObserverNotifiedOncePerQuery(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
		`{"PackageName":"p","CycleCapacitySize":30,"CycleCapacityRemain":30,"CycleCapacityUsed":0}` +
		`]}}}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, payload), nil
	})
	type call struct {
		uid     string
		credits int64
	}
	var calls []call
	c.SetCreditObserver(func(uid string, credits int64) { calls = append(calls, call{uid, credits}) })

	remain, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 30 {
		t.Fatalf("remain=%d want 30", remain)
	}
	if len(calls) != 1 || calls[0].uid != "u1" || calls[0].credits != 30 {
		t.Fatalf("观察者应被回调一次 (u1,30)，得到 %+v", calls)
	}

	// UserResource / UserResourceDetailed 都是委托调用：每次查询只回调一次，不重复。
	if _, _, err := c.UserResource(&auth.Auth{AccessToken: "at", UID: "u2"}); err != nil {
		t.Fatalf("resource: %v", err)
	}
	if len(calls) != 2 || calls[1].uid != "u2" || calls[1].credits != 30 {
		t.Fatalf("委托路径应恰好再回调一次，得到 %+v", calls)
	}
}

// 查询失败不通知：错误路径返回的 0 余额若被留痕，会在下一次成功时造出一条假变动。
// 用业务错误（code!=0）注入而不是非法 JSON：后者会被重试策略当成瞬时错误，
// 白白等满 6s 退避（见 retryBillingTransient）。
func TestCreditObserverSilentOnBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":400,"msg":"bad request"}`), nil
	})
	n := 0
	c.SetCreditObserver(func(string, int64) { n++ })
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0); err == nil {
		t.Fatal("业务错误应返回错误")
	}
	if n != 0 {
		t.Fatalf("失败不应通知观察者，得到 %d 次", n)
	}
}

// 未挂载 / 注销观察者时必须零开销且不 panic（观察者是可选旁路，不得改变既有行为）。
func TestCreditObserverOptionalAndDetachable(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, payload), nil
	})
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0); err != nil {
		t.Fatalf("未挂载观察者时查询应正常: %v", err)
	}
	n := 0
	c.SetCreditObserver(func(string, int64) { n++ })
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("挂载后应通知 1 次，得到 %d", n)
	}
	c.SetCreditObserver(nil)
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("注销后不应再通知，得到 %d", n)
	}
}

// 观察者是纯旁路：它 panic 绝不能打断余额查询主流程——调度器 goroutine 里未恢复的
// panic 会直接带走整个进程。recover 之后查询结果必须照常返回。
func TestCreditObserverPanicIsContained(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"p","CycleCapacitySize":30,"CycleCapacityRemain":30,"CycleCapacityUsed":0}]}}}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, payload), nil
	})
	c.SetCreditObserver(func(string, int64) { panic("observer boom") })

	remain, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("观察者 panic 不应影响查询: %v", err)
	}
	if remain != 30 {
		t.Fatalf("remain=%d want 30", remain)
	}
}
