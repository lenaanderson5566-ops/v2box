package core

import (
	"encoding/base64"
	"strings"
	"testing"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
)

func TestSharedProtocolsIndependentPanels(t *testing.T) {
	limiter.Init()
	for _, protocol := range []string{"anytls", "vless", "vmess", "trojan", "shadowsocks", "ss2022", "ss2022-256", "hysteria2", "tuic", "vless-tls", "vmess-tls", "vless-ws", "vmess-grpc"} {
		t.Run(protocol, func(t *testing.T) {
			echo := echoServer(t)
			kind := strings.Split(protocol, "-")[0]
			if strings.HasPrefix(protocol, "ss2022") {
				kind = "shadowsocks"
			}
			a := testNode(t, kind)
			if kind == "shadowsocks" {
				a.Common.Cipher = "aes-128-gcm"
			}
			if protocol == "ss2022" {
				a.Common.Cipher = "2022-blake3-aes-128-gcm"
				a.Common.ServerKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
			}
			if protocol == "ss2022-256" {
				a.Common.Cipher = "2022-blake3-aes-256-gcm"
				a.Common.ServerKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
			}
			if strings.HasSuffix(protocol, "-tls") {
				a.Security = panel.Tls
			}
			if protocol == "vless-ws" {
				a.Common.Network = "ws"
				a.Common.NetworkSettings = []byte(`{"path":"/shared"}`)
			}
			if protocol == "vmess-grpc" {
				a.Common.Network = "grpc"
				a.Common.NetworkSettings = []byte(`{"serviceName":"shared"}`)
			}
			a.Tag = "[https://panel-a.example]-node:1"
			if kind == "trojan" || kind == "anytls" || kind == "hysteria2" || kind == "tuic" {
				a.Security = panel.Tls
			}
			a.SharedListener = "public-443"
			a.Common.CertInfo = testCertificate(t)
			b := *a
			b.Tag = "[https://panel-b.example]-v2box-anytls:1"
			hub := NewSharedListeners()
			va, vb := New(testConf()), New(testConf())
			va.UseSharedListeners(hub)
			vb.UseSharedListeners(hub)
			t.Cleanup(func() { va.Close(); vb.Close(); limiter.DeleteLimiter(a.Tag); limiter.DeleteLimiter(b.Tag) })
			ua := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
			ub := []panel.UserInfo{{Id: 42, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}
			for _, item := range []struct {
				core  *V2Core
				info  *panel.NodeInfo
				users []panel.UserInfo
			}{{va, a, ua}, {vb, &b, ub}} {
				limiter.AddLimiter(kind, item.info.Tag, item.users, nil)
				if err := item.core.AddNode(item.info.Tag, item.info); err != nil {
					t.Fatal(err)
				}
				if err := item.core.SetUsers(item.info.Tag, item.users); err != nil {
					t.Fatal(err)
				}
			}
			va.SetTargetPolicy(a.Tag, panel.TargetPolicy{Enabled: false})
			vb.SetTargetPolicy(b.Tag, panel.TargetPolicy{Enabled: true, Version: "shared", LeaseSeconds: 120})
			if len(hub.groups) != 1 || len(hub.groups[a.SharedListener].members) != 2 {
				t.Fatal("nodes did not share one listener")
			}
			ca, cb := clientBox(t, a, testUUID), clientBox(t, &b, ub[0].Uuid)
			sa, sb := persistent(t, ca, echo.Addr()), persistent(t, cb, echo.Addr())
			// Discard A's handshake traffic, then exercise only B: identical panel user
			// IDs must never cause A to receive B's bytes or activity.
			awaitTraffic(t, va, a.Tag, int64(len("persistent-connection")))
			awaitTraffic(t, vb, b.Tag, int64(len("persistent-connection")))
			va.TakeOnlineActivity(a.Tag)
			vb.TakeOnlineActivity(b.Tag)
			if err := roundTrip(sb); err != nil {
				t.Fatal(err)
			}
			gotB := awaitTraffic(t, vb, b.Tag, int64(len("persistent-connection")))
			if got, _ := va.GetUserTrafficSlice(a.Tag, 0); len(got) != 0 {
				t.Fatal("other panel received traffic", got)
			}
			if got := gotB; len(got) != 1 || got[0].UID != 42 || got[0].Upload == 0 || got[0].Download == 0 {
				t.Fatal("shared traffic attributed incorrectly", got)
			}
			if len(va.TakeOnlineActivity(a.Tag)) != 0 || vb.TakeOnlineActivity(b.Tag)[42] == 0 {
				t.Fatal("activity crossed panels")
			}
			if va.TargetBatch(a.Tag) != nil {
				t.Fatal("disabled panel collected peer targets")
			}
			targets := vb.TargetBatch(b.Tag)
			if targets == nil || len(targets.Records) != 1 || targets.Records[0].UserID != 42 || targets.Records[0].Target != "127.0.0.1" {
				t.Fatal("shared targets attributed incorrectly", targets)
			}
			// A conflicting update is rejected before changing the live authentication
			// table or invoking the limiter commit.
			committed := false
			if err := vb.SetUsersWithCommit(b.Tag, ua, func() { committed = true }); err == nil || committed {
				t.Fatal("duplicate credential committed")
			}
			if err := roundTrip(sa); err != nil {
				t.Fatal(err)
			}
			if err := roundTrip(sb); err != nil {
				t.Fatal(err)
			}
			udp := udpEcho(t)
			pa := packetClient(t, ca, udp.LocalAddr())
			pb := packetClient(t, cb, udp.LocalAddr())
			if err := packetRoundTrip(pa, udp.LocalAddr(), "panel-a-udp"); err != nil {
				t.Fatal(err)
			}
			if err := packetRoundTrip(pb, udp.LocalAddr(), "panel-b-udp"); err != nil {
				t.Fatal(err)
			}
			if err := va.SetUsers(a.Tag, nil); err != nil {
				t.Fatal(err)
			}
			assertDenied(t, ca, echo.Addr())
			if err := roundTrip(sb); err != nil {
				t.Fatal("other panel stream interrupted", err)
			}
			if packetRoundTrip(pa, udp.LocalAddr(), "revoked-a") == nil {
				t.Fatal("revoked UDP session remains active")
			}
			if err := packetRoundTrip(pb, udp.LocalAddr(), "b-after-a-revoke"); err != nil {
				t.Fatal(err)
			}
			if err := va.Close(); err != nil {
				t.Fatal(err)
			}
			if err := roundTrip(sb); err != nil {
				t.Fatal("listener owner removal interrupted peer", err)
			}
			if err := packetRoundTrip(pb, udp.LocalAddr(), "b-after-owner-close"); err != nil {
				t.Fatal(err)
			}
			va.SetTrafficBudget(a.Tag, &panel.TrafficBudget{ID: 1, LimitBytes: 1, UsedBytes: 1, CountingMode: "both", PeriodStart: "period"})
			vb.SetTrafficBudget(b.Tag, &panel.TrafficBudget{ID: 1, LimitBytes: 100000, CountingMode: "both", PeriodStart: "period"})
			if err := roundTrip(sb); err != nil {
				t.Fatal("same-ID budget from another panel blocked B", err)
			}
			// Enforce B's local quota without allowing any other panel to supply or
			// reset its counter. Exhaustion rejects B's subsequent streams.
			vb.SetTrafficBudget(b.Tag, &panel.TrafficBudget{ID: 1, LimitBytes: 1, UsedBytes: 1, CountingMode: "both", PeriodStart: "period"})
			assertDenied(t, cb, echo.Addr())
		})
	}
}

func TestSharedAnyTLSRejectsIncompatibleSettings(t *testing.T) {
	limiter.Init()
	a := testNode(t, "anytls")
	a.Security = panel.Tls
	a.SharedListener = "shared"
	a.Common.CertInfo = testCertificate(t)
	v := startTestNode(t, a)
	peer := *a
	peer.Tag = "peer"
	peerCommon := *a.Common
	peer.Common = &peerCommon
	peer.Common.PaddingScheme = []string{"stop=1", "0=30-30", "1=100-100"}
	if err := v.ValidateNodeUsers(&peer, []panel.UserInfo{{Id: 1, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}); err == nil {
		t.Fatal("incompatible shared padding accepted")
	}
	peer.Common.PaddingScheme = nil
	cert := *a.Common.CertInfo
	peer.Common.CertInfo = &cert
	cert.CertDomain = "other.example"
	if err := v.ValidateNodeUsers(&peer, nil); err == nil {
		t.Fatal("incompatible TLS identity accepted")
	}
	cert.CertDomain = "localhost"
	cert.CertMode = "http"
	if err := v.ValidateNodeUsers(&peer, nil); err == nil {
		t.Fatal("shared ACME writer accepted")
	}
	cert.CertMode = "remote"
	peer.Type = "trojan"
	if err := v.ValidateNodeUsers(&peer, []panel.UserInfo{{Id: 42, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}); err == nil {
		t.Fatal("different valid protocols accepted in one shared group")
	}
	echo := echoServer(t)
	exchange(t, clientBox(t, a, testUUID), echo.Addr())
	peer.Type = "unsupported"
	if err := v.ValidateNodeUsers(&peer, nil); err == nil {
		t.Fatal("unsupported shared protocol accepted")
	}
}

func TestSharedNaiveIndependentPanels(t *testing.T) {
	limiter.Init()
	a := testNode(t, "naive")
	a.Security = panel.Tls
	a.Common.CertInfo = testCertificate(t)
	a.SharedListener = "shared-naive"
	a.Tag = "panel-a"
	b := *a
	b.Tag = "panel-b"
	va, vb := New(testConf()), New(testConf())
	hub := NewSharedListeners()
	va.UseSharedListeners(hub)
	vb.UseSharedListeners(hub)
	ua := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
	ub := []panel.UserInfo{{Id: 42, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}
	for _, item := range []struct {
		v     *V2Core
		n     *panel.NodeInfo
		users []panel.UserInfo
	}{{va, a, ua}, {vb, &b, ub}} {
		limiter.AddLimiter("naive", item.n.Tag, item.users, nil)
		t.Cleanup(func() { item.v.Close(); limiter.DeleteLimiter(item.n.Tag) })
		if err := item.v.AddNode(item.n.Tag, item.n); err != nil {
			t.Fatal(err)
		}
		if err := item.v.SetUsers(item.n.Tag, item.users); err != nil {
			t.Fatal(err)
		}
	}
	echo := echoServer(t)
	ca, cb := naiveClient(t, a), naiveClient(t, &b)
	sa, err := naiveConnect(ca, testUUID, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	sb, err := naiveConnect(cb, ub[0].Uuid, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	check := func(stream *naiveStream) {
		t.Helper()
		if err := naiveEcho(stream, []byte("persistent-connection")); err != nil {
			t.Fatal(err)
		}
	}
	check(sa)
	check(sb)
	awaitTraffic(t, va, a.Tag, 21)
	awaitTraffic(t, vb, b.Tag, 21)
	check(sb)
	awaitTraffic(t, vb, b.Tag, 21)
	if got, _ := va.GetUserTrafficSlice(a.Tag, 0); len(got) != 0 {
		t.Fatal("Naive accounting crossed panels")
	}
	if err := vb.SetUsers(b.Tag, ua); err == nil {
		t.Fatal("duplicate Naive credentials accepted")
	}
	check(sa)
	check(sb)
	if err := va.SetUsers(a.Tag, nil); err != nil {
		t.Fatal(err)
	}
	if stream, err := naiveConnect(ca, testUUID, echo.Addr().String()); err == nil {
		stream.Close()
		t.Fatal("revoked Naive credentials admitted")
	}
	check(sb)
	if err := va.Close(); err != nil {
		t.Fatal(err)
	}
	check(sb)
	stream, err := naiveConnect(cb, ub[0].Uuid, echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	check(stream)
}

func TestSharedEffectiveCredentialConflicts(t *testing.T) {
	limiter.Init()
	for _, protocol := range []string{"vless", "vmess", "tuic", "ss2022"} {
		t.Run(protocol, func(t *testing.T) {
			kind := protocol
			if kind == "ss2022" {
				kind = "shadowsocks"
			}
			a := testNode(t, kind)
			a.Tag = "a"
			a.SharedListener = "credentials"
			if kind == "tuic" {
				a.Security = panel.Tls
				a.Common.CertInfo = testCertificate(t)
			}
			if protocol == "ss2022" {
				a.Common.Cipher = "2022-blake3-aes-128-gcm"
				a.Common.ServerKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
			}
			v := startTestNode(t, a)
			b := *a
			b.Tag = "b"
			credential := strings.ToUpper(testUUID)
			if protocol == "ss2022" {
				credential = testUUID[:16] + "-different-suffix"
			}
			if err := v.ValidateNodeUsers(&b, []panel.UserInfo{{Id: 99, Uuid: credential}}); err == nil {
				t.Fatal("equivalent credential accepted")
			}
		})
	}
}

func TestSharedListenerRejectsInvalidLogLevelWithoutDisconnecting(t *testing.T) {
	limiter.Init()
	node := testNode(t, "anytls")
	node.Security = panel.Tls
	node.Common.CertInfo = testCertificate(t)
	node.SharedListener = "logging"
	live := startTestNode(t, node)
	echo := echoServer(t)
	client := clientBox(t, node, testUUID)
	stream := persistent(t, client, echo.Addr())
	cfg := testConf()
	cfg.LogConfig.Level = "verbose"
	candidate := New(cfg)
	candidate.InheritSharedListeners(live)
	if err := candidate.ValidateNodeUsers(node, []panel.UserInfo{{Id: 42, Uuid: testUUID}}); err == nil {
		t.Fatal("invalid shared log level accepted before replacement")
	}
	if err := roundTrip(stream); err != nil {
		t.Fatal("log validation interrupted live stream", err)
	}
}
