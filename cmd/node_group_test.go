package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/lenaanderson5566-ops/v2box/node"
)

type groupPanel struct {
	server   *httptest.Server
	port     atomic.Int64
	fail     atomic.Bool
	blocked  atomic.Bool
	requests atomic.Int64
	failed   atomic.Int64
	applied  atomic.Int64
	uuid     string
	key      string
}

func groupFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
func newGroupPanel(t *testing.T, uuid, key string, tlsSettings ...map[string]any) *groupPanel {
	t.Helper()
	fixture := &groupPanel{uuid: uuid, key: key}
	fixture.port.Store(int64(groupFreePort(t)))
	fixture.server = reloadTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixture.key || r.Header.Get("X-Node-ID") != "1" {
			t.Error("panel credentials crossed node ownership")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/api/v10/server/configuration" && r.Method == "GET" {
			fixture.requests.Add(1)
			if fixture.blocked.Load() {
				<-r.Context().Done()
				return
			}
			if fixture.fail.Load() {
				w.WriteHeader(503)
				return
			}
			port := fixture.port.Load()
			if len(tlsSettings) > 0 {
				w.Header().Set("ETag", fmt.Sprintf(`W/"%064x"`, port))
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"protocol": "anytls", "tls": 1, "tlsSettings": tlsSettings[0], "listenAddress": "127.0.0.1", "listenPort": port, "intervals": map[string]any{"pushInterval": 3600, "pullInterval": 3600}}})
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`W/"%064x"`, port))
			fmt.Fprintf(w, `{"data":{"protocol":"vless","tls":0,"listenAddress":"127.0.0.1","listenPort":%d,"intervals":{"pushInterval":3600,"pullInterval":3600}}}`, port)
			return
		}
		switch r.URL.Path {
		case "/api/v10/server/users":
			fmt.Fprintf(w, `{"data":[{"id":1,"uuid":%q,"speedLimit":0,"deviceLimit":0}]}`, fixture.uuid)
		case "/api/v10/server/capabilities":
			var body struct{ Configuration struct{ State string } }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Configuration.State == "applied" {
				fixture.applied.Add(1)
			}
			if body.Configuration.State == "failed" {
				fixture.failed.Add(1)
			}
			io.WriteString(w, `{"data":{"accepted":true}}`)
		default:
			io.WriteString(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}
func (p *groupPanel) entry(t *testing.T) conf.NodeConfig {
	zero := 0
	return conf.NodeConfig{APIHost: p.server.URL, NodeType: "v2box", NodeID: 1, Key: p.key, RetryCount: &zero, Timeout: 1, TrafficSpoolDirectory: t.TempDir(), CertificateDirectory: filepath.Join(t.TempDir(), "certs")}
}
func waitGroup(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node did not reach expected state")
}
func groupListening(port int64) bool {
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	connection.Close()
	return true
}
func groupStream(t *testing.T, fixture *groupPanel, destination int) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", fixture.port.Load()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	uuid, _ := hex.DecodeString(strings.ReplaceAll(fixture.uuid, "-", ""))
	header := append([]byte{0}, uuid...)
	header = append(header, 0, 1, byte(destination>>8), byte(destination), 1, 127, 0, 0, 1)
	if _, err = connection.Write(append(header, byte('x'))); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 3)
	if _, err = io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	if response[2] != 'x' {
		t.Fatal("proxy stream did not reach echo server")
	}
	return connection
}
func groupPing(t *testing.T, connection net.Conn) {
	t.Helper()
	connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write([]byte("still-active")); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, len("still-active"))
	if _, err := io.ReadFull(connection, result); err != nil {
		t.Fatalf("unrelated backend disconnected: %v", err)
	}
	if string(result) != "still-active" {
		t.Fatal("payload corrupted")
	}
}

