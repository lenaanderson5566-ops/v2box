package shadowsocks

import (
	"net"
	"sync"
	"sync/atomic"
)

type rawConnectionKey struct{}

// Upstream SS2022 initializes its response writer on the first Write, while
// Close inspects and releases it without synchronization. Interrupt raw I/O
// first, then wait for active encrypted reads/writes before releasing buffers.
// Keep this wrapper opaque so optimized copies cannot bypass these locks.
type closeSafeConn struct {
	net.Conn
	raw    net.Conn
	reads  sync.Mutex
	writes sync.Mutex
	once   sync.Once
	closed atomic.Bool
	err    error
}

func (c *closeSafeConn) Read(p []byte) (int, error) {
	c.reads.Lock()
	defer c.reads.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.Conn.Read(p)
}
func (c *closeSafeConn) Write(p []byte) (int, error) {
	c.writes.Lock()
	defer c.writes.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}
func (c *closeSafeConn) Close() error {
	c.once.Do(func() {
		c.closed.Store(true)
		_ = c.raw.Close()
		c.reads.Lock()
		defer c.reads.Unlock()
		c.writes.Lock()
		defer c.writes.Unlock()
		c.err = c.Conn.Close()
	})
	return c.err
}
