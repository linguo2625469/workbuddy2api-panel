//go:build windows && tray

package main

//go:generate windres -i wb2api.rc -o wb2api_windows_amd64.syso -O coff

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	startupNoticeFileName   = ".startup-notice-disabled"
	startupNoticeButtonID   = 1001
	taskDialogSizeToContent = 0x01000000
)

var taskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")

type packedTaskDialog struct {
	ptrSize int
	data    []byte
}

func newPackedTaskDialog() *packedTaskDialog {
	return &packedTaskDialog{ptrSize: int(unsafe.Sizeof(uintptr(0)))}
}

func (p *packedTaskDialog) uint32(v uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], v)
	p.data = append(p.data, raw[:]...)
}

func (p *packedTaskDialog) int32(v int32) {
	p.uint32(uint32(v))
}

func (p *packedTaskDialog) pointer(v uintptr) {
	if p.ptrSize == 8 {
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], uint64(v))
		p.data = append(p.data, raw[:]...)
		return
	}
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], uint32(v))
	p.data = append(p.data, raw[:]...)
}

func utf16Pointer(value string) (*uint16, error) {
	ptr, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return nil, err
	}
	return ptr, nil
}

func startupNoticeMarkerPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), startupNoticeFileName), nil
}

func startupNoticeSuppressed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func persistStartupNoticeChoice(path string, suppress bool) error {
	if !suppress {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func showStartupNotice() {
	marker, err := startupNoticeMarkerPath()
	if err != nil {
		log.Printf("启动提示: 无法确定标记文件路径: %v", err)
		return
	}
	if startupNoticeSuppressed(marker) {
		return
	}

	suppress, err := showStartupTaskDialog()
	if err != nil {
		log.Printf("启动提示: TaskDialog 不可用，降级为 MessageBox: %v", err)
		showStartupNoticeMessageBox()
		return
	}
	if !suppress {
		return
	}
	if err := persistStartupNoticeChoice(marker, true); err != nil {
		log.Printf("启动提示: 保存“不再显示”设置失败: %v", err)
		return
	}
	log.Printf("启动提示: 已创建 %s，后续启动不再显示", marker)
}

func showStartupTaskDialog() (bool, error) {
	if err := taskDialogIndirect.Find(); err != nil {
		return false, err
	}
	title, err := utf16Pointer("WorkBuddy2API 已启动")
	if err != nil {
		return false, err
	}
	instruction, err := utf16Pointer("程序已成功运行")
	if err != nil {
		return false, err
	}
	content, err := utf16Pointer("WorkBuddy2API 已在系统托盘中运行。\n\n右键点击托盘图标，选择“打开Panel”即可进入管理界面。")
	if err != nil {
		return false, err
	}
	verification, err := utf16Pointer("以后不再显示此提示")
	if err != nil {
		return false, err
	}
	buttonText, err := utf16Pointer("确定")
	if err != nil {
		return false, err
	}

	config, button := buildStartupTaskDialog(title, instruction, content, verification, buttonText)
	var selectedButton int32
	var checked int32
	ret, _, _ := taskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&config.data[0])),
		uintptr(unsafe.Pointer(&selectedButton)),
		0,
		uintptr(unsafe.Pointer(&checked)),
	)
	runtime.KeepAlive(config)
	runtime.KeepAlive(button)
	runtime.KeepAlive(title)
	runtime.KeepAlive(instruction)
	runtime.KeepAlive(content)
	runtime.KeepAlive(verification)
	runtime.KeepAlive(buttonText)
	if ret != 0 {
		return false, fmt.Errorf("TaskDialogIndirect HRESULT=0x%08X", uint32(ret))
	}
	return selectedButton == startupNoticeButtonID && checked != 0, nil
}

func buildStartupTaskDialog(
	title, instruction, content, verification, buttonText *uint16,
) (*packedTaskDialog, *packedTaskDialog) {
	button := newPackedTaskDialog()
	button.int32(startupNoticeButtonID)
	button.pointer(uintptr(unsafe.Pointer(buttonText)))

	config := newPackedTaskDialog()
	config.uint32(0) // cbSize，最后回填
	config.pointer(0)
	config.pointer(0)
	config.uint32(taskDialogSizeToContent)
	config.uint32(0)
	config.pointer(uintptr(unsafe.Pointer(title)))
	config.pointer(0)
	config.pointer(uintptr(unsafe.Pointer(instruction)))
	config.pointer(uintptr(unsafe.Pointer(content)))
	config.uint32(1)
	config.pointer(uintptr(unsafe.Pointer(&button.data[0])))
	config.int32(startupNoticeButtonID)
	config.uint32(0)
	config.pointer(0)
	config.int32(0)
	config.pointer(uintptr(unsafe.Pointer(verification)))
	config.pointer(0)
	config.pointer(0)
	config.pointer(0)
	config.pointer(0)
	config.pointer(0)
	config.pointer(0)
	config.pointer(0)
	config.uint32(0)
	binary.LittleEndian.PutUint32(config.data[:4], uint32(len(config.data)))
	return config, button
}

func showStartupNoticeMessageBox() {
	text, _ := windows.UTF16PtrFromString("WorkBuddy2API 已在系统托盘中运行。\n\n右键点击托盘图标，选择“打开Panel”即可进入管理界面。")
	title, _ := windows.UTF16PtrFromString("WorkBuddy2API 已启动")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONINFORMATION)
}
