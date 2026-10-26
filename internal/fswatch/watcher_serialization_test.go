package fswatch

import (
	"testing"
	"time"
)

func TestCallbacksSerializePathsWithoutBlockingAnotherWatcher(t *testing.T) {
	first, second, independent := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	w := &Watcher{timerMap: map[string]pendingCallback{"cert": {generation: 1}, "key": {generation: 1}}}
	w.callback = func(path string) {
		if path == "cert" {
			close(first)
			<-release
		} else {
			close(second)
		}
	}
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { defer close(doneA); w.fireCallback("cert", 1) }()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}
	go func() { defer close(doneB); w.fireCallback("key", 1) }()
	other := &Watcher{timerMap: map[string]pendingCallback{"other": {generation: 1}}, callback: func(string) { close(independent) }}
	go other.fireCallback("other", 1)
	select {
	case <-independent:
	case <-time.After(time.Second):
		t.Fatal("one watcher blocked another")
	}
	select {
	case <-second:
		t.Fatal("certificate and key callbacks overlapped")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	released = true
	for _, done := range []chan struct{}{doneA, doneB} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("callback did not finish")
		}
	}
	select {
	case <-second:
	default:
		t.Fatal("serialized key callback was lost")
	}
}

func TestClosedWatcherDiscardsQueuedCallback(t *testing.T) {
	started, release, doneA, doneB := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	called := make(chan struct{}, 1)
	pending := time.NewTimer(time.Hour)
	defer pending.Stop()
	w := &Watcher{timerMap: map[string]pendingCallback{"cert": {generation: 1}, "key": {generation: 1, timer: pending}}}
	w.callback = func(path string) {
		if path == "cert" {
			close(started)
			<-release
		} else {
			called <- struct{}{}
		}
	}
	go func() { defer close(doneA); w.fireCallback("cert", 1) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	go func() { defer close(doneB); w.fireCallback("key", 1) }()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	for _, done := range []chan struct{}{doneA, doneB} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("callback did not terminate")
		}
	}
	select {
	case <-called:
		t.Fatal("closed watcher ran a pending callback")
	default:
	}
}
