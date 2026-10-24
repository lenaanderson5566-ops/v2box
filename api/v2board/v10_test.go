package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/internal/buildinfo"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentLegacyCredentialNeverEntersURL(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("token") || r.Header.Get("Authorization") != "Bearer nk_test" {
			t.Error("independent credential must only use Authorization")
		}
		w.Write([]byte(`{"users":[]}`))
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "nk_test", APIVersion: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetUserList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

// Optional fixture exported by the actual panel HTTP contract test, never a
// production response. This checks PHP/Go serialization interoperability.
func TestV10PanelFixture(t *testing.T) {
	name := os.Getenv("V2BOX_PANEL_FIXTURE")
	if name == "" {
		t.Skip("panel fixture not provided")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.Write([]byte(`{"data":{"accepted":true}}`))
			return
		}
		w.Write(data)
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n.Type != "vless" || n.Common.ServerPort != 443 || len(n.Common.Routes) != 0 || len(n.Common.SingBox) == 0 {
		t.Fatal("PHP/Go configuration mismatch")
	}
	var envelope struct {
		Meta struct {
			Hash string `json:"configurationHash"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Meta.Hash) != 64 || n.ConfigurationHash != envelope.Meta.Hash {
		t.Fatal("PHP/Go configuration receipt version mismatch without ETag")
	}
}

func TestV10Contract(t *testing.T) {
	ctx := context.Background()
	usersBody := `{"data":[{"id":42,"uuid":"user","speedLimit":10,"deviceLimit":2}]}`
	badAck := false
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Node-Type") != "v2box" || r.Header.Get("X-Node-ID") != "7" {
			t.Error("missing node identity headers")
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.URL.RawQuery != "" {
			t.Error("V10 must use header credentials only")
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v10/server/") {
			t.Error(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/v10/server/") {
		case "capabilities":
			if r.Method != "POST" {
				t.Error("capabilities method")
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["agentVersion"] != buildinfo.Version {
				t.Error("running agent version missing")
			}
			if len(body["features"].([]any)) != 9 {
				t.Error("capabilities missing")
			}
			w.Write([]byte(`{"data":{"accepted":true}}`))
		case "configuration":
			w.Header().Set("ETag", `"`+strings.Repeat("a", 64)+`"`)
			w.Write([]byte(`{"data":{"protocol":"vless","listenAddress":"127.0.0.1","listenPort":443,"network":"tcp","tls":0,"tlsSettings":{"serverName":"example.org"},"intervals":{"pushInterval":60,"pullInterval":60},"routes":[{"id":1,"match":["domain:example.org"],"action":"block","actionValue":null}],"singBox":{"route":{"final":"direct"}}}}`))
		case "users":
			w.Header().Set("ETag", "users-v1")
			if r.Header.Get("If-None-Match") == "users-v1" {
				w.WriteHeader(304)
			} else {
				w.Write([]byte(usersBody))
			}
		case "online-counts":
			w.Write([]byte(`{"data":[{"userId":42,"count":2}]}`))
		case "traffic":
			var body struct {
				Records []struct {
					ID     int   `json:"userId"`
					Upload int64 `json:"uploadBytes"`
				} `json:"records"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Records) != 1 || body.Records[0].ID != 42 || body.Records[0].Upload != 123 {
				t.Error("traffic contract")
			}
			w.WriteHeader(202)
			if badAck {
				w.Write([]byte(`{"data":{"accepted":false}}`))
			} else {
				w.Write([]byte(`{"data":{"accepted":true}}`))
			}
		case "online-users":
			var body struct {
				Records []struct {
					IPs []string `json:"ips"`
				} `json:"records"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Records) != 1 || body.Records[0].IPs[0] != "127.0.0.1" {
				t.Error("online contract")
			}
			w.Write([]byte(`{"data":{"accepted":true}}`))
		default:
			t.Error("unexpected path")
		}
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 7, Key: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.GetNodeInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Common.ServerPort != 443 || n.Common.TlsSettings.ServerName != "example.org" || len(n.Common.Routes) != 1 || len(n.Common.SingBox) == 0 {
		t.Fatal("configuration mapping")
	}
	if n.Common.CertInfo.Email != "fastdog.ltd@gmail.com" {
		t.Fatal("certificate contact must be independent of the original panel mailbox")
	}
	u, err := c.GetUserList(ctx)
	if err != nil || len(u) != 1 || u[0].SpeedLimit != 10 {
		t.Fatalf("users: %v %v", u, err)
	}
	u, err = c.GetUserList(ctx)
	if err != nil || u != nil {
		t.Fatal("304 semantics")
	}
	a, err := c.GetUserAlive(ctx)
	if err != nil || a[42] != 2 {
		t.Fatal("online counts")
	}
	if err = c.ReportUserTraffic(ctx, []UserTraffic{{UID: 42, Upload: 123}}); err != nil {
		t.Fatal(err)
	}
	badAck = true
	if c.ReportUserTraffic(ctx, []UserTraffic{{UID: 42, Upload: 123}}) == nil {
		t.Fatal("missing ack accepted")
	}
	data := map[int][]string{42: {"127.0.0.1_node"}}
	if err = c.ReportNodeOnlineUsers(ctx, &data); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"data":[]}`, `{"data":null}`, `{}`, `{"data":[{"id":1,"uuid":"a"},{"id":1,"uuid":"b"}]}`} {
		usersBody = body
		c.InvalidateUserCache()
		u, err = c.GetUserList(ctx)
		if body == `{"data":[]}` {
			if err != nil || u == nil || len(u) != 0 {
				t.Fatal("empty table")
			}
		} else if err == nil {
			t.Fatal("malformed response accepted")
		}
	}
}

func TestTargetStatisticsContract(t *testing.T) {
	mode := "enabled"
	var batches []string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" || r.URL.Query().Get("token") != "" {
			t.Error("bad node authentication")
		}
		if strings.HasSuffix(r.URL.Path, "/policy") {
			switch mode {
			case "oversized":
				w.Write([]byte(strings.Repeat(" ", 4097)))
			case "bad-interval":
				w.Write([]byte(`{"data":{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120,"reportIntervalSeconds":10}}`))
			case "bad-version":
				w.Write([]byte(`{"data":{"enabled":true,"version":"invalid","leaseSeconds":120}}`))
			case "missing":
				w.WriteHeader(404)
			case "invalid":
				w.Write([]byte(`{"data":{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":121}}`))
			case "disabled":
				w.Write([]byte(`{"data":{"enabled":false,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120}}`))
			default:
				w.Write([]byte(`{"data":{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120}}`))
			}
			return
		}
		var batch TargetBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		batches = append(batches, batch.BatchID)
		if mode == "oversized-ack" {
			w.Write([]byte(strings.Repeat(" ", 4097)))
			return
		}
		w.Write([]byte(`{"data":{"accepted":false}}`))
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.GetTargetPolicy(context.Background()); err != nil || !p.Enabled {
		t.Fatal("enabled policy", err)
	}
	mode = "disabled"
	if p, err := c.GetTargetPolicy(context.Background()); err != nil || p.Enabled {
		t.Fatal("disabled policy", err)
	}
	mode = "missing"
	if p, err := c.GetTargetPolicy(context.Background()); err != nil || p.Enabled {
		t.Fatal("legacy panel", err)
	}
	mode = "invalid"
	if _, err := c.GetTargetPolicy(context.Background()); err == nil {
		t.Fatal("unbounded lease accepted")
	}
	for _, invalid := range []string{"oversized", "bad-version", "bad-interval"} {
		mode = invalid
		if _, err := c.GetTargetPolicy(context.Background()); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	mode = "invalid"
	batch := &TargetBatch{Version: "v", BatchID: "same", Records: []TargetRecord{}}
	for i := 0; i < 2; i++ {
		if accepted, err := c.ReportTargets(context.Background(), batch); err != nil || accepted {
			t.Fatal("disable acknowledgement", err)
		}
	}
	if len(batches) != 2 || batches[0] != batches[1] {
		t.Fatal("retry ID changed")
	}
	mode = "oversized-ack"
	if _, err := c.ReportTargets(context.Background(), batch); err == nil {
		t.Fatal("accepted oversized acknowledgement")
	}
}

func TestTargetPolicyConfigurationLeaseAndBodyLimit(t *testing.T) {
	hits := 0
	header := `{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120}`
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/configuration"):
			w.Header().Set("X-Target-Statistics-Policy", header)
			w.WriteHeader(304)
		case strings.HasSuffix(r.URL.Path, "/policy"):
			hits++
			w.Write([]byte(`{"data":{"enabled":false,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120}}`))
		case strings.HasSuffix(r.URL.Path, "/capabilities"):
			w.Write([]byte(`{"data":{"accepted":true}}`))
		default:
			t.Error("oversized batch reached HTTP")
		}
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetNodeInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := c.GetTargetPolicy(context.Background())
	if err != nil || !p.Enabled || p.LeaseSeconds > 120 || hits != 0 {
		t.Fatalf("header lease not reused: %+v %v hits=%d", p, err, hits)
	}
	c.targetPolicy.Store(&cachedTargetPolicy{p, time.Now().Add(90 * time.Second)})
	if reduced, _ := c.GetTargetPolicy(context.Background()); reduced.LeaseSeconds > 90 {
		t.Fatal("cached lease was renewed")
	}
	c.targetPolicy.Store(&cachedTargetPolicy{p, time.Now().Add(69 * time.Second)})
	if p, err = c.GetTargetPolicy(context.Background()); err != nil || p.Enabled || hits != 1 {
		t.Fatal("stale lease was extended", err)
	}
	header = `{"enabled":false,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120}`
	c.GetNodeInfo(context.Background())
	if p, err = c.GetTargetPolicy(context.Background()); err != nil || p.Enabled || hits != 1 {
		t.Fatal("304 disable ignored", err)
	}
	header = "invalid"
	c.GetNodeInfo(context.Background())
	if _, err = c.GetTargetPolicy(context.Background()); err != nil || hits != 2 {
		t.Fatal("invalid header did not fall back", err)
	}
	batch := &TargetBatch{Records: []TargetRecord{{Target: strings.Repeat("x", 262144)}}}
	if _, err = c.ReportTargets(context.Background(), batch); err == nil {
		t.Fatal("unbounded outgoing request")
	}
}

