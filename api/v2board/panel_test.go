package panel

import (
	"context"
	"errors"
	"fmt"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
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

func TestPanelHTTPConfigurationBounds(t *testing.T) {
	negative := -1
	configs := []conf.NodeConfig{{Timeout: -1}, {RetryCount: &negative}}
	maximumSeconds := int64((1<<63 - 1) / time.Second)
	if int64(^uint(0)>>1) > maximumSeconds {
		configs = append(configs, conf.NodeConfig{Timeout: int(maximumSeconds) + 1})
	}
	for _, config := range configs {
		config.APIHost = "https://panel.example.test"
		if _, err := New(&config); err == nil {
			t.Fatal("invalid HTTP timeout/retry configuration accepted")
		}
	}
	client, err := New(&conf.NodeConfig{APIHost: "https://panel.example.test"})
	if err != nil || client.client.GetClient().Timeout != conf.DefaultNodeTimeout*time.Second || client.client.RetryCount != conf.DefaultNodeRetryCount {
		t.Fatal("default timeout/retry settings changed", err)
	}
}

func TestPanelIntervalBounds(t *testing.T) {
	for _, value := range []any{1, "60", float64(60)} {
		if intervalToTime(value) <= 0 {
			t.Fatal("valid integer polling interval rejected")
		}
	}
	for _, value := range []any{nil, true, -1, "0", "1.5", "9223372037", "18446744073709551616", float64(0.5), float64(1.5), float64(9223372037)} {
		if intervalToTime(value) != 0 {
			t.Fatal("invalid or overflowing interval accepted")
		}
	}
}

func TestLegacyUserSnapshotRequiresCompleteEnvelope(t *testing.T) {
	valid, err := msgpack.Marshal(map[string]any{"users": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, contentType string
	}{
		{"missing", `{}`, "application/json"},
		{"null", `{"users":null}`, "application/json"},
		{"truncated-array", `{"users":[`, "application/json"},
		{"truncated-envelope", `{"users":[]`, "application/json"},
		{"nested-only", `{"meta":{"users":[]}}`, "application/json"},
		{"trailing-record", `{"users":[]}{"users":[]}`, "application/json"},
		{"duplicate-field", `{"users":[],"users":[]}`, "application/json"},
		{"duplicate-id", `{"users":[{"id":1,"uuid":"a"},{"id":1,"uuid":"b"}]}`, "application/json"},
		{"empty-msgpack", string(valid), "application/x-msgpack"},
		{"msgpack-trailing-record", string(append(append([]byte{}, valid...), valid...)), "application/x-msgpack"},
		{"msgpack-false-array-length", "\x81\xa5users\xdd\xff\xff\xff\xff", "application/x-msgpack"},
		{"msgpack-enormous-trailing-array", string(valid) + "\xdd\xff\xff\xff\xff", "application/x-msgpack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("ETag", `"invalid-next"`)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := newTestPanel(t, server, &conf.NodeConfig{APIHost: server.URL, APIVersion: "legacy", RetryCount: new(int)})
			if err != nil {
				t.Fatal(err)
			}
			client.userEtag = `"previous"`
			if tc.name == "empty-msgpack" {
				if users, err := client.GetUserList(context.Background()); err != nil || users == nil || len(users) != 0 || client.userEtag != `"invalid-next"` {
					t.Fatal("valid empty MessagePack snapshot rejected", err)
				}
				return
			}
			if users, err := client.GetUserList(context.Background()); err == nil || users != nil || client.userEtag != `"previous"` {
				t.Fatal("incomplete snapshot replaced authorization/cache state", err)
			}
		})
	}
}

func TestPanelUserResponseLimitsAndRecovery(t *testing.T) {
	for _, version := range []string{"legacy", "v10"} {
		t.Run(version, func(t *testing.T) {
			var oversized atomic.Bool
			oversized.Store(true)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"next"`)
				if oversized.Load() {
					fmt.Fprint(w, strings.Repeat(" ", 256))
				}
				if version == "legacy" {
					fmt.Fprint(w, `{"users":[]}`)
				} else {
					fmt.Fprint(w, `{"data":[]}`)
				}
			}))
			defer server.Close()
			client, err := newTestPanel(t, server, &conf.NodeConfig{APIHost: server.URL, APIVersion: version, RetryCount: new(int)})
			if err != nil {
				t.Fatal(err)
			}
			if client.client.ResponseBodyLimit < 64*1024*1024 {
				t.Fatal("normal large-directory allowance reduced")
			}
			// A small test-only cap exercises both Resty and the streaming path.
			client.client.SetResponseBodyLimit(128)
			if _, err = client.GetUserList(context.Background()); err == nil || client.userEtag != "" {
				t.Fatal("oversized user response accepted or cached")
			}
			oversized.Store(false)
			users, err := client.GetUserList(context.Background())
			if err != nil || users == nil || len(users) != 0 || client.userEtag != `"next"` {
				t.Fatal("valid empty snapshot did not recover after oversized response", err)
			}
		})
	}
}

func TestLegacyLargeSnapshotStillStreams(t *testing.T) {
	// Do not regress large installations to a small 4/8 MiB response ceiling.
	padding := strings.Repeat(" ", 9*1024*1024)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, padding, `{"users":[]}`)
	}))
	defer server.Close()
	client, err := newTestPanel(t, server, &conf.NodeConfig{APIHost: server.URL, APIVersion: "legacy", RetryCount: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	if users, err := client.GetUserList(context.Background()); err != nil || users == nil {
		t.Fatal("large complete legacy snapshot rejected", err)
	}
}

func TestLegacyRequestErrorsHideCredentialsAndPreserveCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled request reached server") }))
	defer server.Close()
	key := "fixture&secret+with-space"
	client, err := newTestPanel(t, server, &conf.NodeConfig{APIHost: server.URL, APIVersion: "legacy", Key: key, RetryCount: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.GetUserList(ctx)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "token=") {
		t.Fatal("request error exposed a query credential or lost cancellation", err)
	}
	message := (credentialSafeLogger{key: key}).message("request %s / %s", key, url.QueryEscape(key))
	if strings.Contains(message, key) || strings.Contains(message, url.QueryEscape(key)) {
		t.Fatal("retry logger exposed raw or encoded credentials")
	}
}

func TestNodeTypeCompatibility(t *testing.T) {
	for _, tc := range []struct{ configured, expected string }{{"", "v2box"}, {"v2box", "v2box"}} {
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
	for _, retired := range []string{"v2node", "unsupported"} {
		if _, err := New(&conf.NodeConfig{APIHost: "https://panel.example.test", APIVersion: "legacy", NodeType: retired}); err == nil {
			t.Fatal("retired or invalid type accepted")
		}
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
		{"missing", 200, `{}`, true},
		{"null", 200, `{"alive":null}`, true},
		{"invalid-user", 200, `{"alive":{"0":2}}`, true},
		{"negative", 200, `{"alive":{"1":-1}}`, true},
		{"empty", 200, `{"alive":{}}`, false},
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

func TestSharedACMEBindingPreservesModeAndPinsDomain(t *testing.T) {
	for _, mode := range []string{"http", "dns"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"protocol":"vless","network":"tcp","tls":1,"server_port":20001,"tls_settings":{"cert_mode":%q,"server_name":"node.example.test"},"base_config":{"push_interval":60,"pull_interval":60}}`, mode)
			}))
			defer server.Close()
			binding := &conf.CertificateBinding{Domain: "node.example.test", CertFile: "/existing/cert.pem", KeyFile: "/existing/key.pem"}
			client, err := newTestPanel(t, server, &conf.NodeConfig{APIVersion: "legacy", APIHost: server.URL, NodeType: "v2box", NodeID: 14, Key: "test", SharedListener: "automatic", SharedCertificate: binding})
			if err != nil {
				t.Fatal(err)
			}
			info, err := client.GetNodeInfo(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cert := info.Common.CertInfo
			if cert.CertMode != mode || cert.CertFile != binding.CertFile || cert.KeyFile != binding.KeyFile || cert.LegacyCertFile != "" {
				t.Fatal("binding rewrote renewal mode or source paths")
			}
			binding.LegacyCertFile, binding.LegacyKeyFile = "/old/cert.pem", "/old/key.pem"
			client.nodeEtag, client.responseBodyHash = "", ""
			migrated, err := client.GetNodeInfo(context.Background())
			if err != nil || migrated.Common.CertInfo.LegacyCertFile != binding.LegacyCertFile || migrated.Common.CertInfo.LegacyKeyFile != binding.LegacyKeyFile {
				t.Fatal("binding lost legacy certificate/guard provenance", err)
			}
			binding.Domain = "another.example.test"
			client.nodeEtag = ""
			client.responseBodyHash = ""
			if _, err := client.GetNodeInfo(context.Background()); err == nil {
				t.Fatal("panel domain change reused an unrelated certificate binding")
			}
		})
	}
}
