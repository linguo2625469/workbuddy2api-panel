//go:build windows && tray

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestStartupNoticeMarkerPath(t *testing.T) {
	path, err := startupNoticeMarkerPath()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Dir(exe) || filepath.Base(path) != startupNoticeFileName {
		t.Fatalf("marker path = %q, exe dir = %q", path, filepath.Dir(exe))
	}
}

func TestStartupNoticeMarkerLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), startupNoticeFileName)
	if startupNoticeSuppressed(path) {
		t.Fatal("marker unexpectedly exists")
	}
	if err := persistStartupNoticeChoice(path, false); err != nil {
		t.Fatal(err)
	}
	if startupNoticeSuppressed(path) {
		t.Fatal("unchecked choice created marker")
	}
	if err := persistStartupNoticeChoice(path, true); err != nil {
		t.Fatal(err)
	}
	if !startupNoticeSuppressed(path) {
		t.Fatal("checked choice did not create marker")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if startupNoticeSuppressed(path) {
		t.Fatal("removing marker did not restore notice")
	}
}

func TestPersistStartupNoticeChoiceFailure(t *testing.T) {
	if err := persistStartupNoticeChoice(t.TempDir(), true); err == nil {
		t.Fatal("writing marker over a directory succeeded")
	}
}

func TestTaskDialogPackedLayout(t *testing.T) {
	config, button := buildStartupTaskDialog(nil, nil, nil, nil, nil)
	ptrSize := int(unsafe.Sizeof(uintptr(0)))
	wantConfigSize := 32 + 16*ptrSize
	wantButtonSize := 4 + ptrSize
	if len(config.data) != wantConfigSize {
		t.Fatalf("config size = %d, want %d", len(config.data), wantConfigSize)
	}
	if len(button.data) != wantButtonSize {
		t.Fatalf("button size = %d, want %d", len(button.data), wantButtonSize)
	}
	if got := binary.LittleEndian.Uint32(config.data[:4]); got != uint32(wantConfigSize) {
		t.Fatalf("cbSize = %d, want %d", got, wantConfigSize)
	}
}
