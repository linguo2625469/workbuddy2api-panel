package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifyDedupe(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	n := NewNotifier(srv.Client(), time.Minute)
	n.SetChannels([]Channel{{
		Name: "t", Type: "wechat", Enabled: true,
		Config: map[string]any{"webhook_url": srv.URL},
	}})
	n.Notify(EventBreaker, "uid1", "m1", nil)
	n.Notify(EventBreaker, "uid1", "m2", nil) // 窗口内同 key 应被防抖
	n.Notify(EventBreaker, "uid2", "m3", nil) // 不同账号正常推
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls=%d want 2（防抖应吞掉一次）", got)
	}
}

func TestNotifyEventFilter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	n := NewNotifier(srv.Client(), time.Minute)
	n.SetChannels([]Channel{
		{Name: "a", Type: "wechat", Enabled: true, Events: []string{EventDisabled},
			Config: map[string]any{"webhook_url": srv.URL}},
		{Name: "b", Type: "wechat", Enabled: true, // 空 events = 全部
			Config: map[string]any{"webhook_url": srv.URL}},
		{Name: "c", Type: "wechat", Enabled: false, // 禁用不推
			Config: map[string]any{"webhook_url": srv.URL}},
	})
	n.Notify(EventCheckinFailed, "", "签到失败", nil)
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls=%d want 1（a 未订阅 checkin_failed、c 已禁用）", got)
	}
}

func TestNotifyChannelFailureNonBlocking(t *testing.T) {
	n := NewNotifier(&http.Client{Timeout: time.Second}, time.Minute)
	n.SetChannels([]Channel{
		{Name: "bad", Type: "wechat", Enabled: true,
			Config: map[string]any{"webhook_url": "http://127.0.0.1:1/x"}},
	})
	// 不应 panic、不应阻塞（异步发送，错误只记日志）。
	n.Notify(EventDisabled, "u", "x", nil)
}

func TestSetChannelsReplace(t *testing.T) {
	n := NewNotifier(nil, time.Minute)
	n.SetChannels([]Channel{{Name: "x"}})
	n.SetChannels([]Channel{{Name: "y"}})
	got := n.Channels()
	if len(got) != 1 || got[0].Name != "y" {
		t.Fatalf("channels=%v", got)
	}
}

func TestSubscribed(t *testing.T) {
	c := Channel{}
	if !c.subscribed(EventBreaker) {
		t.Fatal("空 Events 应订阅全部")
	}
	c.Events = []string{EventDisabled}
	if c.subscribed(EventBreaker) {
		t.Fatal("未订阅的事件应返回 false")
	}
	if !c.subscribed(EventDisabled) {
		t.Fatal("订阅的事件应返回 true")
	}
}

func TestTestMessage(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got.Store(string(buf[:n]))
	}))
	defer srv.Close()
	n := NewNotifier(srv.Client(), time.Minute)
	err := n.Test(context.Background(), Channel{Name: "测", Type: "wechat", Config: map[string]any{"webhook_url": srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := got.Load().(string)
	if body == "" {
		t.Fatal("应收到测试消息")
	}
	if !strings.Contains(body, "通知渠道测试") || !strings.Contains(body, "测") {
		t.Fatalf("body=%s", body)
	}
}
