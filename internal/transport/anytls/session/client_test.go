package session

import (
	"context"
	"net"
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

func TestCloseCancelsPendingDial(t *testing.T) {
	started := make(chan struct{})
	c := NewClient(context.Background(), nil, func(ctx context.Context) (net.Conn, error) { close(started); <-ctx.Done(); return nil, ctx.Err() }, nil, time.Hour, time.Hour, 0)
	done := make(chan error, 1)
	go func() { _, err := c.createSession(context.Background()); done <- err }()
	<-started
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("pending dial not canceled")
	}
}
func TestCloseRejectsLateConnection(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	left, right := net.Pipe()
	defer right.Close()
	c := NewClient(context.Background(), nil, func(context.Context) (net.Conn, error) { close(started); <-release; return left, nil }, nil, time.Hour, time.Hour, 0)
	done := make(chan error, 1)
	go func() { _, err := c.createSession(context.Background()); done <- err }()
	<-started
	c.Close()
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted late connection")
		}
	case <-time.After(time.Second):
		t.Fatal("late dial stuck")
	}
	if len(c.sessions) != 0 {
		t.Fatal("session retained after shutdown")
	}
	right.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := right.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection not closed")
	}
}