func TestNodeGroupOriginMigrationAndRollbackPreserveOtherPanelStream(t *testing.T) {
	limiter.Init()
	a := newGroupPanel(t, "b831381d-6324-4d53-ad4f-8cda48b30811", "existing-a-key")
	b := newGroupPanel(t, "c831381d-6324-4d53-ad4f-8cda48b30822", "existing-b-key")
	var oldApplied, newApplied atomic.Int64
	observe := func(handler http.Handler, applied *atomic.Int64) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v10/server/capabilities" {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				var receipt struct{ Configuration struct{ State string } }
				json.Unmarshal(body, &receipt)
				if receipt.Configuration.State == "applied" {
					applied.Add(1)
				}
			}
			handler.ServeHTTP(w, r)
		})
	}
	originalHandler := a.server.Config.Handler
	a.server.Config.Handler = observe(originalHandler, &oldApplied)
	alias := reloadTestPanel(t, observe(originalHandler, &newApplied))
	defer alias.Close()
	config := conf.New()
	config.OutboundAllowCIDRs = []string{"127.0.0.0/8"}
	config.NodeConfigs = []conf.NodeConfig{a.entry(t), b.entry(t)}
	certPath := filepath.Join(config.NodeConfigs[0].CertificateDirectory, "existing.pem")
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("existing-certificate-preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	original := config.NodeConfigs[0]
	group := newNodeGroup(context.Background())
	group.retry = 30 * time.Millisecond
	defer group.Close()
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return oldApplied.Load() > 0 && groupListening(b.port.Load()) })
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
	stream := groupStream(t, b, echo.Addr().(*net.TCPAddr).Port)
	defer stream.Close()
	config.NodeConfigs[0].APIHost = alias.URL
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return newApplied.Load() > 0 })
	groupPing(t, stream)
	migratedStream := groupStream(t, a, echo.Addr().(*net.TCPAddr).Port)
	migratedStream.Close()
	before := oldApplied.Load()
	config.NodeConfigs[0] = original
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return oldApplied.Load() > before })
	groupPing(t, stream)
	if content, err := os.ReadFile(certPath); err != nil || string(content) != "existing-certificate-preserved" {
		t.Fatal("origin migration altered certificate storage")
	}
	if config.NodeConfigs[0].Key != original.Key {
		t.Fatal("origin migration rotated credentials")
	}
}
func TestNodeGroupIndependentStartupReloadRecoveryAndRemoval(t *testing.T) {
	limiter.Init()
	a := newGroupPanel(t, "b831381d-6324-4d53-ad4f-8cda48b30811", "fixture-a")
	b := newGroupPanel(t, "c831381d-6324-4d53-ad4f-8cda48b30822", "fixture-b")
	a.blocked.Store(true)
	config := conf.New()
	config.OutboundAllowCIDRs = []string{"127.0.0.0/8"}
	config.NodeConfigs = []conf.NodeConfig{a.entry(t), b.entry(t)}
	group := newNodeGroup(context.Background())
	group.retry = 30 * time.Millisecond
	defer group.Close()
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return groupListening(b.port.Load()) })
	if groupListening(a.port.Load()) {
		t.Fatal("unavailable backend unexpectedly started")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			connection, e := echo.Accept()
			if e != nil {
				return
			}
			go func() { defer connection.Close(); io.Copy(connection, connection) }()
		}
	}()
	stream := groupStream(t, b, echo.Addr().(*net.TCPAddr).Port)
	defer stream.Close()
	a.blocked.Store(false)
	waitGroup(t, func() bool { return groupListening(a.port.Load()) })
	groupPing(t, stream)
	key, _ := conf.NodeIdentity(config.NodeConfigs[0])
	old := a.port.Load()
	a.port.Store(int64(groupFreePort(t)))
	group.workers[key].reload <- struct{}{}
	waitGroup(t, func() bool { return groupListening(a.port.Load()) && !groupListening(old) })
	groupPing(t, stream)
	// A bind failure restores A without closing any of B's accepted streams.
	old = a.port.Load()
	a.port.Store(b.port.Load())
	before := a.failed.Load()
	group.workers[key].reload <- struct{}{}
	waitGroup(t, func() bool { return a.failed.Load() > before })
	waitGroup(t, func() bool { return groupListening(old) })
	groupPing(t, stream)
	a.port.Store(int64(groupFreePort(t)))
	waitGroup(t, func() bool { return groupListening(a.port.Load()) && !groupListening(old) })
	groupPing(t, stream)
	// Outage during a later reload retains A and never blocks B's own update.
	a.fail.Store(true)
	group.workers[key].reload <- struct{}{}
	bKey, _ := conf.NodeIdentity(config.NodeConfigs[1])
	oldB := b.port.Load()
	b.port.Store(int64(groupFreePort(t)))
	group.workers[bKey].reload <- struct{}{}
	waitGroup(t, func() bool { return groupListening(b.port.Load()) && !groupListening(oldB) })
	if !groupListening(a.port.Load()) {
		t.Fatal("panel outage closed its existing listener")
	}
	// File reordering and removal act on identity, not array position.
	config.NodeConfigs = []conf.NodeConfig{config.NodeConfigs[1], config.NodeConfigs[0]}
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	config.NodeConfigs = config.NodeConfigs[:1]
	if err := group.Apply(config); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return !groupListening(a.port.Load()) })
	if !groupListening(b.port.Load()) {
		t.Fatal("removing A stopped B")
	}
}
func TestNodeGroupRejectsDuplicateIdentityBeforeMutation(t *testing.T) {
	fixture := newGroupPanel(t, "b831381d-6324-4d53-ad4f-8cda48b30811", "fixture")
	entry := fixture.entry(t)
	config := conf.New()
	config.NodeConfigs = []conf.NodeConfig{entry, entry}
	config.NodeConfigs[1].APIHost += "/"
	group := newNodeGroup(context.Background())
	defer group.Close()
	if err := group.Apply(config); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if len(group.workers) != 0 {
		t.Fatal("invalid update partially applied")
	}
}

