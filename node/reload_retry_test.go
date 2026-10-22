package node

import (
	"context"
	"crypto/x509"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"net/http"
	"net/http/httptest"
	"os"
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
