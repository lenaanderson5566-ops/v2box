package panel

import (
	"context"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUserListAndFailedReport(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	body := `{"users":[]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	defer s.Close()
	c, err := New(&conf.NodeConfig{APIVersion: "legacy", APIHost: s.URL, NodeID: 1, Key: "test", RetryCount: new(int)})
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

func TestNodeTypeCompatibility(t *testing.T) {
	for _, tc := range []struct{ configured, expected string }{{"", "v2node"}, {"v2node", "v2node"}, {"v2box", "v2box"}} {
		t.Run(tc.expected+tc.configured, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("node_type") != tc.expected {
					t.Errorf("wrong node type: %s", r.URL.RawQuery)
				}
				if r.URL.Query().Get("node_id") != "42" {
					t.Error("node id lost")
				}
				_, _ = w.Write([]byte(`{"users":[]}`))
			}))
			defer server.Close()
			client, err := New(&conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, NodeID: 42, Key: "test", NodeType: tc.configured})
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/config" || r.URL.Query().Get("node_type") != "v2box" {
			t.Error("incorrect Naive node request")
		}
		_, _ = w.Write([]byte(`{"protocol":"naive","network":"tcp","tls":1,"server_port":443,"tls_settings":{"cert_mode":"file","server_name":"naive.example","cert_file":"/cert.pem","key_file":"/key.pem"},"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer server.Close()
	client, err := New(&conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, NodeID: 42, NodeType: "v2box", Key: "test"})
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"protocol":"naive","network":"tcp","tls":1,"server_port":443,"landing_page":{"enabled":true,"directory":"/etc"},"LandingPage":{"Enabled":true,"Directory":"/etc"},"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer server.Close()
	for _, local := range []conf.LandingPage{{}, {Enabled: true, Directory: "/site"}} {
		client, err := New(&conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, LandingPage: local})
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