func TestTargetIntervalCompatibility(t *testing.T) {
	for _, seconds := range []int{0, 30, 60, 120, 300} {
		p := TargetPolicy{ReportIntervalSeconds: seconds}
		expected := seconds
		if expected == 0 {
			expected = 60
		}
		if p.ReportInterval() != time.Duration(expected)*time.Second {
			t.Fatal("bad interval fallback")
		}
		c := &Client{}
		raw := fmt.Sprintf(`{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120,"reportIntervalSeconds":%d}`, seconds)
		c.captureTargetPolicy(raw)
		if c.targetPolicy.Load() == nil || c.targetPolicy.Load().policy.ReportIntervalSeconds != seconds {
			t.Fatal("cadence lost in policy header")
		}
	}
	c := &Client{}
	c.captureTargetPolicy(`{"enabled":true,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120,"reportIntervalSeconds":301}`)
	if c.targetPolicy.Load() != nil {
		t.Fatal("unsafe cadence accepted from header")
	}
}

func TestConfigurationReceiptRetryAndValidation(t *testing.T) {
	reports := make(chan ConfigurationReport, 16)
	var fail atomic.Bool
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer receipt-test" || r.URL.RawQuery != "" {
			t.Error("receipt credentials must be headers only")
		}
		var body struct {
			Configuration *ConfigurationReport `json:"configuration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Configuration != nil {
			reports <- *body.Configuration
		}
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer s.Close()
	zero := 0
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "receipt-test", RetryCount: &zero})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	c.ReportConfiguration(context.Background(), "invalid", "")
	c.ReportConfiguration(context.Background(), hash, "arbitrary-secret")
	if len(reports) != 0 {
		t.Fatal("invalid receipt sent")
	}
	fail.Store(true)
	c.ReportConfiguration(context.Background(), hash, "core")
	report := <-reports
	if report.State != "failed" || report.ErrorCode != "core" {
		t.Fatal(report)
	}
	fail.Store(false)
	if err := c.announceCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if report = <-reports; report.Hash != hash || report.State != "failed" {
		t.Fatal("latest receipt not retried", report)
	}
	c.ReportConfiguration(context.Background(), hash, "", strings.Repeat("f", 64))
	if report = <-reports; report.State != "applied" || report.ErrorCode != "" || report.CertificateFingerprint != strings.Repeat("f", 64) {
		t.Fatal("success retains error", report)
	}
}

func TestConfigurationDecodeFailureReceipt(t *testing.T) {
	reports := make(chan ConfigurationReport, 4)
	hash := strings.Repeat("b", 64)
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			var body struct {
				Configuration *ConfigurationReport `json:"configuration"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Configuration != nil {
				reports <- *body.Configuration
			}
			w.Write([]byte(`{"data":{"accepted":true}}`))
			return
		}
		w.Header().Set("ETag", `"`+hash+`"`)
		w.Write([]byte(`{"data":{"protocol":"invalid","listenPort":443}}`))
	}))
	defer s.Close()
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetNodeInfo(context.Background()); err == nil {
		t.Fatal("invalid protocol accepted")
	}
	select {
	case report := <-reports:
		if report.Hash != hash || report.State != "failed" || report.ErrorCode != "decode" {
			t.Fatal(report)
		}
	default:
		t.Fatal("decode failure missing receipt")
	}
}

