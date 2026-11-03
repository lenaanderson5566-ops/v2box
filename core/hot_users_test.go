package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	M "github.com/sagernet/sing/common/metadata"
)

func dialTest(b *box.Box, dst net.Addr) (net.Conn, error) {
	out, _ := b.Outbound().Outbound("proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return out.DialContext(ctx, "tcp", M.ParseSocksaddr(dst.String()))
}
func roundTrip(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	p := []byte("persistent-connection")
	if _, err := c.Write(p); err != nil {
		return err
	}
	got := make([]byte, len(p))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if !bytes.Equal(p, got) {
		return fmt.Errorf("echo mismatch")
	}
	return nil
}
func assertDenied(t *testing.T, b *box.Box, dst net.Addr) {
	t.Helper()
	c, err := dialTest(b, dst)
	if err != nil {
		return
	}
	defer c.Close()
	if roundTrip(c) == nil {
		t.Fatal("revoked credentials still forward traffic")
	}
}
func persistent(t *testing.T, b *box.Box, dst net.Addr) net.Conn {
	t.Helper()
	c, err := dialTest(b, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err = roundTrip(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHotUsersPreserveConnections(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
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
			server := v.nodes[n.Tag].server
			a := panel.UserInfo{Id: 42, Uuid: testUUID}
			// Insert before A in the sorted list to catch index-based identity bugs.
			b := panel.UserInfo{Id: 1, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}
			c := panel.UserInfo{Id: 1, Uuid: "c8389d79-d204-4f90-b8f0-cb4878a14933"}
			l, _ := limiter.GetLimiter(n.Tag)
			apply := func(users []panel.UserInfo) {
				t.Helper()
				if err := v.SetUsersWithCommit(n.Tag, users, func() { l.UpdateUser(n.Tag, users, nil, nil) }); err != nil {
					t.Fatal(err)
				}
				if v.nodes[n.Tag].server != server {
					t.Fatal("user update replaced node")
				}
			}
			clientA := clientBox(t, n, a.Uuid)
			connA := persistent(t, clientA, echo.Addr())
			checkA := func() {
				t.Helper()
				if err := roundTrip(connA); err != nil {
					t.Fatalf("unaffected connection interrupted: %v", err)
				}
			}
			apply([]panel.UserInfo{a, b})
			checkA()
			clientB := clientBox(t, n, b.Uuid)
			connB := persistent(t, clientB, echo.Addr())
			// A malformed update must preserve both existing users and their streams.
			committed := false
			if err := v.SetUsersWithCommit(n.Tag, []panel.UserInfo{a, {Id: 0, Uuid: b.Uuid}}, func() { committed = true }); err == nil {
				t.Fatal("invalid update accepted")
			}
			if committed {
				t.Fatal("failed update modified controller limits")
			}
			checkA()
			if err := roundTrip(connB); err != nil {
				t.Fatal(err)
			}
			apply([]panel.UserInfo{a, c})
			checkA()
			if roundTrip(connB) == nil {
				t.Fatal("changed UUID retained its existing connection")
			}
			assertDenied(t, clientB, echo.Addr())
			assertDenied(t, clientBox(t, n, b.Uuid), echo.Addr())
			clientC := clientBox(t, n, c.Uuid)
			connC := persistent(t, clientC, echo.Addr())
			apply([]panel.UserInfo{a})
			checkA()
			if roundTrip(connC) == nil {
				t.Fatal("deleted user retained its existing connection")
			}
			assertDenied(t, clientC, echo.Addr())
			apply([]panel.UserInfo{a, b})
			checkA()
			newB := persistent(t, clientBox(t, n, b.Uuid), echo.Addr())
			b.Id = 2
			apply([]panel.UserInfo{a, b})
			checkA()
			if roundTrip(newB) == nil {
				t.Fatal("reassigned UID retained previous account session")
			}
			exchange(t, clientBox(t, n, b.Uuid), echo.Addr())

			// Exercise authentication concurrently with repeated account additions/removals.
			errs := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 12; i++ {
					conn, err := dialTest(clientA, echo.Addr())
					if err == nil {
						err = roundTrip(conn)
						conn.Close()
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}()
			for i := 0; i < 12; i++ {
				apply([]panel.UserInfo{a, b})
				checkA()
				apply([]panel.UserInfo{a})
				checkA()
			}
			wg.Wait()
			select {
			case err := <-errs:
				t.Fatal(err)
			default:
			}
			apply(nil)
			if roundTrip(connA) == nil {
				t.Fatal("empty list retained active user")
			}
			assertDenied(t, clientA, echo.Addr())
			apply([]panel.UserInfo{a})
			exchange(t, clientBox(t, n, a.Uuid), echo.Addr())
		})
	}
}
