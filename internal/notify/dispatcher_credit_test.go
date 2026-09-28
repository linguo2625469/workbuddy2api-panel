package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func captureBody(t *testing.T, got *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got.Store(m)
		w.Write([]byte(`{"errcode":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNotifyRendersTemplate(t *testing.T) {
	var got atomic.Value
	srv := captureBody(t, &got)

	n := NewNotifier(srv.Client(), time.Minute)
	n.SetChannels([]Channel{{
		Name: "t", Type: "wechat", Enabled: true,
		Config: map[string]any{
			"webhook_url": srv.URL,
			"template":    "账号 {{nickname}} 事件 {{event}} 剩余 {{credits}}",
		},
	}})
	n.Notify(EventBreaker, "u1", "默认正文", map[string]string{
		"nickname": "小号", "event": EventBreaker, "credits": "88",
	})
	time.Sleep(200 * time.Millisecond)
	m, _ := got.Load().(map[string]any)
	text, _ := m["text"].(map[string]any)["content"].(string)
	if !strings.Contains(text, "账号 小号 事件 breaker 剩余 88") {
		t.Fatalf("text=%q", text)
	}
	if strings.Contains(text, "默认正文") {
		t.Fatalf("应渲染模板而非默认正文: %q", text)
	}
}

func TestNotifyTemplateFallback(t *testing.T) {
	var got atomic.Value
	srv := captureBody(t, &got)

	n := NewNotifier(srv.Client(), time.Minute)
	n.SetChannels([]Channel{{
		Name: "t", Type: "wechat", Enabled: true,
		Config: map[string]any{
			"webhook_url": srv.URL,
			"template":    "非法 {{unknown_var}}", // 非法模板 → 回落默认
		},
	}})
	n.Notify(EventDisabled, "u1", "默认正文", map[string]string{"nickname": "x"})
	time.Sleep(200 * time.Millisecond)
	m, _ := got.Load().(map[string]any)
	text, _ := m["text"].(map[string]any)["content"].(string)
	if !strings.Contains(text, "默认正文") {
		t.Fatalf("text=%q", text)
	}
}

func TestReportCreditAggregation(t *testing.T) {
	var calls atomic.Int32
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got.Store(m)
		w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	n := NewNotifier(srv.Client(), time.Minute)
	n.creditWin = 120 * time.Millisecond // 短窗口验证完整聚合推送
	n.SetChannels([]Channel{{
		Name: "t", Type: "wechat", Enabled: true,
		Config: map[string]any{"webhook_url": srv.URL},
	}})
	// 窗口内三次消耗 → 应只推一条，consume=窗口总和、remain=最后一次快照
	n.ReportCredit("u1", "小号", 3, 97, 100)
	n.ReportCredit("u1", "小号", 2, 95, 100)
	n.ReportCredit("u1", "小号", 5, 90, 100)
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("窗口内多次消耗应聚合成 1 条，calls=%d", calls.Load())
	}
	m, _ := got.Load().(map[string]any)
	text, _ := m["text"].(map[string]any)["content"].(string)
	for _, want := range []string{"小号", "10", "90", "100"} { // 3+2+5=10
		if !strings.Contains(text, want) {
			t.Fatalf("text 缺 %q: %s", want, text)
		}
	}
}

func TestDefaultCreditMsg(t *testing.T) {
	msg := defaultCreditMsg("小号", 10, 90, 100)
	for _, want := range []string{"小号", "10", "90", "100"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("msg 缺 %q: %s", want, msg)
		}
	}
}

func TestFormatConsume(t *testing.T) {
	if got := formatConsume(10); got != "10" {
		t.Fatalf("got=%q", got)
	}
	if got := formatConsume(2.5); got != "2.5" {
		t.Fatalf("got=%q", got)
	}
}
