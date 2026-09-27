//go:build !windows || !tray

package main

import "errors"

type singleInstance struct{}

func acquireSingleInstance() (*singleInstance, bool, error) {
	return &singleInstance{}, false, nil
}

func (s *singleInstance) Close() error { return nil }

func openPanelInBrowser(string) error {
	return errors.New("desktop browser integration is only available on Windows")
}

func showFatalError(string, string) {}
