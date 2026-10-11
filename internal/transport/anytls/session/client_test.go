package session

import (
	"context"
	"testing"
	"time"
)

func TestClientCloseStopsIdleCleanup(t *testing.T) {
	c := NewClient(context.Background(), nil, nil, nil, time.Hour, time.Hour, 0)
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt idle cleanup")
	}
	select {
	case <-c.cleanupDone:
	default:
		t.Fatal("cleanup goroutine still running after Close")
	}
	c.Close()
}
