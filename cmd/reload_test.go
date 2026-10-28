package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/node"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reload(config string, nodes **node.Node, v2core **core.V2Core) error {
	return reloadContext(context.Background(), config, nodes, v2core)
}

func reloadContext(parent context.Context, config string, nodes **node.Node, v2core **core.V2Core) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	newConf := conf.New()
	if err := newConf.LoadFromPath(config); err != nil {
		return &reloadRejected{err}
	}
	return reloadConfiguration(ctx, newConf, nodes, v2core)
}

var reloadTestRoots sync.Once

func TestReloadInvalidConfigurationHashPreservesLiveListener(t *testing.T) {
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.Addr().(*net.TCPAddr).Port
	socket.Close()
	var invalid atomic.Bool
	var slow atomic.Bool
	s := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() && r.URL.Path == "/api/v10/server/configuration" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v10/server/configuration":
			if !invalid.Load() {
				w.Header().Set("ETag", `W/"`+strings.Repeat("a", 64)+`"`)
			}
			fmt.Fprintf(w, `{"data":{"protocol":"vless","tls":0,"listenAddress":"127.0.0.1","listenPort":%d,"intervals":{"pushInterval":3600,"pullInterval":3600}}}`, port)
		case "/api/v10/server/capabilities":
			w.Write([]byte(`{"data":{"accepted":true}}`))
		case "/api/v10/server/users":
			w.Write([]byte(`{"data":[{"id":1,"uuid":"b831381d-6324-4d53-ad4f-8cda48b30811","speedLimit":0,"deviceLimit":0}]}`))
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer s.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "NodeType": "v2box", "ApiKey": "fixture", "RetryCount": 0}}})
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(configPath); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(cfg.NodeConfigs)
	if err != nil {
		t.Fatal(err)
	}
	v := core.New(cfg)
	v.ReloadCh = make(chan struct{}, 1)
	if err := v.Start(n.NodeInfos); err != nil {
		t.Fatal(err)
	}
	if err := n.Start(cfg.NodeConfigs, v); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close(); _ = v.Close() }()
	oldCore, oldNodes := v, n
	slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	err = reloadContext(ctx, configPath, &n, &v)
	cancel()
	var timeoutRejected *reloadRejected
	if !errors.As(err, &timeoutRejected) || time.Since(started) > 2*time.Second || n != oldNodes || v != oldCore {
		t.Fatalf("slow panel disrupted runtime or deadline: %v", err)
	}
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if dialErr != nil {
		t.Fatalf("listener lost after timeout: %v", dialErr)
	}
	conn.Close()
	slow.Store(false)
	if err = reload(configPath, &n, &v); err != nil {
		t.Fatalf("reload did not recover after timeout: %v", err)
	}
	oldCore, oldNodes = v, n
	invalid.Store(true)
	err = reload(configPath, &n, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "configuration hash") || v != oldCore || n != oldNodes {
		t.Fatalf("bad version should be rejected without replacing live runtime: %v", err)
	}
	conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("listener closed on invalid version: %v", err)
	}
	conn.Close()
}

func reloadTestPanel(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	reloadTestRoots.Do(func() { roots := x509.NewCertPool(); roots.AddCert(s.Certificate()); x509.SetFallbackRoots(roots) })
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	return s
}

