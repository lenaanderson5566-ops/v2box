package node

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/sagernet/sing-vmess/vless"
	M "github.com/sagernet/sing/common/metadata"
	"testing"
)

func TestCompareUserListIdentityAndLimits(t *testing.T) {
	old := []panel.UserInfo{{Id: 1, Uuid: "a"}, {Id: 2, Uuid: "b"}}
	next := []panel.UserInfo{{Id: 3, Uuid: "a"}, {Id: 2, Uuid: "b", SpeedLimit: 10}}
	deleted, added, modified := compareUserList(old, next)
	if len(deleted) != 1 || deleted[0].Id != 1 || len(added) != 1 || added[0].Id != 3 || len(modified) != 1 || modified[0].Id != 2 {
		t.Fatalf("bad diff: %+v %+v %+v", deleted, added, modified)
	}
}

func TestFinalPublicationSharesRenewalReceiptSender(t *testing.T) {
	received := make(chan configurationReceipt, 4)
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Configuration struct {
				Hash        string `json:"hash"`
				Fingerprint string `json:"certificateFingerprint"`
			} `json:"configuration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		received <- configurationReceipt{body.Configuration.Hash, body.Configuration.Fingerprint}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"accepted":true}}`)
	}))
	defer apiServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(apiServer.Certificate())
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
	api := func() *panel.Client {
		retries := 0
		client, err := panel.New(&conf.NodeConfig{APIHost: apiServer.URL, NodeID: 1, Key: "fixture-only", APIVersion: "v10", RetryCount: &retries})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.CloseIdleConnections)
		return client
	}
	oldInfo := &panel.NodeInfo{Type: "vless", Security: panel.Tls, ConfigurationHash: strings.Repeat("a", 64), CertificateFingerprint: strings.Repeat("c", 64), Common: &panel.CommonNode{DomainTransition: &panel.DomainTransition{ID: strings.Repeat("d", 32)}}}
	nextInfo := *oldInfo
	common := *oldInfo.Common
	common.DomainTransition = nil
	nextInfo.Common = &common
	nextInfo.ConfigurationHash = strings.Repeat("b", 64)
	serving := &Controller{apiClient: api(), info: oldInfo}
	old := &Node{NodeInfos: []*panel.NodeInfo{oldInfo}, controllers: []*Controller{serving}}
	next := &Node{NodeInfos: []*panel.NodeInfo{&nextInfo}, controllers: []*Controller{{apiClient: api(), info: &nextInfo}}}
	serving.receiptMu.Lock()
	locked := true
	defer func() {
		if locked {
			serving.receiptMu.Unlock()
		}
	}()
	finished := make(chan bool, 1)
	go func() { finished <- old.ConfirmPublication(next) }()
	deadline := time.Now().Add(time.Second)
	for serving.currentReceipt().hash != nextInfo.ConfigurationHash {
		if time.Now().After(deadline) {
			t.Fatal("final publication did not update applied configuration")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-received:
		t.Fatal("temporary candidate bypassed the serving certificate receipt lock")
	case <-finished:
		t.Fatal("final publication bypassed the serving certificate receipt lock")
	case <-time.After(100 * time.Millisecond):
	}
	// The real renewal pipeline validates the bytes before updating this field;
	// hold the sender here to deterministically exercise publication interleaving.
	newFingerprint := strings.Repeat("e", 64)
	serving.updateReceipt("", newFingerprint)
	renewed := make(chan error, 1)
	go func() { renewed <- serving.reportCurrentConfiguration(context.Background(), "") }()
	serving.receiptMu.Unlock()
	locked = false
	if !<-finished {
		t.Fatal("final publication was not accepted")
	}
	if err := <-renewed; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case receipt := <-received:
			if receipt.hash != nextInfo.ConfigurationHash || receipt.fingerprint != newFingerprint {
				t.Fatal("same-hash final publication regressed the renewed certificate fingerprint")
			}
		case <-time.After(time.Second):
			t.Fatal("certificate receipt was not sent")
		}
	}
}

