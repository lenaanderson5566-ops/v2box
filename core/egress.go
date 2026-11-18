package core

import (
	"context"
	"fmt"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"net"
	"net/netip"
	"os"
)

var nonPublicCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "168.63.129.16/32", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/3", "::/96", "::ffff:0:0/96", "64:ff9b::/96",
	"64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
}

func restrictOutboundDestinations(config object, allow []string) error {
	allowed := make([]string, 0, len(allow))
	for _, value := range allow {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("invalid OutboundAllowCIDRs entry %q: %w", value, err)
		}
		allowed = append(allowed, prefix.Masked().String())
	}
	denied := object{"ip_is_private": true, "ip_cidr": nonPublicCIDRs}
	rule := object{"ip_is_private": true, "ip_cidr": nonPublicCIDRs, "action": "reject"}
	if len(allowed) != 0 {
		rule = object{"type": "logical", "mode": "and", "rules": []object{
			denied, {"ip_cidr": allowed, "invert": true},
		}, "action": "reject"}
	}
	route := config["route"].(object)
	// Resolve once before policy matching; the outbound uses these checked addresses.
	prefix := []any{object{"action": "resolve"}, rule}
	switch rules := route["rules"].(type) {
	case []object:
		for _, value := range rules {
			prefix = append(prefix, value)
		}
	case []any:
		prefix = append(prefix, rules...)
	}
	route["rules"] = prefix
	return nil
}

type egressTracker struct {
	allow  []netip.Prefix
	denied []netip.Prefix
}

func newEgressTracker(allow []string) (*egressTracker, error) {
	guard := &egressTracker{}
	for _, value := range allow {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, err
		}
		guard.allow = append(guard.allow, prefix.Masked())
	}
	for _, value := range nonPublicCIDRs {
		guard.denied = append(guard.denied, netip.MustParsePrefix(value))
	}
	return guard, nil
}

func (g *egressTracker) allows(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range g.allow {
		if prefix.Contains(address) {
			return true
		}
	}
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range g.denied {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func (g *egressTracker) RoutedConnection(_ context.Context, conn net.Conn, _ adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	return conn
}
func (g *egressTracker) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}
func (g *egressTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, _ adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) N.PacketConn {
	return &egressPacketConn{PacketConn: conn, ctx: ctx, guard: g, resolver: service.FromContext[adapter.DNSRouter](ctx)}
}

type egressPacketConn struct {
	N.PacketConn
	ctx      context.Context
	guard    *egressTracker
	resolver adapter.DNSRouter
}

func (c *egressPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	destination, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return destination, err
	}
	if destination.IsIP() {
		if !c.guard.allows(destination.Addr) {
			return M.Socksaddr{}, os.ErrPermission
		}
		return destination, nil
	}
	if !destination.IsDomain() || c.resolver == nil {
		return M.Socksaddr{}, os.ErrPermission
	}
	addresses, err := c.resolver.Lookup(c.ctx, destination.Fqdn, adapter.DNSQueryOptions{})
	if err != nil {
		return M.Socksaddr{}, err
	}
	if len(addresses) == 0 {
		return M.Socksaddr{}, os.ErrPermission
	}
	for _, address := range addresses {
		if !c.guard.allows(address) {
			return M.Socksaddr{}, os.ErrPermission
		}
	}
	return M.SocksaddrFrom(addresses[0], destination.Port), nil
}

func (c *egressPacketConn) Upstream() any           { return c.PacketConn }
func (c *egressPacketConn) ReaderReplaceable() bool { return false }
func (c *egressPacketConn) WriterReplaceable() bool { return false }
