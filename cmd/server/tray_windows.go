//go:build windows

package main

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"sync/atomic"

	"github.com/energye/systray"
)

//go:embed assets/tray.ico
var trayIcon []byte

func runDesktop(ctx context.Context, onOpen func()) error {
	var ready atomic.Bool
	readyCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-readyCh:
				systray.Quit()
			case <-doneCh:
			}
		case <-doneCh:
		}
	}()

	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("WorkBuddy2API")

		openItem := systray.AddMenuItem("打开Panel", "在默认浏览器中打开管理面板")
		openItem.Click(func() {
			if onOpen != nil {
				onOpen()
			}
		})
		quitItem := systray.AddMenuItem("退出程序", "停止网关并退出")
		quitItem.Click(func() {
			systray.Quit()
		})

		ready.Store(true)
		close(readyCh)
	}, func() {})

	close(doneCh)
	if !ready.Load() {
		return errors.New("system tray initialization failed")
	}
	log.Printf("system tray ready")
	return nil
}