func TestReloadRejectsBeforeClosingCore(t *testing.T) {
	s := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"protocol":"vless","tls":0,"server_port":443,"base_config":{"push_interval":60,"pull_interval":60},"sing_box":{"route":{"final":"missing"}}}`))
	}))
	defer s.Close()
	name := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test", "ApiVersion": "legacy"}}})
	if err := os.WriteFile(name, body, 0600); err != nil {
		t.Fatal(err)
	}
	nodes := &node.Node{}
	v := core.New(conf.New())
	defer v.Close()
	old := v
	err := reload(name, &nodes, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) || v != old {
		t.Fatalf("invalid config not rejected before shutdown: %v", err)
	}
	if err := v.AddNode("still-open", &panel.NodeInfo{Tag: "still-open", Type: "vless", Common: &panel.CommonNode{ServerPort: 12345, ListenIP: "127.0.0.1", Network: "tcp"}}); err != nil {
		t.Fatalf("old core closed: %v", err)
	}
}

func TestReloadPanelAuthenticationFailurePreservesCore(t *testing.T) {
	for _, failure := range []string{"users-unavailable", "invalid-users", "alive-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			s := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/server/config" {
					w.Write([]byte(`{"protocol":"vless","tls":0,"server_port":12345,"base_config":{"push_interval":60,"pull_interval":60}}`))
				} else if r.URL.Path == "/api/v1/server/UniProxy/user" {
					if failure == "users-unavailable" {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if failure == "invalid-users" {
						w.Write([]byte(`{"users":[{"id":1,"uuid":"invalid"}]}`))
					} else {
						w.Write([]byte(`{"users":[{"id":1,"uuid":"b831381d-6324-4d53-ad4f-8cda48b30811"}]}`))
					}
				} else {
					if failure == "alive-unavailable" {
						w.WriteHeader(503)
						return
					}
					w.Write([]byte(`{"alive":{}}`))
				}
			}))
			defer s.Close()
			name := filepath.Join(t.TempDir(), "config.json")
			body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test", "ApiVersion": "legacy", "RetryCount": 0}}})
			if err := os.WriteFile(name, body, 0600); err != nil {
				t.Fatal(err)
			}
			nodes := &node.Node{}
			v := core.New(conf.New())
			defer v.Close()
			old := v
			err := reload(name, &nodes, &v)
			var rejected *reloadRejected
			if !errors.As(err, &rejected) || v != old {
				t.Fatalf("replacement was not rejected safely: %v", err)
			}
			if err := v.AddNode("still-open", &panel.NodeInfo{Tag: "still-open", Type: "vless", Common: &panel.CommonNode{ServerPort: 12345, ListenIP: "127.0.0.1", Network: "tcp"}}); err != nil {
				t.Fatalf("old core closed: %v", err)
			}
		})
	}
}

func TestReloadStartupFailureRestoresLiveListener(t *testing.T) {
	oldSocket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldPort := oldSocket.Addr().(*net.TCPAddr).Port
	oldSocket.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var port atomic.Int64
	port.Store(int64(oldPort))
	s := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			fmt.Fprintf(w, `{"protocol":"vless","tls":0,"listen_ip":"127.0.0.1","server_port":%d,"base_config":{"push_interval":3600,"pull_interval":3600}}`, port.Load())
		case "/api/v1/server/UniProxy/user":
			w.Write([]byte(`{"users":[{"id":1,"uuid":"b831381d-6324-4d53-ad4f-8cda48b30811"}]}`))
		default:
			w.Write([]byte(`{"alive":{}}`))
		}
	}))
	defer s.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test", "ApiVersion": "legacy", "RetryCount": 0}}})
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(configPath); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(cfg.NodeConfigs)
	if err != nil {
		t.Fatal(err)
	}
	v := core.New(cfg)
	v.ReloadCh = make(chan struct{}, 1)
	if err := v.Start(n.NodeInfos); err != nil {
		t.Fatal(err)
	}
	if err := n.Start(cfg.NodeConfigs, v); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close(); _ = v.Close() }()
	port.Store(int64(occupied.Addr().(*net.TCPAddr).Port))
	err = reload(configPath, &n, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) {
		t.Fatalf("startup failure did not rollback: %v", err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", oldPort), time.Second)
	if err != nil {
		t.Fatalf("old listener not restored: %v", err)
	}
	conn.Close()
	if n.NodeInfos[0].Common.ServerPort != oldPort {
		t.Fatal("candidate became active after startup failure")
	}
}

// Exercise the real panel decoder, certificate preparation, controller and Box
// replacement. A successful API receipt alone cannot satisfy these assertions.
func TestReloadAppliesProtocolPortDomainAndRelay(t *testing.T) {
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	freePort := func() int {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	dir := t.TempDir()
	var mu sync.Mutex
	current := map[string]any{}
	setConfig := func(protocol, domain, token string, port int) {
		mu.Lock()
		defer mu.Unlock()
		current = map[string]any{"protocol": protocol, "tls": 1, "listenAddress": "127.0.0.1", "listenPort": port, "network": "tcp", "tlsSettings": map[string]any{"certMode": "self", "serverName": domain, "certFile": filepath.Join(dir, domain+".pem"), "keyFile": filepath.Join(dir, domain+".key"), "provider": "cloudflare", "dnsEnv": "CF_DNS_API_TOKEN=" + token}, "intervals": map[string]any{"pushInterval": 3600, "pullInterval": 3600}}
	}
	port := freePort()
	setConfig("vless", "direct.example.test", "first-fixture", port)
	panelServer := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v10/server/configuration":
			mu.Lock()
			defer mu.Unlock()
			raw, _ := json.Marshal(current)
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			w.Header().Set("ETag", `"`+hash+`"`)
			json.NewEncoder(w).Encode(map[string]any{"data": current, "meta": map[string]any{"configurationHash": hash}})
		case "/api/v10/server/users":
			fmt.Fprintf(w, `{"data":[{"id":1,"uuid":%q,"speedLimit":0,"deviceLimit":0}]}`, uuid)
		case "/api/v10/server/capabilities":
			w.Write([]byte(`{"data":{"accepted":true}}`))
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer panelServer.Close()
	configPath := filepath.Join(dir, "config.json")
	body, _ := json.Marshal(map[string]any{"OutboundAllowCIDRs": []string{"127.0.0.0/8", "::1/128"}, "Nodes": []any{map[string]any{"ApiHost": panelServer.URL, "NodeID": 1, "NodeType": "v2box", "ApiKey": "fixture", "RetryCount": 0}}})
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(configPath); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(cfg.NodeConfigs)
	if err != nil {
		t.Fatal(err)
	}
	v := core.New(cfg)
	v.ReloadCh = make(chan struct{}, 1)
	if err = v.Start(n.NodeInfos); err != nil {
		t.Fatal(err)
	}
	if err = n.Start(cfg.NodeConfigs, v); err != nil {
		t.Fatal(err)
	}
	defer func() { n.Close(); v.Close() }()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, e := echo.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	probe := func(address, domain, protocol string) {
		certPEM, e := os.ReadFile(filepath.Join(dir, domain+".pem"))
		if e != nil {
			t.Fatal(e)
		}
		block, _ := pem.Decode(certPEM)
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			t.Fatal(e)
		}
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		conn, e := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: domain, MinVersion: tls.VersionTLS12})
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		destination := echo.Addr().(*net.TCPAddr).Port
		var header []byte
		if protocol == "vless" {
			id, _ := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
			header = append([]byte{0}, id...)
			header = append(header, 0, 1, byte(destination>>8), byte(destination), 1, 127, 0, 0, 1)
		} else {
			sum := sha256.Sum224([]byte(uuid))
			header = []byte(hex.EncodeToString(sum[:]) + "\r\n")
			header = append(header, 1, 1, 127, 0, 0, 1, byte(destination>>8), byte(destination), 13, 10)
		}
		payload := []byte("configuration-really-applied")
		if _, e = conn.Write(append(header, payload...)); e != nil {
			t.Fatal(e)
		}
		if protocol == "vless" {
			response := make([]byte, 2)
			if _, e = io.ReadFull(conn, response); e != nil {
				t.Fatal(e)
			}
		}
		got := make([]byte, len(payload))
		if _, e = io.ReadFull(conn, got); e != nil {
			t.Fatal(e)
		}
		if string(got) != string(payload) {
			t.Fatal("proxy payload mismatch")
		}
	}
	address := func(p int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) }
	probe(address(port), "direct.example.test", "vless")
	oldPort := port
	port = freePort()
	setConfig("trojan", "entry.example.test", "rotated-fixture", port)
	if err = reload(configPath, &n, &v); err != nil {
		t.Fatal(err)
	}
	if c, e := net.DialTimeout("tcp", address(oldPort), time.Second); e == nil {
		c.Close()
		t.Fatal("old port still listening")
	}
	if n.NodeInfos[0].Common.CertInfo.DNSEnv["CF_DNS_API_TOKEN"] != "" {
		t.Fatal("non-DNS certificate should not import DNS secrets")
	}
	probe(address(port), "entry.example.test", "trojan")
	// A real stream forwarder emulates a four-layer ingress; TLS ends at V2Box.
	ingress, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ingress.Close()
	upstream := address(port)
	go func() {
		for {
			down, e := ingress.Accept()
			if e != nil {
				return
			}
			go func() {
				defer down.Close()
				up, e := net.DialTimeout("tcp", upstream, time.Second)
				if e != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{})
				go func() { io.Copy(up, down); up.Close(); close(done) }()
				io.Copy(down, up)
				down.Close()
				<-done
			}()
		}
	}()
	probe(ingress.Addr().String(), "entry.example.test", "trojan")
	setConfig("vless", "direct.example.test", "first-fixture", port)
	if err = reload(configPath, &n, &v); err != nil {
		t.Fatal(err)
	}
	probe(address(port), "direct.example.test", "vless")
	// Use the local panel CA's trusted certificate to exercise DNS-mode
	// reloads without contacting a public CA or Cloudflare.
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: panelServer.TLS.Certificates[0].Certificate[0]})
	keyDER, e := x509.MarshalPKCS8PrivateKey(panelServer.TLS.Certificates[0].PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "example.com.pem"), certPEM, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "example.com.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); e != nil {
		t.Fatal(e)
	}
	for _, token := range []string{"first-fixture", "rotated-fixture"} {
		setConfig("trojan", "example.com", token, port)
		mu.Lock()
		current["tlsSettings"].(map[string]any)["certMode"] = "dns"
		mu.Unlock()
		if err = reload(configPath, &n, &v); err != nil {
			t.Fatal(err)
		}
		applied := n.NodeInfos[0].Common.CertInfo
		if applied.Provider != "cloudflare" || applied.DNSEnv["CF_DNS_API_TOKEN"] != token {
			t.Fatal("new certificate credential not applied to active controller")
		}
		probe(address(port), "example.com", "trojan")
	}
	mu.Lock()
	current["tlsSettings"].(map[string]any)["serverName"] = "wrong.example.test"
	mu.Unlock()
	oldCore := v
	err = reload(configPath, &n, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) || v != oldCore {
		t.Fatalf("wrong certificate replaced active service: %v", err)
	}
	probe(address(port), "example.com", "trojan")
}
