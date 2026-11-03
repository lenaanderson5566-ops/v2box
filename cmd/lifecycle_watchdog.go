package cmd

import (
	"context"
	"time"
)

// Cooperative cancellation handles normal I/O. A stuck third-party Close must
// eventually exit so the service manager can recover instead of hanging forever.
func lifecycleWatchdog(ctx context.Context, grace time.Duration, expired func()) func() {
	finished := make(chan struct{})
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-finished:
			return
		case <-timer.C:
		}
		// Prefer a completed operation if completion and the timer became ready together.
		select {
		case <-finished:
			return
		default:
			expired()
		}
	}()
	return func() { close(finished) }
}
