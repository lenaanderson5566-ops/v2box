package core

import (
	"context"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/protocol/anytls"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
)

// Register the server protocols and supported route outbounds only. Importing
// the full desktop registry would also embed unrelated browser/VPN/AI services.
func boxContext(ctx context.Context) context.Context {
	in := inbound.NewRegistry()
	out := outbound.NewRegistry()
	d := dns.NewTransportRegistry()
	vmess.RegisterInbound(in)
	vless.RegisterInbound(in)
	trojan.RegisterInbound(in)
	shadowsocks.RegisterInbound(in)
	hysteria2.RegisterInbound(in)
	tuic.RegisterInbound(in)
	anytls.RegisterInbound(in)
	direct.RegisterOutbound(out)
	socks.RegisterOutbound(out)
	http.RegisterOutbound(out)
	vmess.RegisterOutbound(out)
	vless.RegisterOutbound(out)
	trojan.RegisterOutbound(out)
	shadowsocks.RegisterOutbound(out)
	hysteria2.RegisterOutbound(out)
	tuic.RegisterOutbound(out)
	anytls.RegisterOutbound(out)
	local.RegisterTransport(d)
	transport.RegisterUDP(d)
	transport.RegisterTCP(d)
	transport.RegisterTLS(d)
	transport.RegisterHTTPS(d)
	return box.Context(ctx, in, out, endpoint.NewRegistry(), d, service.NewRegistry(), certificate.NewRegistry())
}
