package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	M "github.com/sagernet/sing/common/metadata"
	"io"
	"net"
	"testing"
	"time"
)

func startTestNode(t *testing.T, n *panel.NodeInfo) *V2Core {
	t.Helper()
	users := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
	limiter.AddLimiter(n.Type, n.Tag, users, nil)
	t.Cleanup(func() { limiter.DeleteLimiter(n.Tag) })
	cfg := conf.New()
	cfg.LogConfig.Level = "error"
	v := New(cfg)
	t.Cleanup(func() { v.Close() })
	if err := v.AddNode(n.Tag, n); err != nil {
		t.Fatal(err)
	}
	if err := v.SetUsers(n.Tag, users); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestTransports(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	for _, network := range []string{"ws", "grpc", "httpupgrade"} {
		t.Run(network, func(t *testing.T) {
			n := testNode(t, "vless")
			n.Common.Network = network
			n.Common.NetworkSettings = json.RawMessage(`{"path":"/proxy"}`)
			if network == "grpc" {
				n.Common.NetworkSettings = json.RawMessage(`{"serviceName":"proxy"}`)
			}
			startTestNode(t, n)
			exchange(t, clientBox(t, n, testUUID), echo.Addr())
		})
	}
}
func TestSS2022(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	n := testNode(t, "shadowsocks")
	n.Common.Cipher = "2022-blake3-aes-128-gcm"
	n.Common.ServerKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	startTestNode(t, n)
	exchange(t, clientBox(t, n, testUUID), echo.Addr())
}
func exchangeUDP(t *testing.T, b *box.Box, destination net.Addr) {
	t.Helper()
	out, _ := b.Outbound().Outbound("proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := out.ListenPacket(ctx, M.ParseSocksaddr(destination.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	payload := []byte("udp-accounting-echo")
	if _, err = c.WriteTo(payload, destination); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4096)
	n, _, err := c.ReadFrom(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], payload) {
		t.Fatal("UDP mismatch")
	}
}
func TestUDPAccounting(t *testing.T) {
	limiter.Init()
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		p := make([]byte, 65535)
		for {
			n, addr, e := echo.ReadFrom(p)
			if e != nil {
				return
			}
			_, _ = echo.WriteTo(p[:n], addr)
		}
	}()
	cert := testCertificate(t)
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			n := testNode(t, protocol)
			if protocol == "shadowsocks" {
				n.Common.Cipher = "aes-128-gcm"
			}
			if protocol == "trojan" || protocol == "hysteria2" || protocol == "tuic" {
				n.Security = panel.Tls
				n.Common.CertInfo = cert
			}
			v := startTestNode(t, n)
			exchangeUDP(t, clientBox(t, n, testUUID), echo.LocalAddr())
			traffic := awaitTraffic(t, v, n.Tag, int64(len("udp-accounting-echo")))
			if len(traffic) != 1 || traffic[0].Upload == 0 || traffic[0].Download == 0 {
				t.Fatalf("UDP traffic missing: %+v", traffic)
			}
		})
	}
}
func TestUserReplacementAndRejectedUpdate(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	n := testNode(t, "vless")
	v := startTestNode(t, n)
	oldClient := clientBox(t, n, testUUID)
	exchange(t, oldClient, echo.Addr())
	bad := []panel.UserInfo{{Id: 99, Uuid: "invalid UUID"}}
	if err := v.SetUsers(n.Tag, bad); err == nil {
		t.Fatal("accepted invalid VLESS UUID")
	}
	exchange(t, oldClient, echo.Addr())
	newUUID := "d8389d79-d204-4f90-b8f0-cb4878a14933"
	users := []panel.UserInfo{{Id: 99, Uuid: newUUID}}
	l, _ := limiter.GetLimiter(n.Tag)
	l.UpdateUser(n.Tag, users, []panel.UserInfo{{Id: 42, Uuid: testUUID}}, nil)
	if err := v.SetUsers(n.Tag, users); err != nil {
		t.Fatal(err)
	}
	exchange(t, clientBox(t, n, newUUID), echo.Addr())
	out, _ := oldClient.Outbound().Outbound("proxy")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(echo.Addr().String()))
	if err == nil {
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		_, _ = c.Write([]byte("revoked"))
		p := make([]byte, 7)
		if _, e := io.ReadFull(c, p); e == nil {
			t.Fatal("revoked user still connects")
		}
	}
}
