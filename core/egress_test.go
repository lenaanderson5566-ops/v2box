package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	N "github.com/sagernet/sing/common/network"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestOutboundGuardCannotBeReplacedByPanelRules(t *testing.T) {
	for _, native := range []string{"", `{"route":{"rules":[{"ip_cidr":["127.0.0.0/8"],"action":"route","outbound":"direct"}],"final":"direct"}}`} {
		n := testNode(t, "vless")
		n.Common.SingBox = json.RawMessage(native)
		options, err := buildOptions(n, nil, conf.New())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(options)
		var config map[string]any
		_ = json.Unmarshal(data, &config)
		rules := config["route"].(map[string]any)["rules"].([]any)
		if rules[0].(map[string]any)["action"] != "resolve" || rules[1].(map[string]any)["action"] != "reject" {
			t.Fatalf("missing mandatory guard: %s", data)
		}
	}
	n := testNode(t, "vless")
	cfg := conf.New()
	cfg.OutboundAllowCIDRs = []string{"invalid"}
	if _, err := buildOptions(n, nil, cfg); err == nil {
		t.Fatal("invalid allow list accepted")
	}
}

func TestDefaultEgressRejectsLocalDestinations(t *testing.T) {
	limiter.Init()
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		t.Run(host, func(t *testing.T) {
			listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("local address is unavailable: %v", err)
			}
			defer listener.Close()
			n := testNode(t, "vless")
			startTestNodeWithConfig(t, n, conf.New())
			b := clientBox(t, n, testUUID)
			out, _ := b.Outbound().Outbound("proxy")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			port := listener.Addr().(*net.TCPAddr).Port
			conn, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(net.JoinHostPort(host, strconv.Itoa(port))))
			if err == nil {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, _ = conn.Write([]byte("guard"))
				_, err = conn.Read(make([]byte, 5))
			}
			if err == nil {
				t.Fatal("restricted connection succeeded")
			}
			_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
			if accepted, err := listener.Accept(); err == nil {
				accepted.Close()
				t.Fatal("restricted target received a connection")
			}
		})
	}
}

func TestExplicitEgressAllowListPreservesPrivateRouting(t *testing.T) {
	limiter.Init()
	n := testNode(t, "vless")
	startTestNodeWithConfig(t, n, testConf())
	listener := echoServer(t)
	exchange(t, clientBox(t, n, testUUID), listener.Addr())
}

func TestDefaultEgressRejectsResolvedPrivateAddress(t *testing.T) {
	limiter.Init()
	resolver, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	go func() {
		buffer := make([]byte, 4096)
		for {
			size, peer, err := resolver.ReadFrom(buffer)
			if err != nil {
				return
			}
			if size < 17 {
				continue
			}
			response := append([]byte(nil), buffer[:size]...)
			binary.BigEndian.PutUint16(response[2:4], 0x8180)
			binary.BigEndian.PutUint16(response[6:8], 0)
			if binary.BigEndian.Uint16(response[size-4:size-2]) == 1 {
				binary.BigEndian.PutUint16(response[6:8], 1)
				response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 127, 0, 0, 1)
			}
			_, _ = resolver.WriteTo(response, peer)
		}
	}()
	n := testNode(t, "vless")
	n.Common.SingBox = json.RawMessage(`{"dns":{"servers":[{"type":"udp","tag":"test","server":"127.0.0.1","server_port":` + strconv.Itoa(resolver.LocalAddr().(*net.UDPAddr).Port) + `}],"final":"test"}}`)
	startTestNodeWithConfig(t, n, conf.New())
	b := clientBox(t, n, testUUID)
	out, _ := b.Outbound().Outbound("proxy")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(net.JoinHostPort("private.audit.invalid", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))))
	if err == nil {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write([]byte("guard"))
		_, err = conn.Read(make([]byte, 5))
	}
	if err == nil {
		t.Fatal("resolved private destination was accepted")
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if accepted, err := listener.Accept(); err == nil {
		accepted.Close()
		t.Fatal("resolved private destination was reached")
	}
}

type changingPacketTarget struct {
	N.PacketConn
	targets []M.Socksaddr
}

func (c *changingPacketTarget) ReadPacket(*buf.Buffer) (M.Socksaddr, error) {
	target := c.targets[0]
	c.targets = c.targets[1:]
	return target, nil
}

type egressTestDNS struct {
	adapter.DNSRouter
	addresses []netip.Addr
}

func (d *egressTestDNS) Lookup(context.Context, string, adapter.DNSQueryOptions) ([]netip.Addr, error) {
	return d.addresses, nil
}

func TestUDPGuardChecksEveryDestination(t *testing.T) {
	guard, err := newEgressTracker([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"127.0.0.2:53", "[::1]:53", "[::ffff:127.0.0.2]:53", "169.254.169.254:80", "100.100.100.200:80", "168.63.129.16:80"} {
		conn := &egressPacketConn{PacketConn: &changingPacketTarget{targets: []M.Socksaddr{M.ParseSocksaddr("127.0.0.1:53"), M.ParseSocksaddr(target)}}, guard: guard}
		buffer := buf.New()
		if _, err := conn.ReadPacket(buffer); err != nil {
			t.Fatal("explicit allow failed", err)
		}
		if _, err := conn.ReadPacket(buffer); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("second target bypassed policy: %s: %v", target, err)
		}
		buffer.Release()
	}
}

func TestUDPDomainIsPinnedToTheCheckedAddress(t *testing.T) {
	guard, _ := newEgressTracker(nil)
	resolver := &egressTestDNS{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	conn := &egressPacketConn{PacketConn: &changingPacketTarget{targets: []M.Socksaddr{M.ParseSocksaddr("audit.invalid:53"), M.ParseSocksaddr("audit.invalid:53")}}, guard: guard, resolver: resolver, ctx: context.Background()}
	buffer := buf.New()
	defer buffer.Release()
	target, err := conn.ReadPacket(buffer)
	if err != nil || target != M.ParseSocksaddr("8.8.8.8:53") {
		t.Fatalf("unchecked DNS destination: %v %v", target, err)
	}
	resolver.addresses = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if _, err := conn.ReadPacket(buffer); !errors.Is(err, os.ErrPermission) {
		t.Fatal("DNS rebinding bypassed policy", err)
	}
}
