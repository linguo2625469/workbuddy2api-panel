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
// Windows daily file sink. Non-Windows builds retain their current output.
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
	if runtime.GOOS != "windows" {
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
	s.system = io.MultiWriter(file, os.Stderr)
	s.chat = io.MultiWriter(file, os.Stdout)
	return s, nil
}

func (s *logSinks) init() {
	log.SetOutput(s.system)
}

func (s *logSinks) attachPanel(p *panel.Panel) {
	system := s.system
	chat := s.chat
	if p != nil {
		system = io.MultiWriter(system, p.Logs())
		chat = io.MultiWriter(chat, p.Logs())
	}
	log.SetOutput(system)
	server.SetChatLogOutput(chat)
}

func (s *logSinks) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	return s.closer.Close()
}
