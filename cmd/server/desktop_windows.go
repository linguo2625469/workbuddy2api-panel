//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Windows 桌面外壳：给双击启动的用户一个可见窗口，关掉窗口即停服务。
//
// 设计取舍：
//   - 只做「窗口生命周期」一件事，不画按钮、不接管业务；面板仍在浏览器里。
//   - 重复双击由 listenFor 的端口占用兜住（见 listen.go）：绑不上端口 = 已有实例，
//     提示后退出，不会留下第二个进程。
//   - 窗口文案在 WM_PAINT 里绘制，内容即「面板地址 + 关闭即退出」，不依赖外部资源。
var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procGetModuleW     = kernel32.NewProc("GetModuleHandleW")
	procMessageBoxW    = user32.NewProc("MessageBoxW")
	procRegisterClassW = user32.NewProc("RegisterClassW")
	procCreateWindowW  = user32.NewProc("CreateWindowExW")
	procShowWindow     = user32.NewProc("ShowWindow")
	procUpdateWindow   = user32.NewProc("UpdateWindow")
	procGetMessageW    = user32.NewProc("GetMessageW")
	procTranslateMsg   = user32.NewProc("TranslateMessage")
	procDispatchMsg    = user32.NewProc("DispatchMessageW")
	procDefWindowProcW = user32.NewProc("DefWindowProcW")
	procDestroyWindow  = user32.NewProc("DestroyWindow")
	procPostQuitMsg    = user32.NewProc("PostQuitMessage")
	procLoadCursorW    = user32.NewProc("LoadCursorW")
	procFindWindowW    = user32.NewProc("FindWindowW")
	procSetForegroundW = user32.NewProc("SetForegroundWindow")
	procBeginPaint     = user32.NewProc("BeginPaint")
	procEndPaint       = user32.NewProc("EndPaint")
	procDrawTextW      = user32.NewProc("DrawTextW")
	procGetClientRect  = user32.NewProc("GetClientRect")
	procSetBkMode      = gdi32.NewProc("SetBkMode")
	procSetTextColor   = gdi32.NewProc("SetTextColor")
)

const (
	wmDestroy    = 0x0002
	wmPaint      = 0x000F
	wmClose      = 0x0010
	csHRedraw    = 0x0002
	csVRedraw    = 0x0001
	wsOverlapped = 0x00CF0000
	wsVisible    = 0x10000000
	cwUseDefault = 0x80000000
	swShow       = 5
	swRestore    = 9
	mbOK         = 0x00000000
	mbIconError  = 0x00000010
	colorWindow  = 5
	idcArrow     = 32512

	transparent = 1
	dtLeft      = 0x00000000
	dtNoPrefix  = 0x00000800
	dtWordBreak = 0x00000010
)

type winRect struct{ Left, Top, Right, Bottom int32 }

type winPaintStruct struct {
	HDC         uintptr
	Erase       int32
	RcPaint     winRect
	Restore     int32
	IncUpdate   int32
	RgbReserved [32]byte
}

type winMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type winWndClass struct {
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
}

// panelURLText 窗口里展示的面板地址（启动时由 main 按实际 listen 地址填入）。
var panelURLText = "http://127.0.0.1:7863/panel/"

// setPanelURL 由 main 注入实际面板地址（listen 被配置改过时窗口文案同步更新）。
func setPanelURL(u string) {
	if u != "" {
		panelURLText = u
	}
}

// apiBaseURL 从面板地址推出 OpenAI 兼容接口根地址（供窗口文案展示）。
func apiBaseURL() string {
	return strings.TrimSuffix(panelURLText, "/panel/")
}

// showErrorDialog 弹原生错误框（windowsgui 构建没有控制台，出错必须可见）。
func showErrorDialog(msg string) {
	t, _ := syscall.UTF16PtrFromString("WorkBuddy2API")
	m, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbOK|mbIconError)
}

