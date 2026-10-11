package limiter

import (
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
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
	l.online[key]["127.0.0.1"] = time.Now().Add(-3 * time.Minute)
	devices, _ := l.GetOnlineDevice()
	if len(*devices) != 1 {
		t.Fatal("report lost observed activity")
	}
	if len(l.online) != 0 {
		t.Fatal("inactive IP state retained after report")
	}
}

func TestExpiredDeviceDoesNotOccupyLimit(t *testing.T) {
	l := AddLimiter("vless", "expiry", []panel.UserInfo{{Id: 1, Uuid: "u", DeviceLimit: 1}}, nil)
	defer DeleteLimiter("expiry")
	key := format.UserTag("expiry", "u")
	l.CheckLimit(key, "127.0.0.1", true)
	l.online[key]["127.0.0.1"] = time.Now().Add(-3 * time.Minute)
	if _, denied := l.CheckLimit(key, "127.0.0.2", true); denied {
		t.Fatal("expired device consumed limit")
	}
}
