package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseWaitsForCallback(t *testing.T) {
	started := make(chan struct{})
	var ended atomic.Bool
	task := &Task{Name: "test", Interval: time.Hour, Execute: func(ctx context.Context) error { close(started); <-ctx.Done(); ended.Store(true); return ctx.Err() }}
	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback not started")
	}
	task.Close()
	if !ended.Load() {
		t.Fatal("Close returned before callback completed")
	}
	task.Close()
}
