package panel

import (
	"context"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserListAndFailedReport(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	body := `{"users":[]}`
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIVersion: "legacy", APIHost: s.URL, NodeID: 1, Key: "test", RetryCount: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	users, err := c.GetUserList(context.Background())
	if err != nil || users == nil || len(users) != 0 {
		t.Fatalf("empty list must be distinct from unchanged: %v %v", users, err)
	}
	status.Store(304)
	users, err = c.GetUserList(context.Background())
	if err != nil || users != nil {
		t.Fatal("304 should mean unchanged")
	}
	status.Store(500)
	if err = c.ReportUserTraffic(context.Background(), []UserTraffic{{UID: 1, Upload: 5}}); err == nil {
		t.Fatal("HTTP failure accepted")
	}
	if _, err = c.GetUserList(context.Background()); err == nil {
		t.Fatal("HTTP failure accepted as user list")
	}
}

func TestPanelRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://panel.example.test",
		"https://user:fixture@panel.example.test",
		"https://panel.example.test/?token=fixture",
		"https://panel.example.test/#fragment",
		"https://panel.example.test/?",
		"https:///missing-host",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := New(&conf.NodeConfig{APIHost: endpoint}); err == nil {
				t.Fatal("unsafe panel endpoint accepted")
			}
		})
	}
}

func TestNodeTypeCompatibility(t *testing.T) {
	for _, tc := range []struct{ configured, expected string }{{"", "v2node"}, {"v2node", "v2node"}, {"v2box", "v2box"}} {
		t.Run(tc.expected+tc.configured, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("node_type") != tc.expected {
					t.Errorf("wrong node type: %s", r.URL.RawQuery)
				}
				if r.URL.Query().Get("node_id") != "42" {
					t.Error("node id lost")
				}
				_, _ = w.Write([]byte(`{"users":[]}`))
			}))
			defer server.Close()
			client, err := newTestPanel(t, server, &conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, NodeID: 42, Key: "test", NodeType: tc.configured})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetUserList(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := New(&conf.NodeConfig{APIVersion: "legacy", NodeType: "unsupported"}); err == nil {
		t.Fatal("invalid type accepted")
	}
}

func TestNaiveNodeConfig(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/config" || r.URL.Query().Get("node_type") != "v2box" {
			t.Error("incorrect Naive node request")
		}
		_, _ = w.Write([]byte(`{"protocol":"naive","network":"tcp","tls":1,"server_port":443,"tls_settings":{"cert_mode":"file","server_name":"naive.example","cert_file":"/cert.pem","key_file":"/key.pem"},"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer server.Close()
	client, err := newTestPanel(t, server, &conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, NodeID: 42, NodeType: "v2box", Key: "test"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if node.Type != "naive" || node.Security != Tls || node.Common.CertInfo.CertDomain != "naive.example" || node.Common.Network != "tcp" {
		t.Fatalf("Naive config not preserved: %+v", node)
	}
}

func TestLandingPageUsesLocalConfigOnly(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"protocol":"naive","network":"tcp","tls":1,"server_port":443,"landing_page":{"enabled":true,"directory":"/etc"},"LandingPage":{"Enabled":true,"Directory":"/etc"},"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer server.Close()
	for _, local := range []conf.LandingPage{{}, {Enabled: true, Directory: "/site"}} {
		client, err := newTestPanel(t, server, &conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, LandingPage: local})
		if err != nil {
			t.Fatal(err)
		}
		node, err := client.GetNodeInfo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if node.Common.LandingPage != local {
			t.Fatal("panel response overrode local static directory")
		}
	}
}

func TestLegacyAliveErrorsAreNotEmptySuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		fail   bool
	}{
		{"unavailable", 503, "", true},
		{"unauthorized", 403, "", true},
		{"malformed", 200, "not-json", true},
		{"unsupported", 404, "", false},
		{"valid", 200, `{"alive":{"1":2}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer s.Close()
			c, err := newTestPanel(t, s, &conf.NodeConfig{APIVersion: "legacy", APIHost: s.URL, NodeID: 1, Key: "test", RetryCount: new(int)})
			if err != nil {
				t.Fatal(err)
			}
			alive, err := c.GetUserAlive(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result: %v, %v", alive, err)
			}
			if tc.name == "valid" && alive[1] != 2 {
				t.Fatalf("online counts lost: %v", alive)
			}
		})
	}
}

func TestCloseIdleConnectionsReleasesSocketAndAllowsReuse(t *testing.T) {
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"users":[]}`)) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.StartTLS()
	defer server.Close()
	c, err := newTestPanel(t, server, &conf.NodeConfig{APIHost: server.URL, APIVersion: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.GetUserList(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.CloseIdleConnections()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("idle panel socket retained")
		}
	}
}
