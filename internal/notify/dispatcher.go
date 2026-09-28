// dispatcher.go 渠道集分发：按事件过滤启用渠道并发推送，单渠道失败不阻塞其它渠道。
//
// 防抖：同一 (事件, 账号) 在 dedupeWindow 内重复触发只推一次——签到失败/熔断在
// 重试密集时会连发，防抖避免通知渠道被刷爆。测试可通过 NewNotifier 注入窗口。
package notify

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// 事件类型常量（Channel.Events 订阅用；空 Events = 订阅全部）。
const (
	EventCheckinFailed = "checkin_failed" // 签到失败
	EventBreaker       = "breaker"        // 连续错误触发熔断
	EventDisabled      = "disabled"       // 账号被禁用
	EventCreditReport  = "credit_report"  // 积分消耗报告（请求成功扣费后按账号窗口聚合推送）
)

// Notifier 持有渠道快照与发送客户端，线程安全。
type Notifier struct {
	httpc *http.Client

	mu      sync.RWMutex
	chans   []Channel
	dedupe  map[string]time.Time
	window  time.Duration

	// creditWin 消耗聚合窗口（ReportCredit 用）；默认 creditWindow，测试可注入短值。
	creditWin time.Duration
	creditMu  sync.Mutex
	creditAgg map[string]*creditAgg
}

func NewNotifier(httpc *http.Client, dedupeWindow time.Duration) *Notifier {
	if dedupeWindow <= 0 {
		dedupeWindow = 30 * time.Minute
	}
	return &Notifier{
		httpc: httpc, dedupe: map[string]time.Time{}, window: dedupeWindow,
		creditWin: creditWindow, creditAgg: map[string]*creditAgg{},
	}
}

// SetChannels 整体替换渠道快照（配置热生效时调用）。
func (n *Notifier) SetChannels(chans []Channel) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.chans = append([]Channel(nil), chans...)
}

// Channels 返回当前渠道快照（面板展示用）。
func (n *Notifier) Channels() []Channel {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return append([]Channel(nil), n.chans...)
}

// subscribed 判断渠道是否订阅了事件（空 Events = 全部订阅）。
func (c Channel) subscribed(event string) bool {
	if len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Notify 向订阅该事件的启用渠道并发推送。key 为防抖键（通常账号 uid，全局事件传 ""）。
// ctx 为模板渲染变量（nickname/credits/consume 等），nil 时模板变量渲染为"未知"。
// 发送异步进行：调用方（scheduler/pool 热路径）不被渠道网络时延阻塞。
func (n *Notifier) Notify(event, key, message string, ctx map[string]string) {
	n.mu.Lock()
	dk := event + "|" + key
	if last, ok := n.dedupe[dk]; ok && time.Since(last) < n.window {
		n.mu.Unlock()
		return
	}
	n.dedupe[dk] = time.Now()
	// 顺带清理过期条目，防止 map 无界增长（账号数有限，但全局 key 可能变化）。
	for k, t := range n.dedupe {
		if time.Since(t) > n.window {
			delete(n.dedupe, k)
		}
	}
	var targets []Channel
	for _, c := range n.chans {
		if c.Enabled && c.subscribed(event) {
			targets = append(targets, c)
		}
	}
	n.mu.Unlock()

	if len(targets) == 0 {
		return
	}
	go func() {
		for _, c := range targets {
			// 每个渠道独立渲染模板（template 存在各自 config 里）。
			msg := RenderTemplate(c.Config, ctx, message)
			ctxT, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := Send(ctxT, c.Type, c.Config, msg, n.httpc); err != nil {
				log.Printf("notify: 渠道 %q(%s) 推送失败: %v", c.Name, c.Type, err)
			} else {
				log.Printf("notify: 渠道 %q(%s) 推送成功: %s", c.Name, c.Type, event)
			}
			cancel()
		}
	}()
}

// creditAgg 单账号的消耗聚合窗口。
type creditAgg struct {
	consume float64     // 窗口内累计消耗
	remain  int64       // 最近一次记账后的剩余积分
	total   int64       // 积分总额
	nick    string      // 最近一次昵称
	timer   *time.Timer // 窗口到期推送定时器
}

// creditWindow 消耗聚合窗口：同账号窗口内多次消耗汇总成一条推送。
const creditWindow = 5 * time.Minute

// ReportCredit 记录一次消耗（uid 记账后的余额快照 + 本次消耗）；窗口到期后
// 汇总推送一条积分报告。nickname 展示用。
//
// 设计：「每次使用完就推送」在高频调用下会刷屏，故按账号 5 分钟窗口聚合——
// 窗口内第一次消耗开启窗口，后续消耗只累加，到期推送 {consume}=窗口总消耗。
func (n *Notifier) ReportCredit(uid, nickname string, consume float64, remain, total int64) {
	if consume <= 0 {
		return
	}
	n.creditMu.Lock()
	a := n.creditAgg[uid]
	if a == nil {
		a = &creditAgg{}
		n.creditAgg[uid] = a
	}
	a.consume += consume
	a.remain = remain
	a.total = total
	a.nick = nickname
	if a.timer == nil {
		a.timer = time.AfterFunc(n.creditWin, func() {
			n.creditMu.Lock()
			agg := n.creditAgg[uid]
			delete(n.creditAgg, uid)
			n.creditMu.Unlock()
			if agg == nil {
				return
			}
			n.Notify(EventCreditReport, uid, defaultCreditMsg(agg.nick, agg.consume, agg.remain, agg.total), map[string]string{
				"nickname":      agg.nick,
				"uid":           uid,
				"event":         EventCreditReport,
				"consume":       formatConsume(agg.consume),
				"credits":       formatInt(agg.remain),
				"credits_total": formatInt(agg.total),
				"time":          time.Now().Format("2006-01-02 15:04:05"),
			})
		})
	}
	n.creditMu.Unlock()
}

func defaultCreditMsg(nickname string, consume float64, remain, total int64) string {
	name := nickname
	if name == "" {
		name = "账号"
	}
	s := "💰 积分消耗报告\n账号: " + name +
		"\n窗口消耗: " + formatConsume(consume) +
		"\n剩余积分: " + formatInt(remain)
	if total > 0 {
		s += " / " + formatInt(total)
	}
	return s + "\n时间: " + time.Now().Format("2006-01-02 15:04:05")
}

func formatConsume(c float64) string {
	// 消耗为积分计数，通常整数；保留一位小数覆盖 fractional 观测。
	if c == float64(int64(c)) {
		return formatInt(int64(c))
	}
	return strconv.FormatFloat(c, 'f', 1, 64)
}

func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

// Test 向单个渠道发送测试消息（同步，面板「发送测试」按钮用）。
func (n *Notifier) Test(ctx context.Context, c Channel) error {
	msg := "🔔 通知渠道测试\n\n渠道名称: " + c.Name + "\n渠道类型: " + c.Type +
		"\n测试时间: " + time.Now().Format("2006-01-02 15:04:05") +
		"\n\n如果您收到此消息，说明通知渠道配置正确！"
	return Send(ctx, c.Type, c.Config, msg, n.httpc)
}
