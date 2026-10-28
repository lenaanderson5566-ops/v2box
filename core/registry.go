package core

import (
	"context"
	hotanytls "github.com/lenaanderson5566-ops/v2box/internal/inbound/anytls"
	hothysteria2 "github.com/lenaanderson5566-ops/v2box/internal/inbound/hysteria2"
	hotnaive "github.com/lenaanderson5566-ops/v2box/internal/inbound/naive"
	hotshadowsocks "github.com/lenaanderson5566-ops/v2box/internal/inbound/shadowsocks"
	hottrojan "github.com/lenaanderson5566-ops/v2box/internal/inbound/trojan"
	hottuic "github.com/lenaanderson5566-ops/v2box/internal/inbound/tuic"
	hotvless "github.com/lenaanderson5566-ops/v2box/internal/inbound/vless"
	hotvmess "github.com/lenaanderson5566-ops/v2box/internal/inbound/vmess"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
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
	hotvmess.RegisterInbound(in)
	hotvless.RegisterInbound(in)
	hottrojan.RegisterInbound(in)
	hotshadowsocks.RegisterInbound(in)
	hothysteria2.RegisterInbound(in)
	hottuic.RegisterInbound(in)
	hotanytls.RegisterInbound(in)
	hotnaive.RegisterInbound(in)
	direct.RegisterOutbound(out)
	socks.RegisterOutbound(out)
	http.RegisterOutbound(out)
	vmess.RegisterOutbound(out)
	vless.RegisterOutbound(out)
	trojan.RegisterOutbound(out)
	shadowsocks.RegisterOutbound(out)
	hysteria2.RegisterOutbound(out)
	tuic.RegisterOutbound(out)
	hotanytls.RegisterOutbound(out)
	local.RegisterTransport(d)
	transport.RegisterUDP(d)
	transport.RegisterTCP(d)
	transport.RegisterTLS(d)
	transport.RegisterHTTPS(d)
	certificates := certificate.NewRegistry()
	registerSharedCertificateProvider(certificates)
	return box.Context(ctx, guardedInboundRegistry{in}, out, endpoint.NewRegistry(), d, service.NewRegistry(), certificates)
}
