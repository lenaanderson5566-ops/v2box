package panel

import (
	"context"
	"fmt"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrafficBudgetPersistenceAndHigherLimit(t *testing.T) {
	limit := 100
	fail := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"data":{"id":1,"limitBytes":%d,"usedBytes":40,"countingMode":"both","periodStart":"2026-10","periodEndUnix":%d}}`, limit, time.Now().Add(time.Hour).Unix())
	}))
	defer server.Close()
	config := &conf.NodeConfig{NodeType: "v2box", NodeID: 7, APIHost: server.URL, TrafficSpoolDirectory: t.TempDir()}
	c, _ := newTestPanel(t, server, config)
	c.budgetSupported.Store(true)
	budget, err := c.GetTrafficBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	budget.UsedBytes = 100
	if err = c.SaveTrafficBudget(budget); err != nil {
		t.Fatal(err)
	}
	replacement, _ := newTestPanel(t, server, config)
	replacement.budgetSupported.Store(true)
	limit = 200
	budget, err = replacement.GetTrafficBudget(context.Background())
	if err != nil || budget.UsedBytes != 100 || budget.LimitBytes != 200 {
		t.Fatal("restarted node lost usage or higher limit", err, budget)
	}
	fail = true
	if _, err = replacement.GetTrafficBudget(context.Background()); err == nil {
		t.Fatal("outage must not clear restrictions")
	}
}

func TestMalformedTrafficBudgetDoesNotDisableRestrictions(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer server.Close()
	c, _ := newTestPanel(t, server, &conf.NodeConfig{NodeType: "v2box", NodeID: 7, APIHost: server.URL, TrafficSpoolDirectory: t.TempDir()})
	c.budgetSupported.Store(true)
	if _, err := c.GetTrafficBudget(context.Background()); err == nil {
		t.Fatal("missing data silently disabled quota")
	}
}
