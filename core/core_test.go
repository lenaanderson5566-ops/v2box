package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"

	"github.com/sagernet/sing-box/option"
	sj "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"io"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"
)

func testConf() *conf.Conf {
	cfg := conf.New()
	cfg.OutboundAllowCIDRs = []string{"127.0.0.0/8", "::1/128"}
	return cfg
}

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func testNode(t *testing.T, protocol string) *panel.NodeInfo {
	t.Helper()
	// A free TCP port can still be reserved for UDP on Windows. Probe both.
	for attempt := 0; attempt < 64; attempt++ {
		candidate, err := rand.Int(rand.Reader, big.NewInt(35000))
		if err != nil {
			t.Fatal(err)
		}
		port := 10000 + int(candidate.Int64())
		u, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		l, err := net.Listen("tcp", u.LocalAddr().String())
		_ = u.Close()
		if err != nil {
			continue
		}
		_ = l.Close()
		return &panel.NodeInfo{Tag: protocol, Type: protocol, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: port, Network: "tcp"}}
	}
	t.Fatal("unable to allocate a free TCP/UDP test port")
	return nil
}
func testCertificate(t *testing.T) *panel.CertInfo {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &panel.CertInfo{CertMode: "remote", CertDomain: "localhost", TlsCert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), TlsKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
}
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l
}
func clientBox(t *testing.T, n *panel.NodeInfo, uuid string) *box.Box {
	t.Helper()
	out := object{"type": n.Type, "tag": "proxy", "server": "127.0.0.1", "server_port": n.Common.ServerPort}
	switch n.Type {
	case "vmess":
		out["uuid"] = uuid
		out["security"] = "auto"
	case "vless":
		out["uuid"] = uuid
	case "tuic":
		out["uuid"] = uuid
		out["password"] = uuid
	case "shadowsocks":
		out["method"] = n.Common.Cipher
		if n.Common.ServerKey != "" {
			size := 16
			if n.Common.Cipher == "2022-blake3-aes-256-gcm" {
				size = 32
			}
			out["password"] = n.Common.ServerKey + ":" + base64.StdEncoding.EncodeToString([]byte(uuid[:size]))
		} else {
			out["password"] = uuid
		}
	default:
		out["password"] = uuid
	}
	if n.Security == panel.Tls {
		out["tls"] = object{"enabled": true, "server_name": n.Common.CertInfo.CertDomain, "certificate": []string{n.Common.CertInfo.TlsCert}}
		if n.Type == "tuic" || n.Type == "hysteria2" {
			out["tls"].(object)["alpn"] = []string{"h3"}
		}
	}
	if n.Type == "vmess" || n.Type == "vless" || n.Type == "trojan" {
		transport, err := buildTransport(n.Common)
		if err != nil {
			t.Fatal(err)
		}
		if transport != nil {
			out["transport"] = transport
		}
	}
	raw, _ := json.Marshal(object{"log": object{"disabled": true}, "outbounds": []any{out}})
	ctx := boxContext(context.Background())
	var opts option.Options
	if err := sj.UnmarshalContext(ctx, raw, &opts); err != nil {
		t.Fatal(err)
	}
	b, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}
