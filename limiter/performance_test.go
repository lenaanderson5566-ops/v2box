package limiter

import (
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkCheckLimit(b *testing.B) {
	for _, count := range []int{1, 64, 1024} {
		b.Run(fmt.Sprintf("IPs=%d", count), func(b *testing.B) {
			l := AddLimiter("vless", "bench", []panel.UserInfo{{Id: 1, Uuid: "u"}}, nil)
			defer DeleteLimiter("bench")
			key := format.UserTag("bench", "u")
			for i := 0; i < count; i++ {
				l.CheckLimit(key, fmt.Sprintf("192.0.%d.%d", i/256, i%256), true)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					l.CheckLimit(key, "192.0.0.0", true)
				}
			})
		})
	}
}

func TestReportPrunesInactiveDevices(t *testing.T) {
	l := AddLimiter("vless", "prune", []panel.UserInfo{{Id: 1, Uuid: "u"}}, nil)
	defer DeleteLimiter("prune")
	key := format.UserTag("prune", "u")
	l.CheckLimit(key, "127.0.0.1", true)
	l.shard(key).online[key]["127.0.0.1"] = time.Now().Add(-3 * time.Minute)
	devices, _ := l.GetOnlineDevice()
	if len(*devices) != 1 {
		t.Fatal("report lost observed activity")
	}
	if len(l.shard(key).online) != 0 {
		t.Fatal("inactive IP state retained after report")
	}
}

func TestExpiredDeviceDoesNotOccupyLimit(t *testing.T) {
	l := AddLimiter("vless", "expiry", []panel.UserInfo{{Id: 1, Uuid: "u", DeviceLimit: 1}}, nil)
	defer DeleteLimiter("expiry")
	key := format.UserTag("expiry", "u")
	l.CheckLimit(key, "127.0.0.1", true)
	l.shard(key).online[key]["127.0.0.1"] = time.Now().Add(-3 * time.Minute)
	if _, denied := l.CheckLimit(key, "127.0.0.2", true); denied {
		t.Fatal("expired device consumed limit")
	}
}

func BenchmarkCheckLimitManyUsers(b *testing.B) {
	users := make([]panel.UserInfo, 1024)
	keys := make([]string, len(users))
	for i := range users {
		users[i] = panel.UserInfo{Id: i + 1, Uuid: fmt.Sprintf("user-%d", i)}
		keys[i] = format.UserTag("many", users[i].Uuid)
	}
	l := AddLimiter("vless", "many", users, nil)
	defer DeleteLimiter("many")
	for _, key := range keys {
		l.CheckLimit(key, "127.0.0.1", true)
	}
	var worker atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(worker.Add(1))
		for pb.Next() {
			l.CheckLimit(keys[i%len(keys)], "127.0.0.1", true)
			i++
		}
	})
}

func TestConcurrentLimitChecksShareBucketAndReportDevices(t *testing.T) {
	const count = 64
	users := make([]panel.UserInfo, count)
	for i := range users {
		users[i] = panel.UserInfo{Id: i + 1, Uuid: fmt.Sprintf("user-%d", i), SpeedLimit: 10, DeviceLimit: 1}
	}
	l := AddLimiter("vless", "parallel", users, nil)
	defer DeleteLimiter("parallel")
	sharedKey := format.UserTag("parallel", users[0].Uuid)
	sharedBucket, _ := l.CheckLimit(sharedKey, "127.0.0.1", true)
	var wg sync.WaitGroup
	for _, u := range users {
		wg.Add(1)
		go func(u panel.UserInfo) {
			defer wg.Done()
			key := format.UserTag("parallel", u.Uuid)
			first, reject := l.CheckLimit(key, "127.0.0.1", true)
			if reject || first == nil {
				t.Error("admission failed")
				return
			}
			for i := 0; i < 100; i++ {
				if bucket, reject := l.CheckLimit(sharedKey, "127.0.0.1", true); reject || bucket != sharedBucket {
					t.Error("concurrent connections did not share the user bucket")
					return
				}
				bucket, reject := l.CheckLimit(key, "127.0.0.1", true)
				if reject || bucket != first {
					t.Error("aggregate bucket was replaced")
					return
				}
			}
			if _, reject := l.CheckLimit(key, "127.0.0.2", true); !reject {
				t.Error("device limit bypassed")
			}
		}(u)
	}
	wg.Wait()
	devices, err := l.GetOnlineDevice()
	if err != nil || len(*devices) != count {
		t.Fatalf("lost observed devices: %d, %v", len(*devices), err)
	}
	devices, _ = l.GetOnlineDevice()
	if len(*devices) != 0 {
		t.Fatal("devices reported twice")
	}
	l.UpdateUser("parallel", nil, users, nil)
	for _, u := range users {
		if _, reject := l.CheckLimit(format.UserTag("parallel", u.Uuid), "127.0.0.1", true); !reject {
			t.Fatal("revoked user accepted")
		}
	}
}

func TestConcurrentLimiterUpdatesAndReporting(t *testing.T) {
	user := panel.UserInfo{Id: 1, Uuid: "u", SpeedLimit: 10}
	l := AddLimiter("vless", "updates", []panel.UserInfo{user}, nil)
	defer DeleteLimiter("updates")
	key := format.UserTag("updates", "u")
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				switch worker {
				case 0:
					if _, reject := l.CheckLimit(key, "127.0.0.1", true); reject {
						t.Error("active user rejected")
					}
				case 1:
					l.SetAlive(map[int]int{1: 0})
					l.UpdateUser("updates", nil, nil, []panel.UserInfo{user})
				case 2:
					if err := l.UpdateDynamicSpeedLimit("updates", "u", 5, time.Now().Add(time.Minute)); err != nil {
						t.Error(err)
					}
				case 3:
					l.GetOnlineDevice()
				}
			}
		}(worker)
	}
	wg.Wait()
}