// serveApplication 绑定端口 → 建窗口 → 起服务 → 跑消息循环；窗口关闭即优雅停机。
func serveApplication(srv *http.Server) error {
	// CreateWindowEx/GetMessage 必须在同一个 OS 线程上：goroutine 会被调度到不同线程，
	// 不锁定线程会让消息循环收不到消息（表现为窗口无响应、关不掉）。
	runtime.LockOSThread()

	// 先绑端口：既是启动前置条件，也是「重复双击」的天然闸门。
	ln, err := listenFor(srv)
	if err != nil {
		// 已有实例：把它激活到前台，本次启动静默退出——比弹框更符合直觉，
		// 也不会留下一个等用户点「确定」的僵尸进程。
		if errors.Is(err, errAlreadyRunning) && focusExistingWindow() {
			return errAlreadyRunning
		}
		return err
	}

	if err := createMainWindow(); err != nil {
		ln.Close()
		return err
	}

	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Serve(ln) }()

	// 消息循环：窗口存活期间阻塞；WM_DESTROY 后 PostQuitMessage 使 GetMessage 返回 0。
	for {
		var msg winMsg
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
	}

	// 关闭窗口 = 停止服务：优雅停机，最多等 5s。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(ctx)
	cancel()

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case <-time.After(time.Second):
	}
	return http.ErrServerClosed
}

// wndProcCallback 持有 NewCallback 返回值，避免回调被回收（GC 后回跳会崩进程）。
var wndProcCallback uintptr

// createMainWindow 注册窗口类并创建主窗口。
func createMainWindow() error {
	className, _ := syscall.UTF16PtrFromString("WorkBuddy2APIWindowClass")
	// 标题带版本号：用户/客服一眼就能确认跑的是哪一版，省掉「你是不是没换新版」的扯皮。
	title, _ := syscall.UTF16PtrFromString("WorkBuddy2API " + appVersion + " - 本地服务")
	instance, _, _ := procGetModuleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	wndProcCallback = syscall.NewCallback(desktopWndProc)
	wc := winWndClass{
		Style:      csHRedraw | csVRedraw,
		WndProc:    wndProcCallback,
		Instance:   instance,
		Cursor:     cursor,
		Background: colorWindow + 1,
		ClassName:  className,
	}
	// 注册失败不单独报错：类已存在（第二次创建窗口）也会返回 0，
	// 真正的失败会在 CreateWindowExW 上暴露。
	procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, e := procCreateWindowW.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		wsOverlapped|wsVisible, cwUseDefault, cwUseDefault, 760, 480,
		0, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("创建窗口失败：%v", e)
	}
	procShowWindow.Call(hwnd, swShow)
	procUpdateWindow.Call(hwnd)
	return nil
}

// focusExistingWindow 找到已经运行的主窗口并激活到前台。
// 用窗口类名查找（与标题里的版本号无关，跨版本也能找到）。
func focusExistingWindow() bool {
	cls, _ := syscall.UTF16PtrFromString("WorkBuddy2APIWindowClass")
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	if h == 0 {
		return false
	}
	procShowWindow.Call(h, swRestore)
	procSetForegroundW.Call(h)
	return true
}

// desktopWndProc 只处理「重绘」与「关闭」两件事。
func desktopWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmPaint:
		paintWindow(hwnd)
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMsg.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

// paintWindow 在窗口里画使用说明：面板地址 + 关闭即退出。
func paintWindow(hwnd uintptr) {
	var ps winPaintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	procSetBkMode.Call(hdc, transparent)
	procSetTextColor.Call(hdc, 0x00202020)

	body := "WorkBuddy2API " + appVersion + " 已启动\r\n\r\n" +
		"管理面板：" + panelURLText + "\r\n" +
		"接口地址：" + apiBaseURL() + "/v1\r\n\r\n" +
		"请用浏览器打开上面的管理面板，添加账号后即可使用。\r\n" +
		"关闭本窗口即停止服务（数据保存在程序所在目录）。\r\n\r\n" +
		"运行日志：runtime.log（与本程序同目录）"
	text, err := syscall.UTF16FromString(body)
	if err != nil {
		return
	}
	rc.Left += 24
	rc.Top += 24
	rc.Right -= 24
	rc.Bottom -= 24
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)-1),
		uintptr(unsafe.Pointer(&rc)), uintptr(dtLeft|dtNoPrefix|dtWordBreak))
}
