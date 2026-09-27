//go:build windows && tray

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

const instanceMutexName = `Local\WorkBuddy2API-Panel-Singleton`

type singleInstance struct {
	handle windows.Handle
}

func acquireSingleInstance() (*singleInstance, bool, error) {
	name, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create instance mutex: %w", err)
	}
	return &singleInstance{handle: handle}, false, nil
}

func (s *singleInstance) Close() error {
	if s == nil || s.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(s.handle)
	s.handle = 0
	return err
}

func openPanelInBrowser(rawURL string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(rawURL)
	if err != nil {
		return err
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("open %s: %w", rawURL, err)
	}
	return nil
}

func showFatalError(title, message string) {
	caption, errCaption := windows.UTF16PtrFromString(title)
	text, errText := windows.UTF16PtrFromString(message)
	if errCaption != nil || errText != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR)
}