func TestConfigurationHashAcrossProxyResponses(t *testing.T) {
	hash := strings.Repeat("a", 64)
	id := strings.Repeat("c", 32)
	for _, tc := range []struct {
		name, etag, meta string
		fail             bool
	}{
		{"strong", `"` + hash + `"`, "", false},
		{"weak", `W/"` + hash + `"`, "", false},
		{"missing-header", "", `,"meta":{"configurationHash":"` + hash + `"}`, false},
		{"rewritten-header", `W/"proxy-cache-key"`, `,"meta":{"configurationHash":"` + hash + `"}`, false},
		{"missing-version", "", "", true},
		{"invalid-weak", `W/"short"`, "", true},
		{"unquoted", hash, "", true},
		{"multiple", `"` + hash + `", "` + hash + `"`, "", true},
		{"nonhex", `"` + strings.Repeat("z", 64) + `"`, "", true},
		{"invalid-meta", `"` + hash + `"`, `,"meta":{"configurationHash":"bad"}`, true},
		{"null-meta-hash", `"` + hash + `"`, `,"meta":{"configurationHash":null}`, true},
		{"numeric-meta-hash", `"` + hash + `"`, `,"meta":{"configurationHash":123}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var repaired atomic.Bool
			var receipts atomic.Int32
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					var body struct {
						Configuration *ConfigurationReport `json:"configuration"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Configuration != nil {
						if body.Configuration.Hash != hash || body.Configuration.TransitionID != id || body.Configuration.State != "applied" {
							t.Error("incorrect application receipt", body.Configuration)
						}
						receipts.Add(1)
					}
					w.Write([]byte(`{"data":{"accepted":true,"domainTransitionAccepted":true}}`))
					return
				}
				etag, meta := tc.etag, tc.meta
				if repaired.Load() {
					etag, meta = `W/"`+hash+`"`, ""
				}
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				fmt.Fprintf(w, `{"data":{"protocol":"anytls","listenPort":9443,"tls":1,"tlsSettings":{"serverName":"new.example.test","certMode":"http"},"intervals":{"pushInterval":60,"pullInterval":60},"domainTransition":{"id":"%s","domain":"new.example.test","addresses":{"ipv4":"8.8.8.8"}}}%s}`, id, meta)
			}))
			defer s.Close()
			c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 3, Key: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			n, err := c.GetNodeInfo(context.Background())
			if tc.fail {
				if err == nil || n != nil || c.nodeEtag != "" || c.responseBodyHash != "" || receipts.Load() != 0 {
					t.Fatalf("invalid version reached node preparation or poisoned cache: %v", err)
				}
				repaired.Store(true)
				n, err = c.GetNodeInfo(context.Background())
			}
			if err != nil || n == nil || n.ConfigurationHash != hash || n.Common.DomainTransition.ID != id {
				t.Fatalf("candidate version lost: %v", err)
			}
			if !tc.fail && c.nodeEtag != tc.etag {
				t.Fatal("transport cache validator rewritten")
			}
			if err := c.ReportConfiguration(context.Background(), n.ConfigurationHash, "", strings.Repeat("b", 64), id); err != nil {
				t.Fatal(err)
			}
			if receipts.Load() != 1 {
				t.Fatal("publication receipt not confirmed exactly once")
			}
		})
	}
}

