//go:build !windows || !tray

package main

import "context"

func runDesktop(ctx context.Context, _ func()) error {
	<-ctx.Done()
	return nil
}
