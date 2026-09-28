package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureServer 记录请求方法与 body，可校验 header。
type captureServer struct {
	t      *testing.T
	srv    *httptest.Server
	calls  atomic.Int32
	method string
	body   []byte
	header http.Header
	status int // 返回状态码，默认 200
	resp   string
}

func newCapture(t *testing.T, status int, resp string) *captureServer {
	c := &captureServer{t: t, status: status, resp: resp}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		c.method = r.Method
		c.header = r.Header.Clone()
		body := make([]byte, 0, 512)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		c.body = body
		w.WriteHeader(status)
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *captureServer) bodyJSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.body, &m); err != nil {
		t.Fatalf("body 非 JSON: %v, body=%s", err, c.body)
	}
	return m
}

func TestDingTalkPlain(t *testing.T) {
	c := newCapture(t, 200, `{"errcode":0}`)
	err := Send(context.Background(), "dingtalk", map[string]any{"webhook_url": c.srv.URL + "?access_token=x"}, "你好", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["msgtype"] != "markdown" {
		t.Fatalf("msgtype=%v", m["msgtype"])
	}
	md := m["markdown"].(map[string]any)
	if md["text"] != "你好" {
		t.Fatalf("text=%v", md["text"])
	}
}

func TestDingTalkSign(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
	}))
	defer srv.Close()
	err := Send(context.Background(), "dingtalk", map[string]any{
		"webhook_url": srv.URL + "?access_token=x",
		"secret":      "SECabc",
	}, "msg", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "timestamp=") || !strings.Contains(gotQuery, "sign=") {
		t.Fatalf("缺少签名参数: %s", gotQuery)
	}
}

func TestFeishuSign(t *testing.T) {
	c := newCapture(t, 200, `{"code":0}`)
	err := Send(context.Background(), "feishu", map[string]any{
		"webhook_url": c.srv.URL,
		"secret":      "s3cr3t",
	}, "msg", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["msg_type"] != "text" {
		t.Fatalf("msg_type=%v", m["msg_type"])
	}
	if m["timestamp"] == nil || m["timestamp"] == "" {
		t.Fatal("飞书带 secret 时应带 timestamp")
	}
	if m["sign"] == nil || m["sign"] == "" {
		t.Fatal("飞书带 secret 时应带 sign")
	}
}

func TestFeishuBizError(t *testing.T) {
	c := newCapture(t, 200, `{"code":19021,"msg":"sign not match"}`)
	err := Send(context.Background(), "feishu", map[string]any{"webhook_url": c.srv.URL}, "msg", c.srv.Client())
	if err == nil || !strings.Contains(err.Error(), "sign not match") {
		t.Fatalf("err=%v", err)
	}
}

func TestBark(t *testing.T) {
	c := newCapture(t, 200, `{"code":200}`)
	err := Send(context.Background(), "bark", map[string]any{
		"server_url": c.srv.URL,
		"device_key": "devkey",
		"title":      "标题",
	}, "正文", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["device_key"] != "devkey" || m["title"] != "标题" || m["body"] != "正文" {
		t.Fatalf("m=%v", m)
	}
}

func TestBarkMissingKey(t *testing.T) {
	err := Send(context.Background(), "bark", map[string]any{}, "x", http.DefaultClient)
	if err == nil {
		t.Fatal("缺少 device_key 应报错")
	}
}

func TestWechat(t *testing.T) {
	c := newCapture(t, 200, `{"errcode":0}`)
	err := Send(context.Background(), "wechat", map[string]any{"webhook_url": c.srv.URL}, "企业微信消息", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["msgtype"] != "text" {
		t.Fatalf("msgtype=%v", m["msgtype"])
	}
	if m["text"].(map[string]any)["content"] != "企业微信消息" {
		t.Fatalf("m=%v", m)
	}
}

func TestPushPlus(t *testing.T) {
	c := newCapture(t, 200, `{"code":200}`)
	err := Send(context.Background(), "pushplus", map[string]any{
		"server_url": c.srv.URL,
		"token":      "tk",
		"topic":      "grp",
	}, "内容", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["token"] != "tk" || m["content"] != "内容" || m["topic"] != "grp" {
		t.Fatalf("m=%v", m)
	}
}

func TestWebhookCustomHeaders(t *testing.T) {
	c := newCapture(t, 200, `{}`)
	err := Send(context.Background(), "webhook", map[string]any{
		"webhook_url": c.srv.URL,
		"http_method": "POST",
		"headers":     `{"Authorization":"Bearer abc"}`,
	}, "x", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.header.Get("Authorization"); got != "Bearer abc" {
		t.Fatalf("Authorization=%q", got)
	}
	m := c.bodyJSON(t)
	if m["message"] != "x" {
		t.Fatalf("m=%v", m)
	}
}

func TestWebhookPUT(t *testing.T) {
	c := newCapture(t, 200, `{}`)
	err := Send(context.Background(), "webhook", map[string]any{
		"webhook_url": c.srv.URL,
		"http_method": "PUT",
	}, "x", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if c.method != "PUT" {
		t.Fatalf("method=%s", c.method)
	}
}

func TestTelegram(t *testing.T) {
	c := newCapture(t, 200, `{"ok":true}`)
	// telegram 官方 api 地址固定；为可测试，支持 base_url 覆盖（面板侧不暴露）。
	err := Send(context.Background(), "telegram", map[string]any{
		"bot_token": "123:abc",
		"chat_id":   "42",
		"base_url":  c.srv.URL,
	}, "tg", c.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := c.bodyJSON(t)
	if m["chat_id"] != "42" || m["text"] != "tg" {
		t.Fatalf("m=%v", m)
	}
}

func TestUnknownType(t *testing.T) {
	err := Send(context.Background(), "slack", map[string]any{}, "x", http.DefaultClient)
	if err == nil {
		t.Fatal("未知类型应报错")
	}
}

func TestHTTPFailure(t *testing.T) {
	c := newCapture(t, 500, `boom`)
	err := Send(context.Background(), "wechat", map[string]any{"webhook_url": c.srv.URL}, "x", c.srv.Client())
	if err == nil {
		t.Fatal("500 应报错")
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := Send(ctx, "wechat", map[string]any{"webhook_url": srv.URL}, "x", srv.Client())
	if err == nil {
		t.Fatal("超时 ctx 应报错")
	}
}

func TestEmailValidation(t *testing.T) {
	// 不真正连 SMTP，只验证配置不完整时报错而不是 panic。
	err := Send(context.Background(), "email", map[string]any{"smtp_server": ""}, "x", http.DefaultClient)
	if err == nil {
		t.Fatal("配置不完整应报错")
	}
}
