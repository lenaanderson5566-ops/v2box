package node

import (
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"testing"
)

func TestCompareUserListIdentityAndLimits(t *testing.T) {
	old := []panel.UserInfo{{Id: 1, Uuid: "a"}, {Id: 2, Uuid: "b"}}
	next := []panel.UserInfo{{Id: 3, Uuid: "a"}, {Id: 2, Uuid: "b", SpeedLimit: 10}}
	deleted, added, modified := compareUserList(old, next)
	if len(deleted) != 1 || deleted[0].Id != 1 || len(added) != 1 || added[0].Id != 3 || len(modified) != 1 || modified[0].Id != 2 {
		t.Fatalf("bad diff: %+v %+v %+v", deleted, added, modified)
	}
}
