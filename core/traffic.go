package core

import (
	"context"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/sing-box/adapter"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type trafficKey struct {
	tag string
	uid int
}
type trafficTracker struct {
	targets     targetTracker
	budgets     map[string]*localTrafficBudget
	hasBudgets  atomic.Bool
	mu          sync.RWMutex
	users       map[string]map[string]panel.UserInfo
	counters    [64]trafficCounters
	connections map[string]map[io.Closer]*meter
}

type trafficCounters struct {
	mu       sync.Mutex
	traffic  map[trafficKey]panel.UserTraffic
	activity map[trafficKey]int64
}

func newTrafficTracker() *trafficTracker {
	t := &trafficTracker{budgets: map[string]*localTrafficBudget{}, users: map[string]map[string]panel.UserInfo{}, connections: map[string]map[io.Closer]*meter{}}
	for i := range t.counters {
		t.counters[i].traffic = map[trafficKey]panel.UserTraffic{}
		t.counters[i].activity = map[trafficKey]int64{}
	}
	return t
}
func (t *trafficTracker) counter(tag string, uid int) *trafficCounters {
	hash := uint64(uid) * 11400714819323198485
	for i := 0; i < len(tag); i++ {
		hash = (hash ^ uint64(tag[i])) * 1099511628211
	}
	return &t.counters[hash%uint64(len(t.counters))]
}
func (t *trafficTracker) setUsers(tag string, users []panel.UserInfo) {
	m := map[string]panel.UserInfo{}
	for _, u := range users {
		m[u.Uuid] = u
	}
	_ = t.updateUsers(tag, m, func() error { return nil })
}

// Authentication publication and connection admission share this barrier.
// Protocol updates only swap credential tables; they never perform network I/O.
func (t *trafficTracker) updateUsers(tag string, users map[string]panel.UserInfo, apply func() error) error {
	t.mu.Lock()
	if err := apply(); err != nil {
		t.mu.Unlock()
		return err
	}
	if len(users) == 0 {
		delete(t.users, tag)
	} else {
		t.users[tag] = users
	}
	var revoked []io.Closer
	for c, m := range t.connections[tag] {
		if u, ok := users[m.identity]; !ok || u.Id != m.uid {
			m.cancel()
			revoked = append(revoked, c)
			delete(t.connections[tag], c)
		}
	}
	t.mu.Unlock()
	for _, c := range revoked {
		_ = c.Close()
	}
	return nil
}
func (t *trafficTracker) account(tag string, uid int, up, down int) {
	if up == 0 && down == 0 {
		return
	}
	// When no budgets are configured, packet I/O skips admission locking.
	if t.hasBudgets.Load() {
		t.accountBudget(tag, up, down)
	}
	c := t.counter(tag, uid)
	c.mu.Lock()
	defer c.mu.Unlock()
	k := trafficKey{tag, uid}
	v := c.traffic[k]
	v.UID = uid
	v.Upload += int64(up)
	v.Download += int64(down)
	c.traffic[k] = v
	c.activity[k] += int64(up + down)
}

// Metadata publication stays under the admission barrier. Byte increments use
// the shared quota's atomic counter, allowing independent users to account in
// parallel while a period/configuration update still excludes all increments.
func (t *trafficTracker) accountBudget(tag string, up, down int) {
	t.mu.RLock()
	var closeList []io.Closer
	if b := t.budgets[tag]; b != nil {
		var bytes int64
		if b.CountingMode != "download" {
			bytes += int64(up)
		}
		if b.CountingMode != "upload" {
			bytes += int64(down)
		}
		used := b.used.Add(bytes)
		// Only the increment crossing the threshold schedules connection closure.
		if b.LimitBytes > 0 && used-bytes < b.LimitBytes && used >= b.LimitBytes {
			for other, quota := range t.budgets {
				if quota == b {
					for c, m := range t.connections[other] {
						m.cancel()
						closeList = append(closeList, c)
					}
				}
			}
		}
	}
	t.mu.RUnlock()
	if len(closeList) > 0 {
		go func() {
			for _, c := range closeList {
				_ = c.Close()
			}
		}()
	}
}
func (t *trafficTracker) takeTraffic(tag string, min int) []panel.UserTraffic {
	var out []panel.UserTraffic
	for i := range t.counters {
		c := &t.counters[i]
		c.mu.Lock()
		for k, v := range c.traffic {
			if k.tag == tag && v.Upload+v.Download > int64(min)*1000 {
				out = append(out, v)
				delete(c.traffic, k)
			}
		}
		c.mu.Unlock()
	}
	return out
}
func (t *trafficTracker) restoreTraffic(tag string, items []panel.UserTraffic) {
	for _, v := range items {
		c := t.counter(tag, v.UID)
		c.mu.Lock()
		k := trafficKey{tag, v.UID}
		old := c.traffic[k]
		old.UID = v.UID
		old.Upload += v.Upload
		old.Download += v.Download
		c.traffic[k] = old
		c.mu.Unlock()
	}
}
func (t *trafficTracker) closeNode(tag string) {
	t.targets.policy(tag, panel.TargetPolicy{})
	t.mu.Lock()
	connections := t.connections[tag]
	delete(t.connections, tag)
	t.mu.Unlock()
	for c := range connections {
		_ = c.Close()
	}
}
func (t *trafficTracker) register(tag string, c io.Closer, m *meter) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.budgetBlockedLocked(tag) || m.ctx.Err() != nil || t.users[tag][m.identity].Id != m.uid {
		return false
	}
	if t.connections[tag] == nil {
		t.connections[tag] = map[io.Closer]*meter{}
	}
	t.connections[tag][c] = m
	return true
}
func (t *trafficTracker) unregister(tag string, c io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.connections[tag], c)
}

