package limiter

import (
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"testing"
)

func TestLimitsAndRevocation(t *testing.T) {
	Init()
	u := panel.UserInfo{Id: 1, Uuid: "u", SpeedLimit: 1, DeviceLimit: 1}
	l := AddLimiter("vless", "n", []panel.UserInfo{u}, nil)
	key := format.UserTag("n", "u")
	b, reject := l.CheckLimit(key, "127.0.0.1", true)
	if reject || b == nil {
		t.Fatal("first IP rejected or speed limit missing")
	}
	if _, reject = l.CheckLimit(key, "127.0.0.2", true); !reject {
		t.Fatal("second device accepted")
	}
	if _, reject = l.CheckLimit(key, "127.0.0.1", true); reject {
		t.Fatal("existing device rejected")
	}
	u.SpeedLimit = 0
	l.UpdateUser("n", nil, nil, []panel.UserInfo{u})
	if b, _ = l.CheckLimit(key, "127.0.0.1", true); b != nil {
		t.Fatal("speed limit not removed")
	}
	l.UpdateUser("n", nil, []panel.UserInfo{u}, nil)
	if _, reject = l.CheckLimit(key, "127.0.0.1", true); !reject {
		t.Fatal("deleted user accepted")
	}
}
