package core

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestTrafficSourceMustBeConcreteIP(t *testing.T) {
	limiter.Init()
	for _, test := range []struct {
		name, address, want string
	}{
		{"invalid", "", ""},
		{"unspecified-v4", "0.0.0.0", ""},
		{"unspecified-v6", "::", ""},
		{"mapped-unspecified", "::ffff:0.0.0.0", ""},
		{"v4", "127.0.0.2", "127.0.0.2"},
		{"mapped-v4", "::ffff:127.0.0.2", "127.0.0.2"},
		{"v6", "::1", "::1"},
		{"scoped-v6", "fe80::1%test-zone", "fe80::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tag := "source-" + test.name
			user := panel.UserInfo{Id: 42, Uuid: testUUID}
			limits := limiter.AddLimiter("tuic", tag, []panel.UserInfo{user}, nil)
			t.Cleanup(func() { limiter.DeleteLimiter(tag) })
			tracker := newTrafficTracker()
			tracker.setUsers(tag, []panel.UserInfo{user})
			metadata := adapter.InboundContext{Inbound: tag, User: testUUID, Destination: M.ParseSocksaddr("127.0.0.1:443")}
			if test.address != "" {
				metadata.Source = M.Socksaddr{Addr: netip.MustParseAddr(test.address), Port: 4321}
			}
			meter := tracker.newMeter(context.Background(), metadata)
			if meter != nil {
				defer meter.cancel()
			}
			if test.want == "" {
				if meter != nil {
					t.Fatal("invalid or unspecified source admitted as an online device")
				}
			} else if meter == nil || meter.ip != test.want {
				t.Fatal("concrete source IP was not normalized correctly")
			}
			devices, err := limits.GetOnlineDevice()
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" && len(*devices) != 0 {
				t.Fatal("invalid source leaked into online report")
			}
			if test.want != "" && (len(*devices) != 1 || (*devices)[0].IP != test.want) {
				t.Fatal("online report did not preserve normalized actual source")
			}
		})
	}
}

func TestRevokedGenerationCannotReenter(t *testing.T) {
	limiter.Init()
	u := panel.UserInfo{Id: 42, Uuid: testUUID}
	limiter.AddLimiter("vless", "n", []panel.UserInfo{u}, nil)
	defer limiter.DeleteLimiter("n")
	v := New(testConf())
	n := &nodeState{}
	old := v.identities(n, []panel.UserInfo{u})
	n.identities = old
	var identity string
	for name := range old {
		identity = name
	}
	tr := v.tracker
	if err := tr.updateUsers("n", old, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	metadata := adapter.InboundContext{Inbound: "n", User: identity, Source: M.ParseSocksaddr("127.0.0.1:1234")}
	pending := tr.newMeter(context.Background(), metadata)
	if pending == nil {
		t.Fatal("initial auth denied")
	}
	defer pending.cancel()
	tr.setUsers("n", nil)
	n.identities = nil
	replacement := v.identities(n, []panel.UserInfo{u})
	_ = tr.updateUsers("n", replacement, func() error { return nil })
	c, peer := net.Pipe()
	defer c.Close()
	defer peer.Close()
	if tr.register("n", c, pending) {
		t.Fatal("handshake begun before revocation registered afterward")
	}
	if m := tr.newMeter(context.Background(), metadata); m != nil {
		m.cancel()
		t.Fatal("old multiplex identity recovered after re-add")
	}
}

func TestDuplicateEffectiveCredentialsRejected(t *testing.T) {
	for _, kind := range []string{"vless", "vmess", "tuic"} {
		n := testNode(t, kind)
		_, err := buildInbound(n, []panel.UserInfo{{Id: 1, Uuid: testUUID}, {Id: 2, Uuid: strings.ToUpper(testUUID)}})
		if err == nil {
			t.Fatalf("%s accepted two spellings of one credential", kind)
		}
	}
	n := testNode(t, "shadowsocks")
	n.Common.Cipher = "2022-blake3-aes-128-gcm"
	_, err := buildInbound(n, []panel.UserInfo{{Id: 1, Uuid: testUUID}, {Id: 2, Uuid: testUUID[:16] + "different-suffix"}})
	if err == nil {
		t.Fatal("SS2022 accepted duplicate truncated key")
	}
}