func TestDecodeCertificateTransition(t *testing.T) {
	var n CommonNode
	err := decodeV10Config([]byte(`{"data":{"protocol":"vless","listenPort":443,"tls":1,"tlsSettings":{"serverName":"new.example.test","certMode":"http"},"domainTransition":{"id":"0123456789abcdef0123456789abcdef","domain":"new.example.test","addresses":{"ipv4":"8.8.8.8"}}}}`), &n)
	if err != nil || n.DomainTransition == nil || n.DomainTransition.Domain != n.TlsSettings.ServerName || n.DomainTransition.Addresses["ipv4"] != "8.8.8.8" {
		t.Fatalf("transition metadata lost: %#v %v", n, err)
	}
}

func TestDomainTransitionRequiresPublicationAcknowledgement(t *testing.T) {
	var accepted atomic.Bool
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepted.Load() {
			w.Write([]byte(`{"data":{"accepted":true,"domainTransitionAccepted":true}}`))
		} else {
			w.Write([]byte(`{"data":{"accepted":true,"domainTransitionAccepted":false}}`))
		}
	}))
	defer s.Close()
	zero := 0
	c, err := newTestPanel(t, s, &conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "test", RetryCount: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReportConfiguration(context.Background(), strings.Repeat("a", 64), "", strings.Repeat("b", 64), strings.Repeat("c", 32)); err == nil {
		t.Fatal("rejected publication was treated as success")
	}
	accepted.Store(true)
	if err := c.announceCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.configurationReport.Load().TransitionID != "" {
		t.Fatal("acknowledged transition continued to block future configuration fetches")
	}
}
