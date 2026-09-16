// listen.go 启动期监听绑定：把「端口被占用」翻译成用户能看懂的一句话。
//
// 为什么用它做单实例闸门：端口是唯一真正排他的资源。同一个目录重复双击 =
// 同一份 config = 同一个端口，第二次必然绑不上 → 直接给出「已经在运行」提示后退出，
// 不会产生第二个进程，也不会因为按错而误伤「不同端口跑多套实例」的用法。
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// errAlreadyRunning 端口被同类实例占用的哨兵错误。调用方可据此走「静默退出」路径
// （Windows 下激活已有窗口），而不是弹错误框 —— 重复双击是正常误操作，不是故障。
var errAlreadyRunning = errors.New("already running")

// listenFor 绑定 srv.Addr；端口已被占用时返回 errAlreadyRunning 包装的用户可读说明。
func listenFor(srv *http.Server) (net.Listener, error) {
	ln, err := net.Listen("tcp", srv.Addr)
	if err == nil {
		log.Printf("workbuddy2api listening on %s", srv.Addr)
		return ln, nil
	}
	if isAddrInUse(err) {
		return nil, fmt.Errorf("%w：端口 %s 已被占用", errAlreadyRunning, srv.Addr)
	}
	return nil, fmt.Errorf("无法监听端口 %s：%w", srv.Addr, err)
}

// isAddrInUse 判定「端口已被占用」。
//
// Windows 上必须额外比 10048：net 包把 WSAEADDRINUSE(10048) 原样抛出，与
// syscall.EADDRINUSE（Go 在 Windows 上映射的 POSIX 码）并不相等，只比后者会漏判，
// 表现为重复双击时报一串英文 bind 错误而不是「已在运行」。
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	const wsaeAddrInUse = 10048
	if errors.Is(err, syscall.Errno(wsaeAddrInUse)) {
		return true
	}
	// 兜底：跨平台文案判断（某些包装层会丢掉 Errno 链）。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}
