package node

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixtureRootsOnce sync.Once

func TestRejectedReloadIsRetriedWithoutConfigChange(t *testing.T) {
	calls := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(304)
			return
		}
		w.Write([]byte(`{"protocol":"vless","tls":0,"server_port":12345,"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
	retries := 0
	api, err := panel.New(&conf.NodeConfig{APIHost: s.URL, NodeID: 1, Key: "test", APIVersion: "legacy", RetryCount: &retries})
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 1)
	c := &Controller{apiClient: api, server: &core.V2Core{ReloadCh: ch}}
	for i := 0; i < 2; i++ {
		if err := c.nodeInfoMonitor(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
		default:
			t.Fatalf("reload not requested on poll %d", i)
		}
	}
}

func TestTargetScheduleSeparatesUploadAndLease(t *testing.T) {
	now := time.Now()
	for _, seconds := range []int{30, 60, 120, 300} {
		c := &Controller{targetLastReport: now, targetInterval: time.Duration(seconds) * time.Second}
		if c.targetReportDelay(now) != min(time.Minute, time.Duration(seconds)*time.Second) {
			t.Fatal("lease poll exceeds a minute")
		}
		if c.targetReportDelay(now.Add(time.Duration(seconds-10)*time.Second)) != 10*time.Second {
			t.Fatal("remaining upload deadline ignored")
		}
		c.targetRetry = true
		if c.targetReportDelay(now) != 30*time.Second {
			t.Fatal("failed five-minute upload must retry before pending expires")
		}
	}
	c := &Controller{targetLastReport: now, targetInterval: 5 * time.Minute}
	c.targetInterval = 30 * time.Second
	if c.targetReportDelay(now.Add(time.Minute)) != time.Second {
		t.Fatal("shorter cadence did not become due")
	}
}

func TestLongTargetCadenceStillSynchronizesDisable(t *testing.T) {
	enabled := true
	calls := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"data":{"enabled":%t,"version":"a80e93b1-0cba-478a-b780-0d8eb6118267","leaseSeconds":120,"reportIntervalSeconds":300}}`, enabled)
	}))
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
	api, err := panel.New(&conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "test"})
	if err != nil {
		t.Fatal(err)
	}
	last := time.Now()
	c := &Controller{apiClient: api, server: core.New(conf.New()), tag: "n", targetLastReport: last}
	c.reportTargetStatistics(context.Background())
	if c.targetInterval != 5*time.Minute || c.targetLastReport != last {
		t.Fatal("lease renewal triggered an early upload")
	}
	enabled = false
	c.reportTargetStatistics(context.Background())
	if calls != 2 || !c.targetLastReport.IsZero() {
		t.Fatal("long report interval delayed stop policy")
	}
}

func TestTargetBackoffStillRenewsLease(t *testing.T) {
	now := time.Now()
	c := &Controller{targetRetry: true, targetRetryAt: now.Add(2 * time.Minute), targetRetryDelay: 2 * time.Minute}
	if c.targetReportDelay(now) != time.Minute || c.targetReportDelay(now.Add(100*time.Second)) != 20*time.Second {
		t.Fatal("retry interferes with lease renewal")
	}
	c.clearTargetRetry()
	if c.targetRetry || !c.targetRetryAt.IsZero() || c.targetRetryDelay != 0 {
		t.Fatal("retry state survived recovery")
	}
}

func TestTargetUploadRetryBackoffAndRecovery(t *testing.T) {
	now := time.Now()
	c := &Controller{}
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute} {
		c.scheduleTargetRetry(now)
		if !c.targetRetry || c.targetRetryAt.Sub(now) != want {
			t.Fatalf("retry deadline %s want %s", c.targetRetryAt.Sub(now), want)
		}
		if c.targetReportDelay(now) > time.Minute {
			t.Fatal("lease renewal delayed")
		}
		now = c.targetRetryAt
	}
	c.clearTargetRetry()
	c.scheduleTargetRetry(now)
	if c.targetRetryDelay != 30*time.Second {
		t.Fatal("fresh failure inherited old backoff")
	}
}

func TestCommittedTransitionDoesNotReloadUnchangedListener(t *testing.T) {
	old := &panel.NodeInfo{Id: 1, Type: "vless", Security: panel.Tls, Common: &panel.CommonNode{ServerPort: 443, TlsSettings: panel.TlsSettings{ServerName: "new.example.test"}, DomainTransition: &panel.DomainTransition{ID: "candidate"}}}
	next := *old
	common := *old.Common
	common.DomainTransition = nil
	next.Common = &common
	next.ConfigurationHash = "committed-hash"
	if !completedTransitionOnly(old, &next) {
		t.Fatal("metadata removal would disconnect the new listener twice")
	}
	if !(&Node{NodeInfos: []*panel.NodeInfo{old}}).ConfirmPublication(&Node{NodeInfos: []*panel.NodeInfo{&next}}) {
		t.Fatal("queued publication signal not absorbed")
	}
	common.ServerPort = 8443
	if completedTransitionOnly(old, &next) {
		t.Fatal("changed listener incorrectly skipped")
	}
	common.ServerPort = 443
	common.TlsSettings.ServerName = "old.example.test"
	if completedTransitionOnly(old, &next) {
		t.Fatal("rollback identity incorrectly skipped")
	}
	common.TlsSettings.ServerName = "new.example.test"
	common.DomainTransition = &panel.DomainTransition{ID: "retry"}
	if completedTransitionOnly(old, &next) {
		t.Fatal("new verification operation incorrectly skipped")
	}
}

func TestCloseRetainsFinalTrafficAndReplaysSameBatch(t *testing.T) {
	hits := 0
	batchID := ""
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/configuration"):
			w.Header().Set("ETag", `"`+strings.Repeat("a", 64)+`"`)
			w.Write([]byte(`{"data":{"protocol":"vless","listenPort":443,"tls":0,"intervals":{"pushInterval":60,"pullInterval":60},"trafficBatchDedup":true}}`))
		case strings.HasSuffix(r.URL.Path, "/traffic"):
			var body struct {
				BatchID string `json:"batchId"`
				Records []struct {
					Upload int64 `json:"uploadBytes"`
				} `json:"records"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.BatchID == "" || len(body.Records) != 1 || body.Records[0].Upload != 123 {
				t.Error("shutdown bypassed durable batch")
			}
			if batchID != "" && body.BatchID != batchID {
				t.Error("retry changed batch ID")
			}
			batchID = body.BatchID
			hits++
			fmt.Fprintf(w, `{"data":{"accepted":%t}}`, hits > 1)
		default:
			w.Write([]byte(`{"data":{"accepted":true}}`))
		}
	}))
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
	retries := 0
	spool := t.TempDir()
	api, err := panel.New(&conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 77, Key: "test", RetryCount: &retries, TrafficSpoolDirectory: spool})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.GetNodeInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := core.New(conf.New())
	defer server.Close()
	server.RestoreUserTraffic("shutdown-test", []panel.UserTraffic{{UID: 1, Upload: 123}})
	c := &Controller{apiClient: api, server: server, tag: "shutdown-test", limiter: limiter.AddLimiter("", "shutdown-test", nil, nil)}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(spool, "*", "*.json"))
	if len(files) != 1 {
		t.Fatal("final batch not retained")
	}
	live, _ := server.GetUserTrafficSlice(c.tag, 0)
	if len(live) != 0 {
		t.Fatal("durable bytes restored to counters")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ = filepath.Glob(filepath.Join(spool, "*", "*.json"))
	if len(files) != 0 || hits != 2 {
		t.Fatal("empty shutdown failed to drain pending batch")
	}
}
