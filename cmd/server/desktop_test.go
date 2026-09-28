package main

import (
	"errors"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
)

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) {
	return 0, errors.New("console handle is unavailable")
}

func TestPanelMirrorWritesBeforeFailingSink(t *testing.T) {
	ring := panel.NewRing(10)
	writer := panelMirror(ring, failingLogWriter{})
	if _, err := writer.Write([]byte("task checkin ok\n")); err == nil {
		t.Fatal("expected failing sink error")
	}

	entries := ring.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("ring entries = %d, want 1", len(entries))
	}
	if entries[0].Text != "task checkin ok" {
		t.Fatalf("ring entry = %q", entries[0].Text)
	}
}

func TestPanelMirrorFallsBackWithoutRing(t *testing.T) {
	sink := failingLogWriter{}
	if got := panelMirror(nil, sink); got != sink {
		t.Fatal("nil ring did not return the base sink")
	}
}
