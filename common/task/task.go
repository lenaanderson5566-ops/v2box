package task

import (
	"context"
	log "github.com/sirupsen/logrus"
	"math/rand/v2"
	"sync"
	"time"
)

// Execute is synchronous within a task: timeout cancellation never starts an
// overlapping invocation. Close waits for the active callback before teardown.
type Task struct {
	Name     string
	Interval time.Duration
	// Called only on the task goroutine, after Execute; never mutate this callback at runtime.
	NextInterval func() time.Duration
	Jitter       bool
	Execute      func(context.Context) error
	ReloadCh     chan struct{}
	mu           sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
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
		ticker := time.NewTicker(t.nextInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.execute(ctx)
				if t.Jitter || t.NextInterval != nil {
					ticker.Reset(t.nextInterval())
				}
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

// Positive jitter spreads panel requests without polling more frequently than
// configured. Each task still has exactly one synchronous callback at a time.
func (t *Task) nextInterval() time.Duration {
	interval := t.Interval
	if t.NextInterval != nil {
		if next := t.NextInterval(); next > 0 {
			interval = next
		}
	}
	if !t.Jitter {
		return interval
	}
	spread := interval / 10
	if spread <= 0 || interval > time.Duration(1<<63-1)-spread {
		return interval
	}
	return interval + time.Duration(rand.Int64N(int64(spread)))
}
