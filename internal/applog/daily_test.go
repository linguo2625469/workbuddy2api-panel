package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyWriterCreatesNamesAndRotates(t *testing.T) {
	base := t.TempDir()
	day := time.Date(2026, 9, 27, 23, 59, 0, 0, time.Local)
	w, err := newDaily(base, func() time.Time { return day })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(base, "logs", "260927.log")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("first log missing: %v", err)
	}

	day = day.Add(24 * time.Hour)
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(base, "logs", "260928.log")
	secondRaw, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("second log missing: %v", err)
	}
	if got := string(secondRaw); got != "second\n" {
		t.Fatalf("second log = %q, want %q", got, "second\n")
	}

	firstRaw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(firstRaw); got != "first\n" {
		t.Fatalf("first log = %q, want %q", got, "first\n")
	}
}

func TestDailyWriterAppendsAcrossInstances(t *testing.T) {
	base := t.TempDir()
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.Local)
	now := func() time.Time { return day }

	first, err := newDaily(base, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := newDaily(base, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(base, "logs", "260927.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "one\ntwo\n" {
		t.Fatalf("log contents = %q, want %q", got, "one\ntwo\n")
	}
}

func TestDailyWriterRetriesFailedRotation(t *testing.T) {
	base := t.TempDir()
	day := time.Date(2026, 9, 27, 23, 59, 0, 0, time.Local)
	w, err := newDaily(base, func() time.Time { return day })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	next := filepath.Join(base, "logs", "260928.log")
	if err := os.Mkdir(next, 0o755); err != nil {
		t.Fatal(err)
	}
	day = day.Add(24 * time.Hour)
	if _, err := w.Write([]byte("kept\n")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(base, "logs", "260927.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "rotate to 260928.log failed") || !strings.Contains(string(raw), "kept\n") {
		t.Fatalf("fallback log = %q", raw)
	}

	if err := os.Remove(next); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("rotated\n")); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "rotated\n" {
		t.Fatalf("rotated log = %q, want %q", got, "rotated\n")
	}
}

func TestNewDailyRejectsFileAtLogsPath(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "logs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDaily(base); err == nil {
		t.Fatal("NewDaily succeeded with a file at logs path")
	}
}
