package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/linguo2625469/workbuddy2api-panel/internal/applog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// logSinks keeps the platform console streams separate from the optional
// Windows tray build's daily file sink. Console/default builds retain the
// upstream output behavior.
type logSinks struct {
	system io.Writer
	chat   io.Writer
	closer io.Closer
}

func newLogSinks() (*logSinks, error) {
	s := &logSinks{
		system: os.Stderr,
		chat:   os.Stdout,
	}
	if !trayBuild || runtime.GOOS != "windows" {
		return s, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	file, err := applog.NewDaily(filepath.Dir(exe))
	if err != nil {
		return nil, err
	}
	s.closer = file
	// The GUI subsystem has no reliable console handles. Writing to an invalid
	// os.Stdout/os.Stderr would make io.MultiWriter stop before reaching later
	// sinks, so the tray build uses the daily file as its only base sink.
	s.system = file
	s.chat = file
	return s, nil
}

func (s *logSinks) init() {
	log.SetOutput(s.system)
}

func (s *logSinks) attachPanel(p *panel.Panel) {
	system := s.system
	chat := s.chat
	if p != nil {
		system = panelMirror(p.Logs(), system)
		chat = panelMirror(p.Logs(), chat)
	}
	log.SetOutput(system)
	server.SetChatLogOutput(chat)
}

// panelMirror writes to the in-memory Panel ring before the file/console sink.
// A broken external handle must not prevent the log from reaching the Panel.
func panelMirror(ring, sink io.Writer) io.Writer {
	if ring == nil {
		return sink
	}
	if sink == nil {
		return ring
	}
	return io.MultiWriter(ring, sink)
}

func (s *logSinks) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	return s.closer.Close()
}
