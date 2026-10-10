package core

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"testing"
)

func TestRealityAndRoutesConfiguration(t *testing.T) {
	n := testNode(t, "vless")
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n.Security = panel.Reality
	n.Common.TlsSettings = panel.TlsSettings{ServerName: "example.com", ServerPort: "443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), ShortId: "0123456789abcdef"}
	dns := "1.1.1.1"
	out := `{"type":"socks","tag":"relay","server":"127.0.0.1","server_port":1080}`
	n.Common.Routes = []panel.Route{{Id: 1, Action: "block", Match: []string{"domain:blocked.example"}}, {Id: 2, Action: "block_ip", Match: []string{"192.0.2.1"}}, {Id: 3, Action: "block_port", Match: []string{"25", "1000-2000"}}, {Id: 4, Action: "dns", Match: []string{"full:example.com"}, ActionValue: &dns}, {Id: 5, Action: "route", Match: []string{"domain:relay.example"}, ActionValue: &out}}
	opts, err := buildOptions(n, []panel.UserInfo{{Id: 1, Uuid: testUUID}}, conf.New())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty options")
	}
	n.Common.TlsSettings.Mldsa65Seed = "unsupported"
	if _, err = buildOptions(n, nil, conf.New()); err == nil {
		t.Fatal("silently ignored ML-DSA")
	}
}
func TestBillingAndOnlineThresholdsAreIndependent(t *testing.T) {
	tr := newTrafficTracker()
	tr.account("n", 1, 100, 200)
	if got := tr.takeTraffic("n", 1); len(got) != 0 {
		t.Fatal("billing threshold ignored")
	}
	if got := tr.takeActivity("n")[1]; got != 300 {
		t.Fatalf("online traffic lost: %d", got)
	}
	if got := tr.takeTraffic("n", 0); len(got) != 1 || got[0].Upload != 100 {
		t.Fatal("online reporting consumed billing data")
	}
}
