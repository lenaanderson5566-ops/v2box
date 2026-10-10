package core

import (
	"context"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/sing-box/adapter"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"io"
	"net"
	"sync"
	"time"
)

type trafficKey struct {
	tag string
	uid int
}
type trafficTracker struct {
	mu          sync.Mutex
	users       map[string]map[string]int
	traffic     map[trafficKey]panel.UserTraffic
	activity    map[trafficKey]int64
	connections map[string]map[io.Closer]struct{}
}

func newTrafficTracker() *trafficTracker {
	return &trafficTracker{users: map[string]map[string]int{}, traffic: map[trafficKey]panel.UserTraffic{}, activity: map[trafficKey]int64{}, connections: map[string]map[io.Closer]struct{}{}}
}
func (t *trafficTracker) setUsers(tag string, users []panel.UserInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := map[string]int{}
	for _, u := range users {
		m[u.Uuid] = u.Id
	}
	t.users[tag] = m
}
func (t *trafficTracker) account(tag string, uid int, up, down int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := trafficKey{tag, uid}
	v := t.traffic[k]
	v.UID = uid
	v.Upload += int64(up)
	v.Download += int64(down)
	t.traffic[k] = v
	t.activity[k] += int64(up + down)
}
func (t *trafficTracker) takeTraffic(tag string, min int) []panel.UserTraffic {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []panel.UserTraffic
	for k, v := range t.traffic {
		if k.tag == tag && v.Upload+v.Download > int64(min)*1000 {
			out = append(out, v)
			delete(t.traffic, k)
		}
	}
	return out
}
func (t *trafficTracker) restoreTraffic(tag string, items []panel.UserTraffic) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range items {
		k := trafficKey{tag, v.UID}
		old := t.traffic[k]
		old.UID = v.UID
		old.Upload += v.Upload
		old.Download += v.Download
		t.traffic[k] = old
	}
}
func (t *trafficTracker) closeNode(tag string) {
	t.mu.Lock()
	connections := t.connections[tag]
	delete(t.connections, tag)
	t.mu.Unlock()
	for c := range connections {
		_ = c.Close()
	}
}
func (t *trafficTracker) register(tag string, c io.Closer, ctx context.Context) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if t.connections[tag] == nil {
		t.connections[tag] = map[io.Closer]struct{}{}
	}
	t.connections[tag][c] = struct{}{}
	return true
}
func (t *trafficTracker) unregister(tag string, c io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.connections[tag], c)
}

type meter struct {
	tracker       *trafficTracker
	tag, user, ip string
	uid           int
	ctx           context.Context
	cancel        context.CancelFunc
	limiter       *limiter.Limiter
}

func (t *trafficTracker) newMeter(ctx context.Context, m adapter.InboundContext) *meter {
	if ctx.Err() != nil {
		return nil
	}
	t.mu.Lock()
	uid := t.users[m.Inbound][m.User]
	t.mu.Unlock()
	if uid == 0 {
		return nil
	}
	l, err := limiter.GetLimiter(m.Inbound)
	if err != nil {
		return nil
	}
	user := format.UserTag(m.Inbound, m.User)
	ip := m.Source.Addr.Unmap().String()
	if _, reject := l.CheckLimit(user, ip, true); reject {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return &meter{tracker: t, tag: m.Inbound, user: user, ip: ip, uid: uid, ctx: ctx, cancel: cancel, limiter: l}
}
func (m *meter) wait(n int) error {
	bucket, reject := m.limiter.CheckLimit(m.user, m.ip, true)
	if reject {
		return net.ErrClosed
	}
	if bucket == nil || n <= 0 {
		return nil
	}
	delay := bucket.Get().Take(int64(n))
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}

type meteredConn struct {
	net.Conn
	m    *meter
	once sync.Once
}

func (c *meteredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.m.tracker.account(c.m.tag, c.m.uid, n, 0)
		if e := c.m.wait(n); e != nil {
			return 0, e
		}
	}
	return n, err
}
func (c *meteredConn) Write(p []byte) (int, error) {
	if err := c.m.wait(len(p)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	c.m.tracker.account(c.m.tag, c.m.uid, 0, n)
	return n, err
}
func (c *meteredConn) Close() error {
	c.once.Do(func() { c.m.cancel(); c.m.tracker.unregister(c.m.tag, c) })
	return c.Conn.Close()
}

type meteredPacketConn struct {
	N.PacketConn
	m    *meter
	once sync.Once
}

func (c *meteredPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	d, err := c.PacketConn.ReadPacket(b)
	if err == nil {
		c.m.tracker.account(c.m.tag, c.m.uid, b.Len(), 0)
		err = c.m.wait(b.Len())
	}
	return d, err
}
func (c *meteredPacketConn) WritePacket(b *buf.Buffer, d M.Socksaddr) error {
	n := b.Len()
	if err := c.m.wait(n); err != nil {
		b.Release()
		return err
	}
	err := c.PacketConn.WritePacket(b, d)
	if err == nil {
		c.m.tracker.account(c.m.tag, c.m.uid, 0, n)
	}
	return err
}
func (c *meteredPacketConn) Close() error {
	c.once.Do(func() { c.m.cancel(); c.m.tracker.unregister(c.m.tag, c) })
	return c.PacketConn.Close()
}
func (t *trafficTracker) RoutedConnection(ctx context.Context, c net.Conn, m adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	meter := t.newMeter(ctx, m)
	if meter == nil {
		_ = c.Close()
		return &deniedConn{Conn: c}
	}
	wrapped := &meteredConn{Conn: c, m: meter}
	if !t.register(m.Inbound, wrapped, meter.ctx) {
		_ = wrapped.Close()
	}
	return wrapped
}
func (t *trafficTracker) RoutedPacketConnection(ctx context.Context, c N.PacketConn, m adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) N.PacketConn {
	meter := t.newMeter(ctx, m)
	if meter == nil {
		_ = c.Close()
		return &deniedPacketConn{PacketConn: c}
	}
	wrapped := &meteredPacketConn{PacketConn: c, m: meter}
	if !t.register(m.Inbound, wrapped, meter.ctx) {
		_ = wrapped.Close()
	}
	return wrapped
}
func (t *trafficTracker) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}

type deniedConn struct{ net.Conn }

func (*deniedConn) Read([]byte) (int, error)  { return 0, net.ErrClosed }
func (*deniedConn) Write([]byte) (int, error) { return 0, net.ErrClosed }

type deniedPacketConn struct{ N.PacketConn }

func (*deniedPacketConn) ReadPacket(*buf.Buffer) (M.Socksaddr, error) {
	return M.Socksaddr{}, net.ErrClosed
}
func (*deniedPacketConn) WritePacket(b *buf.Buffer, _ M.Socksaddr) error {
	b.Release()
	return net.ErrClosed
}

// Expose protocol buffer requirements without allowing sing's copy helpers to
// unwrap the metering/limiting layer and bypass accounting.
func (c *meteredConn) Upstream() any                 { return c.Conn }
func (c *meteredConn) ReaderReplaceable() bool       { return false }
func (c *meteredConn) WriterReplaceable() bool       { return false }
func (c *meteredPacketConn) Upstream() any           { return c.PacketConn }
func (c *meteredPacketConn) ReaderReplaceable() bool { return false }
func (c *meteredPacketConn) WriterReplaceable() bool { return false }

func (t *trafficTracker) takeActivity(tag string) map[int]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[int]int64{}
	for k, n := range t.activity {
		if k.tag == tag {
			out[k.uid] = n
			delete(t.activity, k)
		}
	}
	return out
}
