package cmd

import (
	"context"
	"testing"
	"time"
)

func TestLifecycleWatchdog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	expired := make(chan struct{})
	stop := lifecycleWatchdog(ctx, 10*time.Millisecond, func() { close(expired) })
	cancel()
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("stalled shutdown was not detected")
	}
	stop()
}
func TestLifecycleWatchdogStopsAfterCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	expired := make(chan struct{})
	stop := lifecycleWatchdog(ctx, 10*time.Millisecond, func() { close(expired) })
	stop()
	cancel()
	select {
	case <-expired:
		t.Fatal("completed lifecycle triggered watchdog")
	case <-time.After(30 * time.Millisecond):
	}
}