type meter struct {
	target, resolvedTarget M.Socksaddr
	targetGeneration       uint64
	targetSeen             map[targetKey]bool // guarded by the active node/user shard
	targetNames            map[M.Socksaddr]targetKey
	tracker                *trafficTracker
	tag, user, ip          string
	uid                    int
	identity               string
	ctx                    context.Context
	cancel                 context.CancelFunc
	limiter                *limiter.Limiter
}

func (t *trafficTracker) newMeter(ctx context.Context, m adapter.InboundContext) *meter {
	if ctx.Err() != nil {
		return nil
	}
	t.mu.Lock()
	u := t.users[m.Inbound][m.User]
	if t.budgetBlockedLocked(m.Inbound) {
		u.Id = 0
	}
	t.mu.Unlock()
	if u.Id == 0 {
		return nil
	}
	l, err := limiter.GetLimiter(m.Inbound)
	if err != nil {
		return nil
	}
	user := format.UserTag(m.Inbound, u.Uuid)
	ip := m.Source.Addr.Unmap().String()
	if _, reject := l.CheckLimit(user, ip, true); reject {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return &meter{tracker: t, tag: m.Inbound, user: user, identity: m.User, ip: ip, uid: u.Id, ctx: ctx, cancel: cancel, limiter: l,
		target: originalTarget(m.Destination, m.RouteOriginalDestination, m.OriginDestination), resolvedTarget: m.Destination}
}
func (m *meter) wait(n int) error {
	m.tracker.mu.RLock()
	blocked := m.tracker.budgetBlockedLocked(m.tag)
	m.tracker.mu.RUnlock()
	if blocked {
		return net.ErrClosed
	}
	if err := m.ctx.Err(); err != nil {
		return err
	}
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
	m        *meter
	once     sync.Once
	prefix   []byte
	readMu   sync.Mutex
	closeErr error
}

func (c *meteredConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	var n int
	var err error
	if len(c.prefix) > 0 {
		n = copy(p, c.prefix)
		c.prefix = c.prefix[n:]
	} else {
		n, err = c.Conn.Read(p)
	}
	if n > 0 {
		c.m.tracker.targets.record(c.m, c.m.target, n, 0)
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
	if n > 0 {
		c.m.tracker.targets.record(c.m, c.m.target, 0, n)
	}
	c.m.tracker.account(c.m.tag, c.m.uid, 0, n)
	return n, err
}
func (c *meteredConn) Close() error {
	c.once.Do(func() { c.m.cancel(); c.m.tracker.unregister(c.m.tag, c); c.closeErr = c.Conn.Close() })
	return c.closeErr
}

type meteredPacketConn struct {
	N.PacketConn
	m        *meter
	once     sync.Once
	closeErr error
}

func (c *meteredPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	d, err := c.PacketConn.ReadPacket(b)
	if err == nil {
		c.m.tracker.targets.record(c.m, packetTarget(c.m, d), b.Len(), 0)
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
		c.m.tracker.targets.record(c.m, packetTarget(c.m, d), 0, n)
		c.m.tracker.account(c.m.tag, c.m.uid, 0, n)
	}
	return err
}
func (c *meteredPacketConn) Close() error {
	c.once.Do(func() { c.m.cancel(); c.m.tracker.unregister(c.m.tag, c); c.closeErr = c.PacketConn.Close() })
	return c.closeErr
}
func (t *trafficTracker) RoutedConnection(ctx context.Context, c net.Conn, m adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	// The router's sniff cache does not support concurrent Read/Close. Take its
	// buffered bytes before forwarding begins; only the reader owns this prefix.
	// Closing the underlying stream can then safely interrupt a blocked Read.
	var prefix []byte
	for {
		cached, ok := c.(*bufio.CachedConn)
		if !ok {
			break
		}
		if b := cached.ReadCached(); b != nil {
			prefix = append(prefix, b.Bytes()...)
			b.Release()
		}
		c = cached.Conn
	}
	meter := t.newMeter(ctx, m)
	if meter == nil {
		_ = c.Close()
		return &deniedConn{Conn: c}
	}
	wrapped := &meteredConn{Conn: c, m: meter, prefix: prefix}
	if !t.register(m.Inbound, wrapped, meter) {
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
	if !t.register(m.Inbound, wrapped, meter) {
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
	out := map[int]int64{}
	for i := range t.counters {
		c := &t.counters[i]
		c.mu.Lock()
		for k, n := range c.activity {
			if k.tag == tag {
				out[k.uid] = n
				delete(c.activity, k)
			}
		}
		c.mu.Unlock()
	}
	return out
}
