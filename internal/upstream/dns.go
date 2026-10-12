package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// dnsDialTimeout 单台 DNS 服务器的建连上限（多台依次尝试）。
const dnsDialTimeout = 5 * time.Second

// NewDNSDialer 构造使用指定 DNS 服务器的出站拨号器（空 = 系统解析器）。
// Android 没有 /etc/resolv.conf：Go 解析器会退回 [::1]:53 导致所有域名解析失败，
// 传真实 DNS（223.5.5.5 / 119.29.29.29）即绕开。
func NewDNSDialer(servers []string) *net.Dialer {
	d := newDialer()
	if len(servers) > 0 {
		d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dd := net.Dialer{Timeout: dnsDialTimeout}
			var lastErr error
			for _, s := range servers {
				addr := s
				if _, _, err := net.SplitHostPort(addr); err != nil {
					addr = net.JoinHostPort(s, "53")
				}
				c, err := dd.DialContext(ctx, network, addr)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("dns: 未配置服务器")
			}
			return nil, lastErr
		}}
	}
	return d
}

// SetDNSDialer 把拨号器换到已有的两个 http.Client 上（只覆盖 Transport.DialContext）。
func (c *Client) SetDNSDialer(d *net.Dialer) {
	if d == nil {
		return
	}
	for _, cl := range []*http.Client{c.HTTP, c.ChatHTTP} {
		if cl == nil {
			continue
		}
		if t, ok := cl.Transport.(*http.Transport); ok {
			t.DialContext = d.DialContext
		}
	}
}