func TestNodeGroupRejectsSharedCertificatePaths(t *testing.T) {
	group := newNodeGroup(context.Background())
	a := &nodeWorker{group: group, claims: map[string]bool{}}
	b := &nodeWorker{group: group, claims: map[string]bool{}}
	candidate := &node.Node{NodeInfos: []*panel.NodeInfo{{Security: panel.Tls, Common: &panel.CommonNode{CertInfo: &panel.CertInfo{CertMode: "file", CertFile: filepath.Join(t.TempDir(), "shared.cer"), KeyFile: filepath.Join(t.TempDir(), "shared.key")}}}}}
	finish, err := a.claimResources(candidate)
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	if _, err := b.claimResources(candidate); err == nil {
		t.Fatal("shared certificate files accepted")
	}
	a.releaseClaims()
	finish, err = b.claimResources(candidate)
	if err != nil {
		t.Fatal(err)
	}
	finish(false)
	if len(group.resources) != 0 {
		t.Fatal("rejected candidate leaked certificate ownership")
	}
}

func TestNodeGroupSharesQuotasWithinBackendOnly(t *testing.T) {
	group := newNodeGroup(context.Background())
	config := conf.New()
	config.NodeConfigs = []conf.NodeConfig{{APIHost: "https://a.example.test", NodeID: 1}}
	a := group.backendAccounting(config)
	config.NodeConfigs[0].NodeID = 2
	b := group.backendAccounting(config)
	config.NodeConfigs[0].APIHost = "https://b.example.test"
	other := group.backendAccounting(config)
	budget := &panel.TrafficBudget{ID: 7, LimitBytes: 1000, UsedBytes: 10, CountingMode: "both", PeriodStart: "period"}
	a.SetTrafficBudget("a", budget)
	b.SetTrafficBudget("b", budget)
	other.SetTrafficBudget("other", budget)
	updated := *budget
	updated.UsedBytes = 200
	a.SetTrafficBudget("a", &updated)
	if got := b.TrafficBudgetSnapshot("b"); got == nil || got.UsedBytes != 200 {
		t.Fatal("same-backend physical-server quota stopped aggregating")
	}
	if got := other.TrafficBudgetSnapshot("other"); got == nil || got.UsedBytes != 10 {
		t.Fatal("another backend's same server ID shared a quota")
	}
}

func TestNodeGroupSharedCertificateClaimsRetainOtherOwner(t *testing.T) {
	group := newNodeGroup(context.Background())
	a := &nodeWorker{group: group, claims: map[string]bool{}}
	b := &nodeWorker{group: group, claims: map[string]bool{}}
	outsider := &nodeWorker{group: group, claims: map[string]bool{}}
	candidate := &node.Node{NodeInfos: []*panel.NodeInfo{{SharedListener: "shared", Security: panel.Tls, Common: &panel.CommonNode{CertInfo: &panel.CertInfo{CertMode: "file", CertFile: filepath.Join(t.TempDir(), "shared.cer"), KeyFile: filepath.Join(t.TempDir(), "shared.key")}}}}}
	for _, worker := range []*nodeWorker{a, b} {
		finish, err := worker.claimResources(candidate)
		if err != nil {
			t.Fatal(err)
		}
		finish(true)
	}
	a.releaseClaims()
	unshared := *candidate.NodeInfos[0]
	unshared.SharedListener = ""
	if _, err := outsider.claimResources(&node.Node{NodeInfos: []*panel.NodeInfo{&unshared}}); err == nil {
		t.Fatal("peer lost certificate ownership after first owner left")
	}
	b.releaseClaims()
	if len(group.resources) != 0 {
		t.Fatal("shared certificate claims leaked")
	}
}

func TestInvalidSharedOptInPreservesExistingListener(t *testing.T) {
	limiter.Init()
	panel := newGroupPanel(t, "b831381d-6324-4d53-ad4f-8cda48b30811", "panel")
	cfg := conf.New()
	cfg.NodeConfigs = []conf.NodeConfig{panel.entry(t)}
	group := newNodeGroup(context.Background())
	defer group.Close()
	if err := group.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, func() bool { return panel.applied.Load() > 0 })
	cfg.NodeConfigs[0].SharedListener = "../invalid"
	if err := group.Apply(cfg); err == nil {
		t.Fatal("invalid sharing selection accepted")
	}
	if !groupListening(panel.port.Load()) {
		t.Fatal("invalid sharing selection stopped an existing node")
	}
}

func TestSharedCertificateClaimsRejectDifferentDomainWriters(t *testing.T) {
	group := newNodeGroup(context.Background())
	first := &nodeWorker{group: group, claims: map[string]bool{}}
	second := &nodeWorker{group: group, claims: map[string]bool{}}
	source := &panel.CertInfo{CertMode: "http", CertDomain: "first.example.test", CertFile: filepath.Join(t.TempDir(), "cert.pem"), KeyFile: filepath.Join(t.TempDir(), "key.pem")}
	candidate := &node.Node{NodeInfos: []*panel.NodeInfo{{SharedListener: "shared", Security: panel.Tls, Common: &panel.CommonNode{CertInfo: source}}}}
	finish, err := first.claimResources(candidate)
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	defer first.releaseClaims()
	another := *source
	another.CertDomain = "second.example.test"
	conflicting := &node.Node{NodeInfos: []*panel.NodeInfo{{SharedListener: "shared", Security: panel.Tls, Common: &panel.CommonNode{CertInfo: &another}}}}
	if _, err := second.claimResources(conflicting); err == nil {
		t.Fatal("different domains were allowed to overwrite one certificate pair")
	}
}
