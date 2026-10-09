package upstream

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// fakeDNS 起一个只回 A=127.0.0.9 的极简 DNS 服务器，返回地址与"收到查询"的信号。
func fakeDNS(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("UDP 不可用：%v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	got := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case got <- struct{}{}:
			default:
			}
			if resp := dnsReply(buf[:n]); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}
	}()
	return pc.LocalAddr().String(), got
}

// dnsReply 把查询原样改成一条 A=127.0.0.9 的响应（够测试用，不解析问题段）。
func dnsReply(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	// 只回带问题段：查询里可能还有 EDNS0 之类的附加记录，整段照抄会被
	// 当成第一条 answer（type=41）解析，客户端就只看到"没有 A 记录"。
	i := 12
	for i < len(q) {
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 == 0xC0 { // 压缩指针（查询里一般不会出现）
			i += 2
			break
		}
		i += 1 + l
	}
	i += 4 // qtype + qclass
	if i > len(q) {
		return nil
	}
	out := make([]byte, 12, 64)
	copy(out, q[:2])                        // 同一个事务 ID
	out[2], out[3] = 0x81, 0x80             // 标准响应 + 递归可用 + 无错误
	binary.BigEndian.PutUint16(out[4:6], 1) // QDCOUNT
	binary.BigEndian.PutUint16(out[6:8], 1) // ANCOUNT
	out = append(out, q[12:i]...)           // 问题段
	out = append(out, 0xC0, 0x0C)           // 名字指针指向问题段
	out = append(out, 0x00, 0x01, 0x00, 0x01)
	out = append(out, 0x00, 0x00, 0x00, 0x3C)
	out = append(out, 0x00, 0x04, 127, 0, 0, 9)
	return out
}

// TestNewDNSDialerUsesConfiguredServer 配了 DNS 就必须查它，而不是系统解析器。
func TestNewDNSDialerUsesConfiguredServer(t *testing.T) {
	addr, got := fakeDNS(t)
	d := NewDNSDialer([]string{addr})
	if d.Resolver == nil {
		t.Fatal("配了 DNS 却没换 Resolver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := d.Resolver.LookupHost(ctx, "example.com")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(ips) != 1 || ips[0] != "127.0.0.9" {
		t.Fatalf("解析结果=%v，期望 [127.0.0.9]（说明没走配置的 DNS）", ips)
	}
	select {
	case <-got:
	default:
		t.Fatal("配置的 DNS 服务器没收到查询")
	}
}

// TestNewDNSDialerEmptyUsesSystemResolver 不配 DNS 时保持系统解析器。
func TestNewDNSDialerEmptyUsesSystemResolver(t *testing.T) {
	for _, servers := range [][]string{nil, {}} {
		if d := NewDNSDialer(servers); d.Resolver != nil {
			t.Fatalf("servers=%v 时换了 Resolver，应该维持系统解析器", servers)
		}
	}
}

// TestSetDNSDialerPatchesBothClients 两个 http.Client 都要换拨号器，且不动超时等既有参数。
func TestSetDNSDialerPatchesBothClients(t *testing.T) {
	httpTr, chatTr := &http.Transport{}, &http.Transport{}
	c := &Client{
		HTTP:     &http.Client{Timeout: 5 * time.Second, Transport: httpTr},
		ChatHTTP: &http.Client{Transport: chatTr},
	}
	d := &net.Dialer{}
	c.SetDNSDialer(d)
	want := reflect.ValueOf(d.DialContext).Pointer()
	for name, tr := range map[string]*http.Transport{"HTTP": httpTr, "ChatHTTP": chatTr} {
		if tr.DialContext == nil {
			t.Fatalf("%s 的 DialContext 没换", name)
		}
		if got := reflect.ValueOf(tr.DialContext).Pointer(); got != want {
			t.Fatalf("%s 的 DialContext 不是传入的拨号器", name)
		}
	}
	if c.HTTP.Timeout != 5*time.Second {
		t.Fatal("既有 Timeout 被改掉了")
	}
}

// TestSetDNSDialerNilIsNoop 传 nil 不动任何东西。
func TestSetDNSDialerNilIsNoop(t *testing.T) {
	tr := &http.Transport{}
	c := &Client{HTTP: &http.Client{Transport: tr}}
	c.SetDNSDialer(nil)
	if tr.DialContext != nil {
		t.Fatal("传 nil 却换了 DialContext")
	}
}
