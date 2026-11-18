package core

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	N "github.com/sagernet/sing/common/network"
)

type routingShutdownKey struct{}

// Wait for route selection to finish before the Box releases its rule sets.
// Established streams are owned by the core's existing connection trackers.
type routingShutdown struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	closed  bool
	next    uint64
	pending map[uint64]io.Closer
}

func (g *routingShutdown) enter(conn io.Closer) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, false
	}
	if g.pending == nil {
		g.pending = make(map[uint64]io.Closer)
	}
	g.next++
	id := g.next
	g.pending[id] = conn
	g.wg.Add(1)
	return func() {
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		g.wg.Done()
	}, true
}

func (g *routingShutdown) close() {
	g.mu.Lock()
	g.closed = true
	pending := make([]io.Closer, 0, len(g.pending))
	for _, conn := range g.pending {
		pending = append(pending, conn)
	}
	g.mu.Unlock()
	// Interrupt sniffing/handshake reads as well as context-aware DNS lookups.
	for _, conn := range pending {
		_ = conn.Close()
	}
	g.wg.Wait()
}

type guardedInboundRegistry struct{ adapter.InboundRegistry }

func (r guardedInboundRegistry) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag, kind string, options any) (adapter.Inbound, error) {
	if gate, ok := ctx.Value(routingShutdownKey{}).(*routingShutdown); ok {
		router = &shutdownRouter{Router: router, gate: gate}
	}
	return r.InboundRegistry.Create(ctx, router, logger, tag, kind, options)
}

type shutdownRouter struct {
	adapter.Router
	gate *routingShutdown
}

func (r *shutdownRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	leave, ok := r.gate.enter(conn)
	if !ok {
		N.CloseOnHandshakeFailure(conn, onClose, net.ErrClosed)
		return
	}
	defer leave()
	r.Router.RouteConnectionEx(ctx, conn, metadata, onClose)
}

func (r *shutdownRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	leave, ok := r.gate.enter(conn)
	if !ok {
		N.CloseOnHandshakeFailure(conn, onClose, net.ErrClosed)
		return
	}
	defer leave()
	r.Router.RoutePacketConnectionEx(ctx, conn, metadata, onClose)
}

func (r *shutdownRouter) RouteConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	leave, ok := r.gate.enter(conn)
	if !ok {
		_ = conn.Close()
		return net.ErrClosed
	}
	defer leave()
	return r.Router.RouteConnection(ctx, conn, metadata)
}

func (r *shutdownRouter) RoutePacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	leave, ok := r.gate.enter(conn)
	if !ok {
		_ = conn.Close()
		return net.ErrClosed
	}
	defer leave()
	return r.Router.RoutePacketConnection(ctx, conn, metadata)
}
