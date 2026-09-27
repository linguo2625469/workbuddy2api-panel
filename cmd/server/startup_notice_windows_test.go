//go:build windows && tray

package main

import (
	"os"
	"path/filepath"
	"testing"
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
