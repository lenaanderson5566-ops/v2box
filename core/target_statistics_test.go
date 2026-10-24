package core

import (
	"encoding/json"
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

func TestTargetConcurrentDrainPreservesExactCounters(t *testing.T) {
	v := New(conf.New())
	v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := &meter{tag: "n", uid: 1, targetFixed: true}
			for j := 0; j < 2000; j++ {
				v.tracker.targets.record(m, M.ParseSocksaddr("example.org:443"), 1, 2)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var up, down, connections int64
	drain := func() {
		if b := v.TargetBatch("n"); b != nil {
			for _, r := range b.Records {
				up += r.UploadBytes
				down += r.DownloadBytes
				connections += r.Connections
			}
			v.AckTargets("n", b.BatchID, true)
		}
	}
	for {
		drain()
		select {
		case <-done:
			drain()
			if up != 16000 || down != 32000 || connections != 8 {
				t.Fatalf("lost counters: %d %d %d", up, down, connections)
			}
			return
		default:
		}
	}
}

func TestExpiredTargetBatchPreservesTruncationNotice(t *testing.T) {
	v := New(conf.New())
	v.SetTargetPolicy("n", panel.TargetPolicy{Enabled: true, Version: "v", LeaseSeconds: 120})
	v.tracker.targets.record(&meter{tag: "n", uid: 1, targetFixed: true}, M.ParseSocksaddr("example.org:443"), 1, 2)
	state := v.tracker.targets.nodes["n"]
	state.drop(7)
	old := v.TargetBatch("n")
	state.pendingAt = time.Now().Add(-targetPendingTTL)
	next := v.TargetBatch("n")
	if next == nil || next.BatchID == old.BatchID || next.Dropped != 8 || len(next.Records) != 0 {
		t.Fatalf("lost expiry/truncation notice: %+v", next)
	}
	if old.Records[0].UploadBytes != 1 || old.Dropped != 7 {
		t.Fatal("pending snapshot mutated")
	}
}

func TestTargetFullBatchWireBudget(t *testing.T) {
	b := panel.TargetBatch{Version: "a80e93b1-0cba-478a-b780-0d8eb6118267", BatchID: "a80e93b1-0cba-478a-b780-0d8eb6118267"}
	for i := 0; i < targetRowLimit; i++ {
		b.Records = append(b.Records, panel.TargetRecord{UserID: i + 1, Target: fmt.Sprintf("cdn-%d.example.org", i), Type: "domain", Connections: 100, UploadBytes: 1048576, DownloadBytes: 10485760})
	}
	wire, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > 262144 {
		t.Fatal("bounded batch exceeds panel limit")
	}
	t.Logf("500 representative rows: %d JSON bytes; 20 nodes every 60 seconds: %.2f GiB/day at continuous full capacity (before HTTP/TLS)", len(wire), float64(len(wire))*20*1440/(1<<30))
}