func exchange(t *testing.T, b *box.Box, destination net.Addr) {
	t.Helper()
	out, ok := b.Outbound().Outbound("proxy")
	if !ok {
		t.Fatal("missing proxy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(destination.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	payload := bytes.Repeat([]byte("sing-box-backend-echo"), 256)
	if _, err = c.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echo mismatch")
	}
}
func TestProtocolConnectionsAndAccounting(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	cert := testCertificate(t)
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic", "anytls"} {
		t.Run(protocol, func(t *testing.T) {
			n := testNode(t, protocol)
			if protocol == "shadowsocks" {
				n.Common.Cipher = "aes-128-gcm"
			}
			if protocol == "trojan" || protocol == "hysteria2" || protocol == "tuic" || protocol == "anytls" {
				n.Security = panel.Tls
				n.Common.CertInfo = cert
			}
			users := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
			limiter.AddLimiter(protocol, n.Tag, users, nil)
			t.Cleanup(func() { limiter.DeleteLimiter(n.Tag) })
			cfg := testConf()
			cfg.LogConfig.Level = "error"
			v := New(cfg)
			t.Cleanup(func() { v.Close() })
			if err := v.Start([]*panel.NodeInfo{n}); err != nil {
				t.Fatal(err)
			}
			if err := v.AddNode(n.Tag, n); err != nil {
				t.Fatal(err)
			}
			if err := v.SetUsers(n.Tag, users); err != nil {
				t.Fatal(err)
			}
			v.SetTargetPolicy(n.Tag, panel.TargetPolicy{Enabled: true, Version: "test", LeaseSeconds: 120})
			b := clientBox(t, n, testUUID)
			exchange(t, b, echo.Addr())
			traffic := awaitTraffic(t, v, n.Tag, int64(len("sing-box-backend-echo")*256))
			if len(traffic) != 1 || traffic[0].UID != 42 || traffic[0].Upload != int64(len("sing-box-backend-echo")*256) || traffic[0].Download != int64(len("sing-box-backend-echo")*256) {
				t.Fatalf("bad traffic: %+v", traffic)
			}
			targets := v.TargetBatch(n.Tag)
			if targets == nil || len(targets.Records) != 1 || targets.Records[0].Target != "127.0.0.1" || targets.Records[0].UploadBytes != traffic[0].Upload || targets.Records[0].DownloadBytes != traffic[0].Download {
				t.Fatalf("target accounting mismatch: %+v", targets)
			}
			v.RestoreUserTraffic(n.Tag, traffic)
			restored, _ := v.GetUserTrafficSlice(n.Tag, 0)
			if len(restored) != 1 || restored[0] != traffic[0] {
				t.Fatal("report retry lost traffic")
			}
			if err := v.SetUsers(n.Tag, nil); err != nil {
				t.Fatal(err)
			}
			assertDenied(t, b, echo.Addr())
		})
	}
}

// A client can receive a write before the server goroutine records its result.
// Accumulate drained reports until that write completes; never assume a sleep
// or the client's Read creates a synchronization barrier for server counters.
func awaitTraffic(t *testing.T, v *V2Core, tag string, expected int64) []panel.UserTraffic {
	t.Helper()
	totals := map[int]panel.UserTraffic{}
	deadline := time.Now().Add(2 * time.Second)
	for {
		batch, _ := v.GetUserTrafficSlice(tag, 0)
		for _, u := range batch {
			old := totals[u.UID]
			old.UID = u.UID
			old.Upload += u.Upload
			old.Download += u.Download
			totals[u.UID] = old
		}
		var up, down int64
		var result []panel.UserTraffic
		for _, u := range totals {
			up += u.Upload
			down += u.Download
			result = append(result, u)
		}
		if up >= expected && down >= expected {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatalf("accounting did not complete: %+v", result)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestRejectUnsupportedConfiguration(t *testing.T) {
	for _, network := range []string{"xhttp", "splithttp", "kcp"} {
		n := testNode(t, "vless")
		n.Common.Network = network
		if _, err := buildOptions(n, nil, conf.New()); err == nil {
			t.Fatalf("accepted %s", network)
		}
	}
}
func TestTrafficSurvivesUserRemoval(t *testing.T) {
	tr := newTrafficTracker()
	tr.setUsers("n", []panel.UserInfo{{Id: 1, Uuid: testUUID}})
	tr.account("n", 1, 100, 200)
	tr.setUsers("n", nil)
	got := tr.takeTraffic("n", 0)
	if len(got) != 1 || got[0].Upload != 100 || got[0].Download != 200 {
		t.Fatalf("lost removed user's traffic: %+v", got)
	}
}

func TestSetupContextDoesNotExpireLiveListener(t *testing.T) {
	v := New(testConf())
	defer v.Close()
	info := testNode(t, "vless")
	if err := v.AddNode(info.Tag, info); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := v.AddUsersContext(ctx, &AddUsersParams{Tag: info.Tag, Users: []panel.UserInfo{{Id: 1, Uuid: testUUID}}, NodeInfo: info})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(info.Common.ServerPort))
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("setup cancellation killed live listener: %v", err)
	}
	conn.Close()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := v.AddUsersContext(canceled, &AddUsersParams{Tag: info.Tag}); err == nil {
		t.Fatal("canceled startup accepted")
	}
	conn, err = net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("rejected startup killed existing listener: %v", err)
	}
	conn.Close()
}
