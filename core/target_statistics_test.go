package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	M "github.com/sagernet/sing/common/metadata"
)

func BenchmarkTargetStatistics(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		for _, target := range []string{"192.0.2.1:443", "example.org:443"} {
			b.Run(fmt.Sprintf("enabled=%t/%s", enabled, target), func(b *testing.B) {
				v := New(conf.New())
				v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: enabled, Version: "v", LeaseSeconds: 120})
				d := M.ParseSocksaddr(target)
				var ids atomic.Int64
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					m := &meter{tag: "n", uid: int(ids.Add(1))}
					for pb.Next() {
						v.tracker.targets.record(m, d, 1024, 1024)
					}
				})
			})
		}
	}
}

func TestTargetStatisticsSwitchLeaseAndRetry(t *testing.T) {
	v := New(conf.New())
	m := &meter{tag: "node", uid: 1}
	d := M.ParseSocksaddr("Example.COM:443")
	record := func() { v.tracker.targets.record(m, d, 100, 200) }
	record()
	if v.TargetBatch("node") != nil {
		t.Fatal("disabled collected")
	}
	p := panel.TargetPolicy{Enabled: true, Version: "one", LeaseSeconds: 120}
	v.SetTargetPolicy("node", p)
	record()
	record()
	b := v.TargetBatch("node")
	if b == nil || len(b.Records) != 1 || b.Records[0].Target != "example.com" || b.Records[0].Connections != 1 || b.Records[0].UploadBytes != 200 {
		t.Fatalf("bad aggregation: %+v", b)
	}
	if v.TargetBatch("node").BatchID != b.BatchID {
		t.Fatal("retry ID changed")
	}
	record()
	v.AckTargets("node", b.BatchID, true)
	if next := v.TargetBatch("node"); next.Records[0].UploadBytes != 100 || next.Records[0].Connections != 0 {
		t.Fatal("pending snapshot changed")
	}
	v.SetTargetPolicy("node", panel.TargetPolicy{})
	record()
	if v.TargetBatch("node") != nil {
		t.Fatal("disable retained pending")
	}
	p.Version = "two"
	v.SetTargetPolicy("node", p)
	record()
	if b = v.TargetBatch("node"); b.Records[0].UploadBytes != 100 || b.Records[0].Connections != 1 {
		t.Fatal("old generation leaked")
	}
	v.tracker.targets.nodes["node"].until.Store(time.Now().Add(-time.Second).UnixNano())
	record()
	if v.TargetBatch("node") != nil {
		t.Fatal("expired lease collected")
	}
}
func TestTargetStatisticsAddressesAndBounds(t *testing.T) {
	for raw, want := range map[string]string{"[2001:db8::1]:443": "2001:db8::1", "192.0.2.1:80": "192.0.2.1", "EXAMPLE.org.:443": "example.org"} {
		got, _ := targetAddress(M.ParseSocksaddr(raw))
		if got != want {
			t.Fatalf("%s: %s", raw, got)
		}
	}
	m := &meter{target: M.ParseSocksaddr("one.example:53"), resolvedTarget: M.ParseSocksaddr("one.example:53")}
	other := M.ParseSocksaddr("two.example:53")
	if packetTarget(m, other) != other {
		t.Fatal("UDP destinations merged")
	}
	ip := M.ParseSocksaddr("192.0.2.1:443")
	if originalTarget(other, ip, M.Socksaddr{}) != ip {
		t.Fatal("rewritten target leaked")
	}
	v := New(conf.New())
	v.SetTargetPolicy("node", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	for i := 0; i < 550; i++ {
		v.tracker.targets.record(&meter{tag: "node", uid: i + 1}, ip, 1, 2)
	}
	b := v.TargetBatch("node")
	if len(b.Records) != 500 || b.Dropped != 50 {
		t.Fatal("unbounded aggregate")
	}
}
func TestTargetStatisticsConcurrentSwitch(t *testing.T) {
	v := New(conf.New())
	var wg sync.WaitGroup
	m := &meter{tag: "n", uid: 1}
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: i%2 == 0, Version: fmt.Sprint(i), LeaseSeconds: 120})
			if batch := v.TargetBatch("n"); batch != nil {
				v.AckTargets("n", batch.BatchID, true)
			}
		}
	}()
	wg.Wait()
	v.SetTargetPolicy("n", panel.TargetPolicy{})
	if v.TargetBatch("n") != nil {
		t.Fatal("disabled data remained")
	}
}

