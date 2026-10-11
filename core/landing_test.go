package core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/quic-go/http3"
)

func TestLandingPagesAndProxyAuthentication(t *testing.T) {
	limiter.Init()
	cert := testCertificate(t)
	echo := echoServer(t)
	for _, protocol := range []string{"naive", "hysteria2"} {
		for _, custom := range []bool{false, true} {
			t.Run(protocol+strconv.FormatBool(custom), func(t *testing.T) {
				n := testNode(t, protocol)
				n.Security = panel.Tls
				n.Common.CertInfo = cert
				n.Common.LandingPage.Enabled = true
				want := "Welcome"
				if custom {
					n.Common.LandingPage.Directory = t.TempDir()
					want = "custom public website"
					if err := os.WriteFile(filepath.Join(n.Common.LandingPage.Directory, "index.html"), []byte("<html>"+want+"</html>"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				v := startTestNode(t, n)
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM([]byte(cert.TlsCert))
				tlsConfig := &tls.Config{RootCAs: roots, ServerName: "localhost"}
				var transport http.RoundTripper
				if protocol == "hysteria2" {
					tr := &http3.Transport{TLSClientConfig: tlsConfig}
					defer tr.Close()
					transport = tr
				} else {
					tr := &http.Transport{TLSClientConfig: tlsConfig} // H1 browser.
					defer tr.CloseIdleConnections()
					transport = tr
				}
				client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
				url := "https://127.0.0.1:" + strconv.Itoa(n.Common.ServerPort) + "/"
				for _, method := range []string{"GET", "HEAD"} {
					req, _ := http.NewRequest(method, url, nil)
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
						t.Fatalf("invalid page: %d %v %v", resp.StatusCode, resp.Header, err)
					}
					if method == "GET" && !strings.Contains(string(body), want) {
						t.Fatal("wrong page")
					}
					if method == "HEAD" && len(body) != 0 {
						t.Fatal("HEAD body")
					}
				}
				if traffic, _ := v.GetUserTrafficSlice(n.Tag, 0); len(traffic) != 0 {
					t.Fatal("public page charged as user traffic")
				}
				if protocol == "naive" {
					h2 := naiveClient(t, n)
					req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
					resp, err := h2.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 || resp.ProtoMajor != 2 {
						t.Fatal("H2 page unavailable")
					}
					if s, err := naiveConnect(h2, "wrong", echo.Addr().String()); err == nil {
						s.Close()
						t.Fatal("unauthenticated CONNECT accepted")
					}
					s, err := naiveConnect(h2, testUUID, echo.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					if err := naiveEcho(s, []byte("proxy still authenticated")); err != nil {
						t.Fatal(err)
					}
				} else {
					assertDenied(t, clientBox(t, n, "wrong"), echo.Addr())
					exchange(t, clientBox(t, n, testUUID), echo.Addr())
				}
				traffic, _ := v.GetUserTrafficSlice(n.Tag, 0)
				if len(traffic) != 1 || traffic[0].UID != 42 || traffic[0].Upload == 0 {
					t.Fatalf("proxy accounting lost: %+v", traffic)
				}
				if err := v.SetUsers(n.Tag, nil); err != nil {
					t.Fatal(err)
				}
				resp, err := client.Get(url)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatal("empty user table disabled public page")
				}
				if protocol == "naive" {
					if s, err := naiveConnect(naiveClient(t, n), testUUID, echo.Addr().String()); err == nil {
						s.Close()
						t.Fatal("empty table allowed CONNECT")
					}
				} else {
					assertDenied(t, clientBox(t, n, testUUID), echo.Addr())
				}
			})
		}
	}
}

func TestLandingPageDisabled(t *testing.T) {
	limiter.Init()
	for _, protocol := range []string{"naive", "hysteria2"} {
		t.Run(protocol, func(t *testing.T) {
			n := testNode(t, protocol)
			n.Security = panel.Tls
			n.Common.CertInfo = testCertificate(t)
			startTestNode(t, n)
			var transport http.RoundTripper
			if protocol == "naive" {
				transport = naiveClient(t, n)
			} else {
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM([]byte(n.Common.CertInfo.TlsCert))
				tr := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}}
				defer tr.Close()
				transport = tr
			}
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(n.Common.ServerPort) + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Fatal("page enabled by default")
			}
		})
	}
}

func TestLandingPageProtocolValidation(t *testing.T) {
	for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "anytls", "tuic"} {
		n := testNode(t, protocol)
		n.Common.LandingPage = conf.LandingPage{Enabled: true}
		if _, err := buildInbound(n, nil); err == nil {
			t.Fatalf("accepted landing page for %s", protocol)
		}
	}
	n := testNode(t, "hysteria2")
	n.Common.Obfs = "salamander"
	n.Common.LandingPage.Enabled = true
	if _, err := buildInbound(n, nil); err == nil {
		t.Fatal("obfuscated H3 page accepted")
	}
}
