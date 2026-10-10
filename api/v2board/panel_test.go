package panel

import (
	"context"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUserListAndFailedReport(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	body := `{"users":[]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	defer s.Close()
	c, err := New(&conf.NodeConfig{APIHost: s.URL, NodeID: 1, Key: "test", RetryCount: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	users, err := c.GetUserList(context.Background())
	if err != nil || users == nil || len(users) != 0 {
		t.Fatalf("empty list must be distinct from unchanged: %v %v", users, err)
	}
	status.Store(304)
	users, err = c.GetUserList(context.Background())
	if err != nil || users != nil {
		t.Fatal("304 should mean unchanged")
	}
	status.Store(500)
	if err = c.ReportUserTraffic(context.Background(), []UserTraffic{{UID: 1, Upload: 5}}); err == nil {
		t.Fatal("HTTP failure accepted")
	}
	if _, err = c.GetUserList(context.Background()); err == nil {
		t.Fatal("HTTP failure accepted as user list")
	}
}
