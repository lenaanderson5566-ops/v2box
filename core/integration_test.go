package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	M "github.com/sagernet/sing/common/metadata"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func startTestNode(t *testing.T, n *panel.NodeInfo) *V2Core {
	return startTestNodeWithConfig(t, n, testConf())
}
func startTestNodeWithConfig(t *testing.T, n *panel.NodeInfo, cfg *conf.Conf) *V2Core {
	t.Helper()
	users := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
	limiter.AddLimiter(n.Type, n.Tag, users, nil)
	t.Cleanup(func() { limiter.DeleteLimiter(n.Tag) })
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
	t.Cleanup(cancel)
	c, err := out.ListenPacket(ctx, M.ParseSocksaddr(destination.String()))
	if err != nil {
		t.Fatal(err)
	}
	// Keep the session alive until accounting assertions finish. Closing the
	// client can retire target caches before the server publishes its last write.
	t.Cleanup(func() { c.Close() })
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
			v.SetTargetPolicy(n.Tag, panel.TargetPolicy{Enabled: true, Version: "udp", LeaseSeconds: 120})
			exchangeUDP(t, clientBox(t, n, testUUID), echo.LocalAddr())
			traffic := awaitTraffic(t, v, n.Tag, int64(len("udp-accounting-echo")))
			if len(traffic) != 1 || traffic[0].Upload == 0 || traffic[0].Download == 0 {
				t.Fatalf("UDP traffic missing: %+v", traffic)
			}
			// Traffic and target counters publish separately; a valid frozen batch
			// may contain only the upload. Acknowledge and sum subsequent batches.
			var up, down int64
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && (up < traffic[0].Upload || down < traffic[0].Download) {
				if targets := v.TargetBatch(n.Tag); targets != nil {
					if len(targets.Records) != 1 || targets.Records[0].Target != "127.0.0.1" || targets.Records[0].UserID != 42 {
						t.Fatalf("UDP target mismatch: %+v", targets)
					}
					up += targets.Records[0].UploadBytes
					down += targets.Records[0].DownloadBytes
					v.AckTargets(n.Tag, targets.BatchID, true)
				}
				if up < traffic[0].Upload || down < traffic[0].Download {
					time.Sleep(time.Millisecond)
				}
			}
			if up != traffic[0].Upload || down != traffic[0].Download {
				t.Fatalf("UDP target totals %d/%d differ from traffic %+v", up, down, traffic)
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

// Server-first protocols (for example SSH banners) must not need an application
// payload from the client just to start forwarding a direct connection.
func TestDirectServerFirstConnections(t *testing.T) {
	limiter.Init()
	cert := testCertificate(t)
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic", "anytls"} {
		t.Run(protocol, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			defer func() { listener.Close(); <-done }()
			go func() {
				defer close(done)
				c, err := listener.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				c.Write([]byte("server-ready"))
			}()
			n := testNode(t, protocol)
			if protocol == "shadowsocks" {
				n.Common.Cipher = "aes-128-gcm"
			}
			if protocol == "trojan" || protocol == "hysteria2" || protocol == "tuic" || protocol == "anytls" {
				n.Security = panel.Tls
				n.Common.CertInfo = cert
			}
			startTestNode(t, n)
			client := clientBox(t, n, testUUID)
			out, _ := client.Outbound().Outbound("proxy")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := time.Now()
			c, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(listener.Addr().String()))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			// Flush a lazy protocol handshake, without sending application data.
			if _, err = c.Write(nil); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len("server-ready"))
			if _, err = io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if string(got) != "server-ready" {
				t.Fatalf("corrupt greeting %q", got)
			}
			t.Logf("direct first byte including handshake: %s", time.Since(started))
		})
	}
}

func TestDomainBlockStillInspectsIPConnections(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	n := testNode(t, "vless")
	n.Common.Routes = []panel.Route{{Id: 1, Action: "block", Match: []string{"full:blocked.example"}}}
	startTestNode(t, n)
	client := clientBox(t, n, testUUID)
	out, _ := client.Outbound().Outbound("proxy")
	for _, host := range []string{"allowed.example", "blocked.example"} {
		t.Run(host, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(echo.Addr().String()))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			payload := []byte("GET / HTTP/1.1\r\nHost: " + host + "\r\n\r\n")
			if _, err = c.Write(payload); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(payload))
			_, err = io.ReadFull(c, got)
			if host == "allowed.example" {
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("allowed domain failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("domain block bypassed for IP destination")
			}
		})
	}
}

