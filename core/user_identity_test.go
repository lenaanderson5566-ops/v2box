package core

import (
	"context"
	"net"
	"strings"
	"testing"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestRevokedGenerationCannotReenter(t *testing.T) {
	limiter.Init()
	u := panel.UserInfo{Id: 42, Uuid: testUUID}
	limiter.AddLimiter("vless", "n", []panel.UserInfo{u}, nil)
	defer limiter.DeleteLimiter("n")
	v := New(conf.New())
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
