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

func TestIntervalJitterStaysWithinBounds(t *testing.T) {
	for _, interval := range []time.Duration{time.Nanosecond, time.Second, time.Minute} {
		task := &Task{Interval: interval, Jitter: true}
		for i := 0; i < 100; i++ {
			got := task.nextInterval()
			if got < interval || got > interval+interval/10 {
				t.Fatalf("interval outside bounds: %s", got)
			}
		}
	}
}

func TestJitterDisabledPreservesInterval(t *testing.T) {
	task := &Task{Interval: time.Minute}
	if task.nextInterval() != task.Interval {
		t.Fatal("default task schedule changed")
	}
	task.Jitter = true
	task.Interval = time.Duration(1<<63 - 1)
	if task.nextInterval() != task.Interval {
		t.Fatal("jitter duration overflow")
	}
}