// A rejected/temporarily unavailable configuration must not freeze the working
// listener's authorization snapshot. Exercise actual VLESS authentication, not
// only the controller's copy of the user list.
func TestUserRefreshIndependentOfConfigurationFailure(t *testing.T) {
	for _, version := range []string{"legacy", "v10"} {
		for _, failure := range []string{"http-error", "decode-error", "pending-reload"} {
			t.Run(version+"/"+failure, func(t *testing.T) {
				var enabled, usersUnavailable atomic.Bool
				var configRequests, userRequests atomic.Int32
				apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/capabilities"):
						fmt.Fprint(w, `{"data":{"accepted":true}}`)
					case strings.HasSuffix(r.URL.Path, "/configuration"), strings.HasSuffix(r.URL.Path, "/config"):
						configRequests.Add(1)
						if failure == "http-error" {
							w.WriteHeader(503)
							return
						}
						if failure == "decode-error" {
							fmt.Fprint(w, `{"broken":true}`)
							return
						}
						w.WriteHeader(304)
					case strings.HasSuffix(r.URL.Path, "/users"), strings.HasSuffix(r.URL.Path, "/user"):
						userRequests.Add(1)
						if usersUnavailable.Load() {
							w.WriteHeader(503)
							return
						}
						etag := fmt.Sprintf(`"users-%t"`, enabled.Load())
						if r.Header.Get("If-None-Match") == etag {
							w.WriteHeader(304)
							return
						}
						w.Header().Set("ETag", etag)
						users := `[]`
						if enabled.Load() {
							users = `[{"id":42,"uuid":"b831381d-6324-4d53-ad4f-8cda48b30811"}]`
						}
						if version == "v10" {
							fmt.Fprintf(w, `{"data":%s}`, users)
						} else {
							fmt.Fprintf(w, `{"users":%s}`, users)
						}
					case strings.HasSuffix(r.URL.Path, "/online-counts"):
						fmt.Fprint(w, `{"data":[]}`)
					case strings.HasSuffix(r.URL.Path, "/alivelist"):
						fmt.Fprint(w, `{"alive":{}}`)
					default:
						t.Errorf("unexpected fixture endpoint %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer apiServer.Close()
				roots := x509.NewCertPool()
				roots.AddCert(apiServer.Certificate())
				t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
				fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
				retries := 0
				api, err := panel.New(&conf.NodeConfig{APIHost: apiServer.URL, NodeID: 1, Key: "fixture", APIVersion: version, RetryCount: &retries})
				if err != nil {
					t.Fatal(err)
				}
				defer api.CloseIdleConnections()
				portProbe, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := portProbe.Addr().(*net.TCPAddr).Port
				portProbe.Close()
				info := &panel.NodeInfo{Tag: "refresh-" + version + "-" + failure, Type: "vless", Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: port, Network: "tcp"}}
				cfg := conf.New()
				cfg.OutboundAllowCIDRs = []string{"127.0.0.0/8"}
				server := core.New(cfg)
				defer server.Close()
				limits := limiter.AddLimiter(info.Type, info.Tag, nil, nil)
				defer limiter.DeleteLimiter(info.Tag)
				if err = server.AddNode(info.Tag, info); err != nil {
					t.Fatal(err)
				}
				if err = server.SetUsers(info.Tag, nil); err != nil {
					t.Fatal(err)
				}
				reload := make(chan struct{}, 1)
				server.ReloadCh = reload
				controller := &Controller{apiClient: api, info: info, tag: info.Tag, server: server, limiter: limits, reloadPending: failure == "pending-reload"}
				echo, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer echo.Close()
				go func() {
					for {
						conn, err := echo.Accept()
						if err != nil {
							return
						}
						go func() { defer conn.Close(); io.Copy(conn, conn) }()
					}
				}()
				exchange := func() bool {
					conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
					if err != nil {
						return false
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(time.Second))
					client, err := vless.NewClient("b831381d-6324-4d53-ad4f-8cda48b30811", "", nil)
					if err != nil {
						t.Fatal(err)
					}
					stream, err := client.DialConn(conn, M.ParseSocksaddr(echo.Addr().String()))
					if err != nil {
						return false
					}
					payload := []byte("renewed-user-local-echo")
					if _, err = stream.Write(payload); err != nil {
						return false
					}
					got := make([]byte, len(payload))
					_, err = io.ReadFull(stream, got)
					return err == nil && bytes.Equal(got, payload)
				}
				if exchange() {
					t.Fatal("unentitled fixture user was initially accepted")
				}
				enabled.Store(true)
				if err = controller.nodeInfoMonitor(context.Background()); err != nil {
					t.Fatal(err)
				}
				if userRequests.Load() != 1 || !exchange() {
					t.Fatal("configuration failure prevented renewed user authorization")
				}
				// A user endpoint outage retains the last usable table, while
				// configuration polling/reload recovery must still continue.
				usersUnavailable.Store(true)
				before := configRequests.Load()
				if err = controller.nodeInfoMonitor(context.Background()); err != nil {
					t.Fatal(err)
				}
				if configRequests.Load() != before+1 || !exchange() {
					t.Fatal("user outage blocked configuration recovery or removed usable users")
				}
				usersUnavailable.Store(false)
				enabled.Store(false)
				if err = controller.nodeInfoMonitor(context.Background()); err != nil {
					t.Fatal(err)
				}
				if exchange() {
					t.Fatal("configuration failure prevented user revocation")
				}
				if failure == "pending-reload" {
					select {
					case <-reload:
					default:
						t.Fatal("pending configuration was not retried")
					}
				}
				beforeUsers, beforeConfig := userRequests.Load(), configRequests.Load()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err = controller.nodeInfoMonitor(ctx); err == nil {
					t.Fatal("canceled monitor continued polling")
				}
				if userRequests.Load() != beforeUsers || configRequests.Load() != beforeConfig {
					t.Fatal("canceled monitor reached panel endpoints")
				}
			})
		}
	}
}
