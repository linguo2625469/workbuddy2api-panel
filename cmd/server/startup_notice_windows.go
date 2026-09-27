//go:build windows && tray

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	startupNoticeFileName   = ".startup-notice-disabled"
	startupNoticeButtonID   = 1001
	taskDialogSizeToContent = 0x01000000
)

var taskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")

type taskDialogButton struct {
	ID   int32
	Text *uint16
}

// taskDialogConfig mirrors TASKDIALOGCONFIG. Pointer-sized union fields are
// kept as uintptr so the layout is correct on both 32-bit and 64-bit Windows.
type taskDialogConfig struct {
	Size                 uint32
	Parent               windows.HWND
	Instance             windows.Handle
	Flags                uint32
	CommonButtons        uint32
	WindowTitle          *uint16
	MainIcon             uintptr
	MainInstruction      *uint16
	Content              *uint16
	ButtonCount          uint32
	Buttons              *taskDialogButton
	DefaultButton        int32
	RadioButtonCount     uint32
	RadioButtons         *taskDialogButton
	DefaultRadioButton   int32
	VerificationText     *uint16
	ExpandedInformation  *uint16
	ExpandedControlText  *uint16
	CollapsedControlText *uint16
	FooterIcon           uintptr
	Footer               *uint16
	Callback             uintptr
	CallbackData         uintptr
	Width                uint32
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
	}
}

func showStartupTaskDialog() (bool, error) {
	if err := taskDialogIndirect.Find(); err != nil {
		return false, err
	}
	title, _ := windows.UTF16PtrFromString("WorkBuddy2API 已启动")
	instruction, _ := windows.UTF16PtrFromString("程序已成功运行")
	content, _ := windows.UTF16PtrFromString("WorkBuddy2API 已在系统托盘中运行。\n\n右键点击托盘图标，选择“打开Panel”即可进入管理界面。")
	verification, _ := windows.UTF16PtrFromString("以后不再显示此提示")
	buttonText, _ := windows.UTF16PtrFromString("确定")
	buttons := []taskDialogButton{{ID: startupNoticeButtonID, Text: buttonText}}
	config := taskDialogConfig{
		Parent:           0,
		Flags:            taskDialogSizeToContent,
		WindowTitle:      title,
		MainInstruction:  instruction,
		Content:          content,
		ButtonCount:      1,
		Buttons:          &buttons[0],
		DefaultButton:    startupNoticeButtonID,
		VerificationText: verification,
	}
	config.Size = uint32(unsafe.Sizeof(config))

	var selectedButton int32
	var checked int32
	ret, _, _ := taskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&config)),
		uintptr(unsafe.Pointer(&selectedButton)),
		0,
		uintptr(unsafe.Pointer(&checked)),
	)
	if ret != 0 {
		return false, fmt.Errorf("TaskDialogIndirect HRESULT=0x%08X", uint32(ret))
	}
	return selectedButton == startupNoticeButtonID && checked != 0, nil
}

func showStartupNoticeMessageBox() {
	text, _ := windows.UTF16PtrFromString("WorkBuddy2API 已在系统托盘中运行。\n\n右键点击托盘图标，选择“打开Panel”即可进入管理界面。")
	title, _ := windows.UTF16PtrFromString("WorkBuddy2API 已启动")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONINFORMATION)
}
