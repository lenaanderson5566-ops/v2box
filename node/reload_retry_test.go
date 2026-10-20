package node

import (
	"context"
	"crypto/x509"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
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
