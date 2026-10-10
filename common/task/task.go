package task

import (
	"context"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

// Execute is synchronous within a task: timeout cancellation never starts an
// overlapping invocation. Close waits for the active callback before teardown.
type Task struct {
	Name     string
	Interval time.Duration
	Execute  func(context.Context) error
	ReloadCh chan struct{}
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func (t *Task) Start(first bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.done = make(chan struct{})
	go func() {
		defer close(t.done)
		if first {
			t.execute(ctx)
		}
		ticker := time.NewTicker(t.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.execute(ctx)
			}
		}
	}()
	return nil
}
func (t *Task) execute(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, min(5*t.Interval, 5*time.Minute))
	defer cancel()
	if err := t.Execute(ctx); err != nil && parent.Err() == nil {
		log.WithError(err).Warnf("Task %s failed", t.Name)
	}
}
func (t *Task) Close() {
	t.mu.Lock()
	cancel, done := t.cancel, t.done
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
