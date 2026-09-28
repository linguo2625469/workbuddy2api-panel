// Package notify 通知渠道发送：钉钉/飞书/Bark/邮件/企业微信/Telegram/PushPlus/通用 Webhook。
//
// 渠道发送语义对齐 xianyu-auto-reply 的 notification_utils.py：
//   - 钉钉：markdown 消息，可选加签（HMAC-SHA256 + base64，拼到 URL query）；
//   - 飞书：text 消息，可选签名（timestamp + sign 字段放 body）；
//   - Bark/PushPlus：JSON POST，按业务返回码判定成败；
//   - 邮件：net/smtp 直连，465=SSL、587=STARTTLS（可 smtp_use_tls 覆盖）、25 明文；
//   - 企业微信/通用 Webhook：text JSON POST，Webhook 支持自定义 method/headers。
//
// 所有发送共享一个 *http.Client（由调用方注入，含超时）；邮件走 smtp 不经 http.Client。
package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Channel 面板配置里的一条通知渠道。
type Channel struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`             // dingtalk/feishu/bark/email/webhook/wechat/telegram/pushplus
	Config  map[string]any `json:"config"`           // 各渠道字段（webhook_url/secret/token...）
	Enabled bool           `json:"enabled"`
	Events  []string       `json:"events,omitempty"` // 订阅事件（checkin_failed/breaker/disabled）；空 = 全部
}

// Send 按渠道类型分发发送。httpc 为空时用带 10s 超时的默认 client。
func Send(ctx context.Context, chType string, cfg map[string]any, message string, httpc *http.Client) error {
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	switch chType {
	case "dingtalk", "ding_talk":
		return sendDingTalk(ctx, httpc, cfg, message)
	case "feishu", "lark":
		return sendFeishu(ctx, httpc, cfg, message)
	case "bark":
		return sendBark(ctx, httpc, cfg, message)
	case "email":
		return sendEmail(cfg, message)
	case "webhook":
		return sendWebhook(ctx, httpc, cfg, message)
	case "wechat":
		return sendWeChat(ctx, httpc, cfg, message)
	case "telegram":
		return sendTelegram(ctx, httpc, cfg, message)
	case "pushplus":
		return sendPushPlus(ctx, httpc, cfg, message)
	default:
		return fmt.Errorf("不支持的通知渠道类型: %s", chType)
	}
}

func str(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func postJSON(ctx context.Context, httpc *http.Client, method, rawURL string, headers map[string]string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, raw, nil
}

func sendDingTalk(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	webhook := str(cfg, "webhook_url")
	if webhook == "" {
		webhook = str(cfg, "config")
	}
	if webhook == "" {
		return fmt.Errorf("钉钉通知配置为空")
	}
	if secret := str(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "\n" + secret))
		sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		sep := "&"
		if !strings.Contains(webhook, "?") {
			sep = "?"
		}
		webhook += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": "WorkBuddy2API 通知",
			"text":  message,
		},
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost, webhook, nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("钉钉通知发送失败: HTTP %d", status)
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("钉钉通知发送失败: %s", r.ErrMsg)
	}
	return nil
}

func sendFeishu(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	webhook := str(cfg, "webhook_url")
	if webhook == "" {
		return fmt.Errorf("飞书通知配置为空")
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": message},
	}
	if secret := str(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
		mac.Write(nil)
		payload["timestamp"] = ts
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost, webhook, nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("飞书通知发送失败: HTTP %d", status)
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Code != 0 {
		return fmt.Errorf("飞书通知发送失败: %s", r.Msg)
	}
	return nil
}

func sendBark(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	server := str(cfg, "server_url")
	if server == "" {
		server = "https://api.day.app"
	}
	server = strings.TrimRight(server, "/")
	key := str(cfg, "device_key")
	if key == "" {
		return fmt.Errorf("Bark 设备密钥为空")
	}
	title := str(cfg, "title")
	if title == "" {
		title = "WorkBuddy2API 通知"
	}
	payload := map[string]any{
		"device_key": key,
		"title":      title,
		"body":       message,
		"sound":      strOr(cfg, "sound", "default"),
		"group":      strOr(cfg, "group", "workbuddy2api"),
	}
	if icon := str(cfg, "icon"); icon != "" {
		payload["icon"] = icon
	}
	if link := str(cfg, "url"); link != "" {
		payload["url"] = link
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost, server+"/push", nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("Bark 通知发送失败: HTTP %d", status)
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Code != 200 {
		return fmt.Errorf("Bark 通知发送失败: %s", r.Message)
	}
	return nil
}

func strOr(cfg map[string]any, key, def string) string {
	if v := str(cfg, key); v != "" {
		return v
	}
	return def
}

func sendEmail(cfg map[string]any, message string) error {
	server := str(cfg, "smtp_server")
	user := str(cfg, "email_user")
	pass := str(cfg, "email_password")
	to := str(cfg, "recipient_email")
	port := 587
	switch v := cfg["smtp_port"].(type) {
	case float64:
		port = int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			port = n
		}
	}
	if server == "" || user == "" || pass == "" || to == "" {
		return fmt.Errorf("邮件通知配置不完整（smtp_server/email_user/email_password/recipient_email）")
	}
	useTLS := port == 587
	if v, ok := cfg["smtp_use_tls"].(bool); ok {
		useTLS = v
	}

	from := user
	if strings.Contains(user, "<") { // 允许 "Name <addr>" 形式原样作为 From 头
		from = user
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		from, to, "WorkBuddy2API 通知", message)
	raw := []byte(b.String())

	addr := fmt.Sprintf("%s:%d", server, port)
	auth := smtp.PlainAuth("", user, pass, server)

	if port == 465 {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: server})
		if err != nil {
			return fmt.Errorf("邮件 SSL 连接失败: %w", err)
		}
		c, err := smtp.NewClient(conn, server)
		if err != nil {
			return fmt.Errorf("邮件 SMTP 初始化失败: %w", err)
		}
		defer c.Close()
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("邮件认证失败（检查授权码而非登录密码）: %w", err)
		}
		return smtpSend(c, user, to, raw)
	}

	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("邮件服务器连接失败 %s: %w", addr, err)
	}
	defer c.Close()
	if useTLS {
		if err := c.StartTLS(&tls.Config{ServerName: server}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("邮件认证失败（检查授权码而非登录密码）: %w", err)
	}
	return smtpSend(c, user, to, raw)
}

func smtpSend(c *smtp.Client, from, to string, raw []byte) error {
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func sendWebhook(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	webhook := str(cfg, "webhook_url")
	if webhook == "" {
		return fmt.Errorf("Webhook 配置为空")
	}
	method := strings.ToUpper(strOr(cfg, "http_method", "POST"))
	if method != "POST" && method != "PUT" {
		method = "POST"
	}
	headers := map[string]string{}
	if hs := str(cfg, "headers"); hs != "" {
		var custom map[string]string
		if json.Unmarshal([]byte(hs), &custom) == nil {
			for k, v := range custom {
				headers[k] = v
			}
		}
	}
	payload := map[string]any{
		"message":   message,
		"timestamp": time.Now().Format("2006-01-02 15:04:05"),
		"source":    "workbuddy2api-panel",
	}
	status, _, err := postJSON(ctx, httpc, method, webhook, headers, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("Webhook 通知发送失败: HTTP %d", status)
	}
	return nil
}

func sendWeChat(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	webhook := str(cfg, "webhook_url")
	if webhook == "" {
		return fmt.Errorf("企业微信通知配置为空")
	}
	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": message},
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost, webhook, nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("企业微信通知发送失败: HTTP %d", status)
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("企业微信通知发送失败: %s", r.ErrMsg)
	}
	return nil
}

func sendTelegram(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	token := str(cfg, "bot_token")
	chatID := str(cfg, "chat_id")
	if token == "" || chatID == "" {
		return fmt.Errorf("Telegram 通知配置不完整（bot_token/chat_id）")
	}
	base := strOr(cfg, "base_url", "https://api.telegram.org")
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost,
		fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(base, "/"), token), nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("Telegram 通知发送失败: HTTP %d", status)
	}
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &r) == nil && !r.OK {
		return fmt.Errorf("Telegram 通知发送失败: %s", r.Description)
	}
	return nil
}

func sendPushPlus(ctx context.Context, httpc *http.Client, cfg map[string]any, message string) error {
	token := str(cfg, "token")
	if token == "" {
		return fmt.Errorf("PushPlus token 为空")
	}
	server := strings.TrimRight(strOr(cfg, "server_url", "https://www.pushplus.plus"), "/")
	payload := map[string]any{
		"token":    token,
		"title":    strOr(cfg, "title", "WorkBuddy2API 通知"),
		"content":  message,
		"template": strOr(cfg, "template", "txt"),
	}
	if topic := str(cfg, "topic"); topic != "" {
		payload["topic"] = topic
	}
	status, raw, err := postJSON(ctx, httpc, http.MethodPost, server+"/send", nil, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("PushPlus 通知发送失败: HTTP %d", status)
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Code != 200 {
		return fmt.Errorf("PushPlus 通知发送失败: %s", r.Msg)
	}
	return nil
}