func TestTargetStatisticsPortsRetryExpiryAndRenewal(t *testing.T) {
	v := New(conf.New())
	p := panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120}
	v.SetTargetPolicy("n", p)
	m := &meter{tag: "n", uid: 1}
	for port := 1; port <= 1000; port++ {
		v.tracker.targets.record(m, M.ParseSocksaddr(fmt.Sprintf("example.org:%d", port)), 1, 2)
	}
	b := v.TargetBatch("n")
	if len(m.targetNames) != 1 || len(b.Records) != 1 || b.Records[0].UploadBytes != 1000 || b.Dropped != 0 {
		t.Fatal("ports consumed target capacity")
	}
	v.tracker.targets.nodes["n"].pendingAt = time.Now().Add(-targetPendingTTL)
	v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 7, 9)
	next := v.TargetBatch("n")
	if next.BatchID == b.BatchID || next.Records[0].UploadBytes != 7 || next.Dropped != 1 {
		t.Fatal("expired retry retained or merged")
	}
	v.AckTargets("n", b.BatchID, true)
	if v.TargetBatch("n").BatchID != next.BatchID {
		t.Fatal("stale acknowledgement cleared new batch")
	}
	v.tracker.targets.nodes["n"].until.Store(time.Now().Add(-time.Second).UnixNano())
	v.SetTargetPolicy("n", p)
	v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
	if v.TargetBatch("n").Records[0].Connections != 1 {
		t.Fatal("renewed lease reused expired meter generation")
	}
}
func TestTargetStatisticsConcurrentBounds(t *testing.T) {
	v := New(conf.New())
	v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	var wg sync.WaitGroup
	for user := 1; user <= 32; user++ {
		wg.Add(1)
		go func(uid int) {
			defer wg.Done()
			m := &meter{tag: "n", uid: uid}
			for i := 0; i < 100; i++ {
				v.tracker.targets.record(m, M.ParseSocksaddr(fmt.Sprintf("host%d.example:443", i)), 1, 2)
			}
			if len(m.targetNames) > 64 || len(m.targetSeen) > 64 {
				t.Error("unbounded meter cache")
			}
		}(user)
	}
	wg.Wait()
	b := v.TargetBatch("n")
	if len(b.Records) != 500 || b.Dropped != 2700 {
		t.Fatalf("incorrect bounded aggregate: rows=%d dropped=%d", len(b.Records), b.Dropped)
	}
}
func TestTargetStatisticsLifecycleResources(t *testing.T) {
	v := New(conf.New())
	baseline := sampleResources()
	for cycle := 0; cycle < 100; cycle++ {
		v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
		for uid := 1; uid <= 550; uid++ {
			v.tracker.targets.record(&meter{tag: "n", uid: uid}, M.ParseSocksaddr("example.org:443"), 1, 2)
		}
		v.TargetBatch("n")
		v.SetTargetPolicy("n", panel.TargetPolicy{})
	}
	if len(v.tracker.targets.nodes) != 0 || len(v.tracker.targets.view.Load().nodes) != 0 {
		t.Fatal("retained retired nodes")
	}
	checkResourceGrowth(t, baseline, sampleResources())
}

func TestTargetCacheNodeBudgetsCloseAndRetire(t *testing.T) {
	v := New(conf.New())
	v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	state := v.tracker.targets.nodes["n"]
	meters := make([]*meter, 0, targetCacheMeterLimit)
	for i := 0; i < targetCacheMeterLimit+100; i++ {
		m := &meter{tag: "n", uid: 1}
		v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
		if i < targetCacheMeterLimit {
			meters = append(meters, m)
		}
	}
	if state.cacheMeters.Load() != targetCacheMeterLimit || state.cacheEntries.Load() != targetCacheMeterLimit {
		t.Fatal("unbounded meter cache")
	}
	v.tracker.targets.release(meters[0])
	v.tracker.targets.release(meters[0])
	v.tracker.targets.record(meters[0], M.ParseSocksaddr("example.org:443"), 1, 2)
	if meters[0].targetNames != nil || state.cacheMeters.Load() != targetCacheMeterLimit-1 {
		t.Fatal("closed meter retained/repopulated cache")
	}
	for _, m := range meters[1:] {
		for i := 0; i < 64; i++ {
			v.tracker.targets.record(m, M.ParseSocksaddr(fmt.Sprintf("host%d.example:443", i)), 1, 2)
		}
	}
	if state.cacheEntries.Load() != targetCacheEntryLimit {
		t.Fatalf("entry limit %d", state.cacheEntries.Load())
	}
	v.SetTargetPolicy("n", panel.TargetPolicy{})
	for _, m := range meters {
		if m.targetSeen != nil || m.targetNames != nil {
			t.Fatal("inactive connection retained retired targets")
		}
	}
}

func TestTargetCacheConcurrentCloseAndSwitch(t *testing.T) {
	v := New(conf.New())
	m := &meter{tag: "n", uid: 1}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 100; n++ {
			v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: n%2 == 0, Version: fmt.Sprint(n), LeaseSeconds: 120})
			v.tracker.targets.release(m)
		}
	}()
	wg.Wait()
	v.SetTargetPolicy("n", panel.TargetPolicy{})
	if m.targetNames != nil {
		t.Fatal("closed target cache retained")
	}
}

func TestTargetTCPFixedCache(t *testing.T) {
	v := New(conf.New())
	p := panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120}
	v.SetTargetPolicy("n", p)
	m := &meter{tag: "n", uid: 1, targetFixed: true}
	for i := 0; i < 1000; i++ {
		v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
	}
	state := v.tracker.targets.nodes["n"]
	batch := v.TargetBatch("n")
	if m.targetSeen != nil || m.targetNames != nil || state.cacheEntries.Load() != 0 || len(batch.Records) != 1 || batch.Records[0].Connections != 1 || batch.Records[0].UploadBytes != 1000 {
		t.Fatal("fixed TCP allocated maps or counted incorrectly")
	}
	v.AckTargets("n", batch.BatchID, true)
	p.Version = "new"
	v.SetTargetPolicy("n", p)
	v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
	if v.TargetBatch("n").Records[0].Connections != 1 {
		t.Fatal("TCP generation not reset")
	}
	v.tracker.targets.release(m)
	v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
}

func BenchmarkTargetStatisticsTCP(b *testing.B) {
	v := New(conf.New())
	v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	d := M.ParseSocksaddr("example.org:443")
	var ids atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		m := &meter{tag: "n", uid: int(ids.Add(1)), targetFixed: true}
		for pb.Next() {
			v.tracker.targets.record(m, d, 1024, 1024)
		}
	})
}
