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
func newGroupPanel(t *testing.T, uuid, key string) *groupPanel {
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
