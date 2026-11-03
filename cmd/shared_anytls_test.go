package cmd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lenaanderson5566-ops/v2box/conf"
	hotanytls "github.com/lenaanderson5566-ops/v2box/internal/inbound/anytls"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/option"
	sj "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
)

func sharedGroupClient(t *testing.T, p *groupPanel, domain string) *box.Box {
	t.Helper()
	out := outbound.NewRegistry()
	hotanytls.RegisterOutbound(out)
	d := dns.NewTransportRegistry()
	local.RegisterTransport(d)
	ctx := box.Context(context.Background(), inbound.NewRegistry(), out, endpoint.NewRegistry(), d, service.NewRegistry(), certificate.NewRegistry())
	raw, _ := json.Marshal(map[string]any{"outbounds": []any{map[string]any{"type": "anytls", "tag": "proxy", "server": "127.0.0.1", "server_port": p.port.Load(), "password": p.uuid, "tls": map[string]any{"enabled": true, "server_name": domain, "insecure": true}}}})
	var options option.Options
	if err := sj.UnmarshalContext(ctx, raw, &options); err != nil {
		t.Fatal(err)
	}
	b, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}
func TestSharedAnyTLSSupervisorsPreservePeer(t *testing.T) {
	limiter.Init()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	defer seed.Close()
	cert := seed.TLS.Certificates[0]
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	tls := map[string]any{"certMode": "remote", "serverName": leaf.DNSNames[0], "tlsCert": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})), "tlsKey": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))}
	a := newGroupPanel(t, "b831381d-6324-4d53-ad4f-8cda48b30811", "panel-a", tls)
	b := newGroupPanel(t, "c831381d-6324-4d53-ad4f-8cda48b30822", "panel-b", tls)
	b.port.Store(a.port.Load())
	cfg := conf.New()
	cfg.OutboundAllowCIDRs = []string{"127.0.0.0/8"}
	cfg.LogConfig.Level = "error"
	ea, eb := a.entry(t), b.entry(t)
	ea.SharedListener = "public"
	eb.SharedListener = "public"
	cfg.NodeConfigs = []conf.NodeConfig{ea, eb}
	g := newNodeGroup(context.Background())
	g.retry = 50 * time.Millisecond
	defer g.Close()
	if err = g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return a.applied.Load() > 0 && b.applied.Load() > 0 })
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, e := echo.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	ca, cb := sharedGroupClient(t, a, leaf.DNSNames[0]), sharedGroupClient(t, b, leaf.DNSNames[0])
	dial := func(client *box.Box) net.Conn {
		t.Helper()
		out, _ := client.Outbound().Outbound("proxy")
		c, e := out.DialContext(context.Background(), "tcp", M.ParseSocksaddr(echo.Addr().String()))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c.Close() })
		groupPing(t, c)
		return c
	}
	sa, sb := dial(ca), dial(cb)
	keyA, _ := conf.NodeIdentity(ea)
	// A conflicting listener change must be rejected before closing A or B.
	before := a.failed.Load()
	a.port.Store(int64(groupFreePort(t)))
	g.workers[keyA].reload <- struct{}{}
	waitGroup(t, func() bool { return a.failed.Load() > before })
	groupPing(t, sa)
	groupPing(t, sb)
	// A panel outage during replacement leaves both applied tables available.
	a.fail.Store(true)
	g.workers[keyA].reload <- struct{}{}
	groupPing(t, sb)
	// Removing the original listener owner still must not close B's socket.
	cfg.NodeConfigs = []conf.NodeConfig{eb}
	if err = g.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.retiring) == 0 })
	groupPing(t, sb)
	dial(cb)
}
