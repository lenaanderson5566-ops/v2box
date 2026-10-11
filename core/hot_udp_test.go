package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	M "github.com/sagernet/sing/common/metadata"
)

func udpEcho(t *testing.T) net.PacketConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		p := make([]byte, 65535)
		for {
			n, a, e := c.ReadFrom(p)
			if e != nil {
				return
			}
			_, _ = c.WriteTo(p[:n], a)
		}
	}()
	return c
}
func packetClient(t *testing.T, b *box.Box, dst net.Addr) net.PacketConn {
	t.Helper()
	out, _ := b.Outbound().Outbound("proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := out.ListenPacket(ctx, M.ParseSocksaddr(dst.String()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func packetRoundTrip(c net.PacketConn, dst net.Addr, payload string) error {
	// QUIC-backed PacketConn implementations may not implement deadlines.
	// Bound the negative-authentication probe by closing its test session.
	done := make(chan struct{})
	timer := time.AfterFunc(2*time.Second, func() { _ = c.Close(); close(done) })
	defer func() {
		if !timer.Stop() {
			<-done
		}
	}()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	p := []byte(payload)
	if _, err := c.WriteTo(p, dst); err != nil {
		return err
	}
	got := make([]byte, 2048)
	n, _, err := c.ReadFrom(got)
	if err != nil {
		return err
	}
	if !bytes.Equal(got[:n], p) {
		return fmt.Errorf("UDP mismatch")
	}
	return nil
}
func TestHotUsersPreserveUDP(t *testing.T) {
	limiter.Init()
	echo := udpEcho(t)
	cert := testCertificate(t)
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "ss2022", "hysteria2", "tuic", "anytls"} {
		t.Run(protocol, func(t *testing.T) {
			kind := protocol
			if kind == "ss2022" {
				kind = "shadowsocks"
			}
			n := testNode(t, kind)
			if kind == "shadowsocks" {
				n.Common.Cipher = "aes-128-gcm"
			}
			if protocol == "ss2022" {
				n.Common.Cipher = "2022-blake3-aes-128-gcm"
				n.Common.ServerKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
			}
			if kind == "trojan" || kind == "hysteria2" || kind == "tuic" || kind == "anytls" {
				n.Security = panel.Tls
				n.Common.CertInfo = cert
			}
			v := startTestNode(t, n)
			a := panel.UserInfo{Id: 42, Uuid: testUUID}
			b := panel.UserInfo{Id: 1, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}
			l, _ := limiter.GetLimiter(n.Tag)
			apply := func(users []panel.UserInfo) {
				t.Helper()
				if err := v.SetUsersWithCommit(n.Tag, users, func() { l.UpdateUser(n.Tag, users, nil, nil) }); err != nil {
					t.Fatal(err)
				}
			}
			ca := packetClient(t, clientBox(t, n, a.Uuid), echo.LocalAddr())
			check := func(c net.PacketConn, p string) {
				t.Helper()
				if err := packetRoundTrip(c, echo.LocalAddr(), p); err != nil {
					t.Fatal(err)
				}
			}
			check(ca, "a-before")
			apply([]panel.UserInfo{a, b})
			check(ca, "a-after-add")
			cb := packetClient(t, clientBox(t, n, b.Uuid), echo.LocalAddr())
			check(cb, "b-before")
			// Receiving the echo does not guarantee its metered Write has returned.
			// Drain only after the pre-update packet's accounting has completed.
			deadline := time.Now().Add(2 * time.Second)
			var beforeUp, beforeDown int64
			for {
				traffic, _ := v.GetUserTrafficSlice(n.Tag, 0)
				for _, record := range traffic {
					if record.UID == b.Id {
						beforeUp += record.Upload
						beforeDown += record.Download
					}
				}
				if beforeUp >= int64(len("b-before")) && beforeDown >= int64(len("b-before")) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("pre-update UDP accounting incomplete")
				}
				time.Sleep(time.Millisecond)
			}
			apply([]panel.UserInfo{a})
			check(ca, "a-after-remove")
			if packetRoundTrip(cb, echo.LocalAddr(), "b-revoked") == nil {
				t.Fatal("revoked UDP user forwarded data")
			}
			traffic, _ := v.GetUserTrafficSlice(n.Tag, 0)
			for _, u := range traffic {
				if u.UID != a.Id {
					t.Fatalf("traffic attributed to wrong account: %+v", traffic)
				}
			}
			apply([]panel.UserInfo{a, b})
			check(ca, "a-after-readd")
			fresh := packetClient(t, clientBox(t, n, b.Uuid), echo.LocalAddr())
			check(fresh, "b-new-session")
		})
	}
}
