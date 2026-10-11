package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
)

type resourceSample struct {
	heap       uint64
	goroutines int
}

func captureHeap(t *testing.T, label string) {
	t.Helper()
	if dir := os.Getenv("V2BOX_PROFILE_DIR"); dir != "" {
		file, err := os.Create(filepath.Join(dir, label+".pprof"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := pprof.WriteHeapProfile(file); err != nil {
			t.Fatal(err)
		}
	}
}

func sampleResources() resourceSample {
	// Two collections also clear objects kept alive by a previous sync.Pool cycle.
	// The independent native H2 client's closed-connection cache lives for 1s.
	time.Sleep(1100 * time.Millisecond)
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return resourceSample{m.HeapAlloc, runtime.NumGoroutine()}
}
func checkResourceGrowth(t *testing.T, baseline, current resourceSample) {
	t.Helper()
	t.Logf("live heap=%d KiB (delta=%+d KiB), goroutines=%d (delta=%+d)", current.heap/1024, (int64(current.heap)-int64(baseline.heap))/1024, current.goroutines, current.goroutines-baseline.goroutines)
	if current.goroutines > baseline.goroutines+8 || int64(current.heap)-int64(baseline.heap) > 2<<20 {
		var stacks bytes.Buffer
		pprof.Lookup("goroutine").WriteTo(&stacks, 2)
		t.Fatalf("resources failed to settle after teardown:\n%s", stacks.String())
	}
}

// Opt-in bounded loopback soak. Subtest cleanups run before measurement so the
// test harness does not itself retain every closed Box and client connection.
func TestCoreLifecycleSoak(t *testing.T) {
	if os.Getenv("V2BOX_SOAK") != "1" {
		t.Skip("set V2BOX_SOAK=1")
	}
	limiter.Init()
	cert := testCertificate(t)
	echo := echoServer(t)
	var baseline resourceSample
	for batch := 0; batch < 6; batch++ {
		for cycle := 0; cycle < 3; cycle++ {
			for _, protocol := range []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic", "anytls", "naive"} {
				if !t.Run(fmt.Sprintf("%d/%d/%s", batch, cycle, protocol), func(t *testing.T) {
					n := testNode(t, protocol)
					if protocol == "shadowsocks" {
						n.Common.Cipher = "aes-128-gcm"
					}
					if protocol == "trojan" || protocol == "hysteria2" || protocol == "tuic" || protocol == "anytls" || protocol == "naive" {
						n.Security = panel.Tls
						n.Common.CertInfo = cert
					}
					v := startTestNode(t, n)
					if protocol == "naive" {
						client := naiveClient(t, n)
						s, err := naiveConnect(client, testUUID, echo.Addr().String())
						if err != nil {
							t.Fatal(err)
						}
						defer s.Close()
						if err := naiveEcho(s, []byte("shutdown-active-stream")); err != nil {
							t.Fatal(err)
						}
					} else {
						client := clientBox(t, n, testUUID)
						persistent(t, client, echo.Addr())
					}
					if err := v.Close(); err != nil {
						t.Fatal(err)
					}
					v.tracker.mu.Lock()
					remaining := len(v.tracker.connections)
					v.tracker.mu.Unlock()
					if remaining != 0 {
						t.Fatal("closed core retains routed connections")
					}
				}) {
					return
				}
			}
		}
		current := sampleResources()
		captureHeap(t, fmt.Sprintf("lifecycle-%d", batch))
		if batch == 0 {
			baseline = current
		}
		checkResourceGrowth(t, baseline, current)
	}
}

func TestNaiveMultiplexChurnSoak(t *testing.T) {
	if os.Getenv("V2BOX_SOAK") != "1" {
		t.Skip("set V2BOX_SOAK=1")
	}
	limiter.Init()
	n := testNode(t, "naive")
	n.Security = panel.Tls
	n.Common.CertInfo = testCertificate(t)
	v := startTestNode(t, n)
	echo := echoServer(t)
	payload := bytes.Repeat([]byte("churn"), 8192)
	var baseline resourceSample
	started := time.Now()
	for batch := 0; batch < 5; batch++ {
		for session := 0; session < 8; session++ {
			if !t.Run(fmt.Sprintf("%d/%d", batch, session), func(t *testing.T) {
				client := naiveClient(t, n)
				var wg sync.WaitGroup
				for worker := 0; worker < 8; worker++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; i < 4; i++ {
							s, err := naiveConnect(client, testUUID, echo.Addr().String())
							if err != nil {
								t.Error(err)
								return
							}
							if err = naiveEcho(s, payload); err != nil {
								t.Error(err)
							}
							s.Close()
						}
					}()
				}
				wg.Wait()
			}) {
				return
			}
		}
		v.GetUserTrafficSlice(n.Tag, 0)
		v.TakeOnlineActivity(n.Tag)
		current := sampleResources()
		captureHeap(t, fmt.Sprintf("naive-%d", batch))
		if batch == 0 {
			baseline = current
		}
		checkResourceGrowth(t, baseline, current)
	}
	t.Logf("1280 CONNECT streams, 40 TLS sessions, %.1f MiB echoed in %s", float64(1280*len(payload))/(1<<20), time.Since(started))
	if os.Getenv("V2BOX_PROFILE_DIR") != "" {
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		closed := sampleResources()
		t.Logf("after server close: heap=%d KiB goroutines=%d", closed.heap/1024, closed.goroutines)
		captureHeap(t, "naive-closed")
	}
}

func TestTrackerEmptyNodesDoNotAccumulate(t *testing.T) {
	tracker := newTrafficTracker()
	for i := 0; i < 1000; i++ {
		tag := fmt.Sprintf("node-%d", i)
		tracker.setUsers(tag, []panel.UserInfo{{Id: 1, Uuid: testUUID}})
		tracker.setUsers(tag, nil)
		tracker.closeNode(tag)
	}
	if len(tracker.users) != 0 {
		t.Fatalf("retained %d empty node tables", len(tracker.users))
	}
	tracker.account("n", 1, 0, 0)
	if len(tracker.traffic) != 0 || len(tracker.activity) != 0 {
		t.Fatal("zero-byte I/O creates unreportable accounting entries")
	}
}

func BenchmarkTrafficAccount(b *testing.B) {
	tracker := newTrafficTracker()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.account("node", 1, 16384, 0)
		}
	})
}