// Exercise the actual sing-box file watcher and TLS handshakes, including
// repeated atomic replacement and preservation of established proxy streams.
func TestTLSCertificateRotationPreservesOtherNodesAndExistingConnections(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	makeNode := func(tag string) *panel.NodeInfo {
		n := testNode(t, "vless")
		n.Tag = tag
		n.Security = panel.Tls
		cert := testCertificate(t)
		cert.CertMode = "file"
		cert.CertFile = filepath.Join(t.TempDir(), "cert.pem")
		cert.KeyFile = filepath.Join(filepath.Dir(cert.CertFile), "key.pem")
		if err := os.WriteFile(cert.CertFile, []byte(cert.TlsCert), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cert.KeyFile, []byte(cert.TlsKey), 0600); err != nil {
			t.Fatal(err)
		}
		n.Common.CertInfo = cert
		startTestNode(t, n)
		return n
	}
	a, b := makeNode("certificate-a"), makeNode("certificate-b")
	old := *a.Common.CertInfo
	next := testCertificate(t)
	bCert, _ := os.ReadFile(b.Common.CertInfo.CertFile)
	bKey, _ := os.ReadFile(b.Common.CertInfo.KeyFile)
	roots := x509.NewCertPool()
	for _, cert := range []string{old.TlsCert, next.TlsCert, b.Common.CertInfo.TlsCert} {
		if !roots.AppendCertsFromPEM([]byte(cert)) {
			t.Fatal("invalid fixture root")
		}
	}
	peek := func(n *panel.NodeInfo) ([]byte, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(n.Common.ServerPort)), &tls.Config{RootCAs: roots, ServerName: "localhost"})
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].Raw, nil
	}
	connect := func(n *panel.NodeInfo) net.Conn {
		box := clientBox(t, n, testUUID)
		out, _ := box.Outbound().Outbound("proxy")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(echo.Addr().String()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	streamA, streamB := connect(a), connect(b)
	ping := func(conn net.Conn) {
		t.Helper()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		payload := []byte("certificate-rotation-long-stream")
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("proxy stream corrupted")
		}
	}
	ping(streamA)
	ping(streamB)
	replace := func(name, data string) {
		t.Helper()
		staged, err := os.CreateTemp(filepath.Dir(name), ".certificate-update-*")
		if err != nil {
			t.Fatal(err)
		}
		temp := staged.Name()
		defer os.Remove(temp)
		if err = staged.Chmod(0600); err != nil {
			staged.Close()
			t.Fatal(err)
		}
		if _, err = staged.WriteString(data); err != nil {
			staged.Close()
			t.Fatal(err)
		}
		if err = staged.Close(); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(temp, name); err != nil {
			t.Fatal(err)
		}
	}
	waitCertificate := func(expected string) {
		t.Helper()
		block, _ := pem.Decode([]byte(expected))
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			got, err := peek(a)
			if err == nil && bytes.Equal(got, block.Bytes) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("new certificate was not served without restarting")
	}
	replace(old.KeyFile, next.TlsKey)
	replace(old.CertFile, next.TlsCert)
	waitCertificate(next.TlsCert)
	ping(streamA)
	ping(streamB)
	// Invalid/partial replacement must retain the last usable in-memory pair.
	replace(old.KeyFile, "invalid fixture key")
	replace(old.CertFile, "invalid fixture certificate")
	nextBlock, _ := pem.Decode([]byte(next.TlsCert))
	for i := 0; i < 10; i++ {
		got, err := peek(a)
		if err != nil || !bytes.Equal(got, nextBlock.Bytes) {
			t.Fatalf("invalid files replaced the usable certificate: %v", err)
		}
		ping(streamA)
		ping(streamB)
		time.Sleep(50 * time.Millisecond)
	}
	replace(old.KeyFile, old.TlsKey)
	replace(old.CertFile, old.TlsCert)
	waitCertificate(old.TlsCert)
	ping(streamA)
	ping(streamB)
	got, err := peek(b)
	block, _ := pem.Decode(bCert)
	if err != nil || !bytes.Equal(got, block.Bytes) {
		t.Fatalf("other panel certificate changed: %v", err)
	}
	currentCert, _ := os.ReadFile(b.Common.CertInfo.CertFile)
	currentKey, _ := os.ReadFile(b.Common.CertInfo.KeyFile)
	if !bytes.Equal(currentCert, bCert) || !bytes.Equal(currentKey, bKey) {
		t.Fatal("other panel certificate files changed")
	}
}
