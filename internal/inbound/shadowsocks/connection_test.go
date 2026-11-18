package shadowsocks

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type blockedResponse struct {
	net.Conn
	started chan struct{}
	writing atomic.Bool
	closed  atomic.Int32
	t       *testing.T
}

func (c *blockedResponse) Write(p []byte) (int, error) {
	c.writing.Store(true)
	close(c.started)
	n, err := c.Conn.Write(p)
	c.writing.Store(false)
	return n, err
}
func (c *blockedResponse) Close() error {
	if c.writing.Load() {
		c.t.Error("released response writer before interrupted I/O returned")
	}
	c.closed.Add(1)
	return c.Conn.Close()
}
func TestCloseInterruptsResponseBeforeReleasingBuffers(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	inner := &blockedResponse{Conn: raw, started: make(chan struct{}), t: t}
	conn := &closeSafeConn{Conn: inner, raw: raw}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = conn.Write([]byte("response")) }()
	<-inner.started
	closed := make(chan struct{})
	go func() { defer close(closed); _ = conn.Close(); _ = conn.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close deadlocked behind blocked response")
	}
	<-done
	if inner.closed.Load() != 1 {
		t.Fatal("encrypted connection closed more than once")
	}
	if _, err := conn.Write(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatal("write reached released writer")
	}
	if _, err := conn.Read(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatal("read reached released reader")
	}
}
