//go:build !race

// Upstream sing-vmess Vision uses unsafe TLS field access and shares TLS filter
// state between Read and Write. Run this interoperability test without -race;
// keep the remaining backend tests under the race detector.
package core

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	sj "github.com/sagernet/sing/common/json"
	"io"
	"net"
	"strconv"
	"testing"
)

func TestVLESSRealityVision(t *testing.T) {
	limiter.Init()
	for _, shared := range []bool{false, true} {
		name := "independent"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			cert := testCertificate(t)
			pair, err := tls.X509KeyPair([]byte(cert.TlsCert), []byte(cert.TlsKey))
			if err != nil {
				t.Fatal(err)
			}
			handshake, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
			if err != nil {
				t.Fatal(err)
			}
			defer handshake.Close()
			go func() {
				for {
					c, e := handshake.Accept()
					if e != nil {
						return
					}
					go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
				}
			}()
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			n := testNode(t, "vless")
			n.Security = panel.Reality
			n.Common.Flow = "xtls-rprx-vision"
			n.Common.TlsSettings = panel.TlsSettings{ServerName: "localhost", Dest: "127.0.0.1", ServerPort: strconv.Itoa(handshake.Addr().(*net.TCPAddr).Port), PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), ShortId: "0123456789abcdef"}
			if shared {
				n.SharedListener = "reality"
				n.Tag = "panel-a"
			}
			v := startTestNode(t, n)
			var peerCore *V2Core
			if shared {
				peer := *n
				peer.Tag = "panel-b"
				peerCore = New(testConf())
				peerCore.UseSharedListeners(v.shared)
				t.Cleanup(func() { peerCore.Close(); limiter.DeleteLimiter(peer.Tag) })
				users := []panel.UserInfo{{Id: 42, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}
				limiter.AddLimiter("vless", peer.Tag, users, nil)
				if err := peerCore.AddNode(peer.Tag, &peer); err != nil {
					t.Fatal(err)
				}
				if err := peerCore.SetUsers(peer.Tag, users); err != nil {
					t.Fatal(err)
				}
			}
			out := object{"type": "vless", "tag": "proxy", "server": "127.0.0.1", "server_port": n.Common.ServerPort, "uuid": testUUID, "flow": "xtls-rprx-vision", "tls": object{"enabled": true, "server_name": "localhost", "utls": object{"enabled": true, "fingerprint": "chrome"}, "reality": object{"enabled": true, "public_key": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "short_id": "0123456789abcdef"}}}
			raw, _ := json.Marshal(object{"log": object{"disabled": true}, "outbounds": []any{out}})
			ctx := boxContext(context.Background())
			var opts option.Options
			if err = sj.UnmarshalContext(ctx, raw, &opts); err != nil {
				t.Fatal(err)
			}
			b, err := box.New(box.Options{Context: ctx, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if err = b.Start(); err != nil {
				t.Fatal(err)
			}
			echo := echoServer(t)
			exchange(t, b, echo.Addr())
			if shared {
				out["uuid"] = "a8389d79-d204-4f90-b8f0-cb4878a14933"
				raw, _ = json.Marshal(object{"log": object{"disabled": true}, "outbounds": []any{out}})
				var peerOptions option.Options
				if err = sj.UnmarshalContext(ctx, raw, &peerOptions); err != nil {
					t.Fatal(err)
				}
				client, err := box.New(box.Options{Context: ctx, Options: peerOptions})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				if err = client.Start(); err != nil {
					t.Fatal(err)
				}
				conn := persistent(t, client, echo.Addr())
				awaitTraffic(t, peerCore, "panel-b", 21)
				if err = v.Close(); err != nil {
					t.Fatal(err)
				}
				if err = roundTrip(conn); err != nil {
					t.Fatal("Reality peer interrupted", err)
				}
				exchange(t, client, echo.Addr())
			}
		})
	}
}
