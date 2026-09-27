// Package applog provides the Windows portable build's daily file sink.
package applog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer appends log records to <baseDir>/logs/yymmdd.log and rotates them at
// local midnight. A failed rotation keeps the previous file active and retries
// on the next write so logging never disappears because of a transient error.
type Writer struct {
	mu     sync.Mutex
	dir    string
	now    func() time.Time
	day    string
	file   *os.File
	closed bool
}

// NewDaily creates the logs directory below baseDir and opens today's log.
func NewDaily(baseDir string) (*Writer, error) {
	if baseDir == "" {
		return nil, errors.New("applog: empty base directory")
	}
	return newDaily(baseDir, time.Now)
}

func newDaily(baseDir string, now func() time.Time) (*Writer, error) {
	w := &Writer{
		dir: filepath.Join(baseDir, "logs"),
		now: now,
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %s: %w", w.dir, err)
	}
	day := w.currentDay()
	if err := w.open(day); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends one complete log record. It implements io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, errors.New("applog: writer is closed")
	}

	day := w.currentDay()
	if day != w.day {
		if err := w.open(day); err != nil {
			// Keep the old file usable. The marker and the current record are
			// written there, and rotation is retried on the next call.
			marker := fmt.Sprintf("%s applog: rotate to %s.log failed: %v\n",
				w.now().Format("2006/01/02 15:04:05"), day, err)
			_, _ = io.WriteString(w.file, marker)
		}
	}

	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	return n, nil
}

// Close closes the active log file. It is safe to call more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

func (w *Writer) currentDay() string {
	return w.now().Format("060102")
}

func (w *Writer) open(day string) error {
	path := filepath.Join(w.dir, day+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	old := w.file
	w.file = file
	w.day = day
	if old != nil {
		_ = old.Close()
	}
	return nil
}
