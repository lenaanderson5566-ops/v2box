package core

import (
	"context"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"testing"
	"time"
)

func TestSharedTrafficBudgetLimitAndRecovery(t *testing.T) {
	v := &V2Core{tracker: newTrafficTracker()}
	b := &panel.TrafficBudget{ID: 1, LimitBytes: 100, UsedBytes: 10, CountingMode: "both", PeriodStart: "2026-10", PeriodEndUnix: time.Now().Add(time.Hour).Unix()}
	v.SetTrafficBudget("a", b)
	v.SetTrafficBudget("b", b)
	v.tracker.account("a", 1, 50, 20)
	if got := v.TrafficBudgetSnapshot("b"); got.UsedBytes != 80 {
		t.Fatal("shared nodes not aggregated", got)
	}
	v.tracker.account("b", 2, 10, 10)
	if !v.tracker.budgetBlockedLocked("a") || !v.tracker.budgetBlockedLocked("b") {
		t.Fatal("limit not enforced on all nodes")
	}
	m := &meter{tracker: v.tracker, tag: "a", ctx: context.Background()}
	if m.wait(1) == nil {
		t.Fatal("existing traffic not blocked")
	}
	b.LimitBytes = 200
	b.UsedBytes = 80
	v.SetTrafficBudget("a", b)
	if v.tracker.budgetBlockedLocked("a") || v.tracker.budgetBlockedLocked("b") {
		t.Fatal("higher limit did not recover")
	}
	if v.TrafficBudgetSnapshot("a").UsedBytes != 100 {
		t.Fatal("stale panel response reduced live usage")
	}
	b.PeriodStart = "2026-11"
	b.UsedBytes = 0
	v.SetTrafficBudget("a", b)
	if v.TrafficBudgetSnapshot("a").UsedBytes != 0 {
		t.Fatal("new cycle not reset")
	}
	v.SetTrafficBudget("a", nil)
	if v.TrafficBudgetSnapshot("a") != nil {
		t.Fatal("removed quota still active")
	}
}
func TestTrafficBudgetCountingModes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int64
	}{{"both", 30}, {"upload", 10}, {"download", 20}} {
		v := &V2Core{tracker: newTrafficTracker()}
		v.SetTrafficBudget("a", &panel.TrafficBudget{ID: 1, CountingMode: tc.mode, PeriodStart: "x"})
		v.tracker.account("a", 1, 10, 20)
		if v.TrafficBudgetSnapshot("a").UsedBytes != tc.want {
			t.Fatal(tc.mode)
		}
	}
}

func TestTrafficBudgetClosesExistingConnectionsAndRecovers(t *testing.T) {
	limiter.Init()
	echo := echoServer(t)
	n := testNode(t, "vless")
	v := startTestNode(t, n)
	client := clientBox(t, n, testUUID)
	b := &panel.TrafficBudget{ID: 1, LimitBytes: 1000, CountingMode: "both", PeriodStart: "2026-10", PeriodEndUnix: time.Now().Add(time.Hour).Unix()}
	v.SetTrafficBudget(n.Tag, b)
	existing := persistent(t, client, echo.Addr())
	b.LimitBytes = 1
	v.SetTrafficBudget(n.Tag, b)
	if roundTrip(existing) == nil {
		t.Fatal("existing connection survived exhausted quota")
	}
	assertDenied(t, client, echo.Addr())
	b.LimitBytes = 100000
	v.SetTrafficBudget(n.Tag, b)
	persistent(t, client, echo.Addr())
}
func TestTrafficBudgetUDPRecovery(t *testing.T) {
	limiter.Init()
	echo := udpEcho(t)
	n := testNode(t, "vless")
	v := startTestNode(t, n)
	client := clientBox(t, n, testUUID)
	b := &panel.TrafficBudget{ID: 1, LimitBytes: 1000, CountingMode: "both", PeriodStart: "2026-10", PeriodEndUnix: time.Now().Add(time.Hour).Unix()}
	v.SetTrafficBudget(n.Tag, b)
	existing := packetClient(t, client, echo.LocalAddr())
	if err := packetRoundTrip(existing, echo.LocalAddr(), "before"); err != nil {
		t.Fatal(err)
	}
	b.UsedBytes = 1000
	v.SetTrafficBudget(n.Tag, b)
	if packetRoundTrip(existing, echo.LocalAddr(), "blocked") == nil {
		t.Fatal("UDP survived exhausted quota")
	}
	b.LimitBytes = 100000
	v.SetTrafficBudget(n.Tag, b)
	fresh := packetClient(t, client, echo.LocalAddr())
	if err := packetRoundTrip(fresh, echo.LocalAddr(), "recovered"); err != nil {
		t.Fatal(err)
	}
}
