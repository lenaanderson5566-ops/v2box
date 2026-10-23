package cmd

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/node"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var reloadTestRoots sync.Once

func TestReloadInvalidConfigurationHashPreservesLiveListener(t *testing.T) {
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.Addr().(*net.TCPAddr).Port
	socket.Close()
	var invalid atomic.Bool
	s := reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	invalid.Store(true)
	err = reload(configPath, &n, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "configuration hash") || v != oldCore || n != oldNodes {
		t.Fatalf("bad version should be rejected without replacing live runtime: %v", err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
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
