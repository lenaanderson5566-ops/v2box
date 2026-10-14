// Package authconn owns authenticated transport sessions independently of the
// streams routed through them. Identities are opaque account generations.
package authconn

import (
	"github.com/sagernet/sing/common/bufio"
	"net"
	"sync"
)

type Registry[U comparable] struct {
	mu       sync.Mutex
	allowed  map[U]bool
	sessions map[*session[U]]U
}
type session[U comparable] struct {
	registry *Registry[U]
	once     sync.Once
	close    func()
}

func (s *session[U]) release() {
	s.registry.mu.Lock()
	delete(s.registry.sessions, s)
	s.registry.mu.Unlock()
}
func (s *session[U]) stop() { s.once.Do(func() { s.release(); s.close() }) }

// Replace rejects old generations immediately. Network close runs outside the
// publishing call, which may hold the core's connection-admission barrier.
func (r *Registry[U]) Replace(users []U) {
	allowed := make(map[U]bool, len(users))
	for _, u := range users {
		allowed[u] = true
	}
	r.mu.Lock()
	r.allowed = allowed
	var revoked []*session[U]
	for s, u := range r.sessions {
		if !allowed[u] {
			revoked = append(revoked, s)
			delete(r.sessions, s)
		}
	}
	r.mu.Unlock()
	if len(revoked) > 0 {
		go func() {
			for _, s := range revoked {
				s.stop()
			}
		}()
	}
}
func (r *Registry[U]) track(user U, close func()) (*session[U], bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.allowed[user] {
		return nil, false
	}
	if r.sessions == nil {
		r.sessions = make(map[*session[U]]U)
	}
	s := &session[U]{registry: r, close: close}
	r.sessions[s] = user
	return s, true
}

// Track returns a release callback for transports with their own close context.
func (r *Registry[U]) Track(user U, close func()) (func(), bool) {
	s, ok := r.track(user, close)
	if !ok {
		return nil, false
	}
	return s.release, true
}
func (r *Registry[U]) Wrap(user U, conn net.Conn) (net.Conn, error) {
	var prefix []byte
	for {
		cached, ok := conn.(*bufio.CachedConn)
		if !ok {
			break
		}
		if b := cached.ReadCached(); b != nil {
			prefix = append(prefix, b.Bytes()...)
			b.Release()
		}
		conn = cached.Conn
	}
	s, ok := r.track(user, func() { _ = conn.Close() })
	if !ok {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	return &trackedConn[U]{Conn: conn, session: s, prefix: prefix, hasPrefix: len(prefix) > 0}, nil
}

type trackedConn[U comparable] struct {
	net.Conn
	session   *session[U]
	prefix    []byte
	hasPrefix bool
	readMu    sync.Mutex
}

func (c *trackedConn[U]) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func (c *trackedConn[U]) Close() error            { c.session.stop(); return nil }
func (c *trackedConn[U]) Upstream() any           { return c.Conn }
func (c *trackedConn[U]) ReaderReplaceable() bool { return !c.hasPrefix }
func (c *trackedConn[U]) WriterReplaceable() bool { return true }
