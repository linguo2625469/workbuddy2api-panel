//go:build !windows

package main

import (
	"log"
	"net/http"
)

// serveApplication 非 Windows 平台直接监听（无桌面窗口概念）。
func serveApplication(srv *http.Server) error {
	ln, err := listenFor(srv)
	if err != nil {
		return err
	}
	return srv.Serve(ln)
}

// showErrorDialog 非 Windows 平台无原生对话框，仅记录日志。
func showErrorDialog(msg string) { log.Printf("%s", msg) }

// setPanelURL 非 Windows 平台无窗口文案，空实现。
func setPanelURL(string) {}
