package panel

import (
	"context"
	"encoding/json"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Optional fixture exported by the actual panel HTTP contract test, never a
// production response. This checks PHP/Go serialization interoperability.
func TestV10PanelFixture(t *testing.T) {
	name := os.Getenv("V2BOX_PANEL_FIXTURE")
	if name == "" {
		t.Skip("panel fixture not provided")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.Write([]byte(`{"data":{"accepted":true}}`))
			return
		}
		w.Write(data)
	}))
	defer s.Close()
	c, err := New(&conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 1, Key: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n.Type != "vless" || n.Common.ServerPort != 443 || len(n.Common.Routes) != 1 || len(n.Common.SingBox) == 0 {
		t.Fatal("PHP/Go configuration mismatch")
	}
}

func TestV10Contract(t *testing.T) {
	ctx := context.Background()
	usersBody := `{"data":[{"id":42,"uuid":"user","speedLimit":10,"deviceLimit":2}]}`
	badAck := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.URL.RawQuery != "" {
			t.Error("V10 must use header credentials only")
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v10/nodes/v2box/7/") {
			t.Error(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/v10/nodes/v2box/7/") {
		case "capabilities":
			if r.Method != "POST" {
				t.Error("capabilities method")
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body["features"].([]any)) != 3 {
				t.Error("capabilities missing")
			}
			w.Write([]byte(`{"data":{"accepted":true}}`))
		case "configuration":
			w.Write([]byte(`{"data":{"protocol":"vless","listenAddress":"127.0.0.1","listenPort":443,"network":"tcp","tls":0,"tlsSettings":{"serverName":"example.org"},"intervals":{"pushInterval":60,"pullInterval":60},"routes":[{"id":1,"match":["domain:example.org"],"action":"block","actionValue":null}],"singBox":{"route":{"final":"direct"}}}}`))
		case "users":
			w.Header().Set("ETag", "users-v1")
			if r.Header.Get("If-None-Match") == "users-v1" {
				w.WriteHeader(304)
			} else {
				w.Write([]byte(usersBody))
			}
		case "online-counts":
			w.Write([]byte(`{"data":[{"userId":42,"count":2}]}`))
		case "traffic":
			var body struct {
				Records []struct {
					ID     int   `json:"userId"`
					Upload int64 `json:"uploadBytes"`
				} `json:"records"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Records) != 1 || body.Records[0].ID != 42 || body.Records[0].Upload != 123 {
				t.Error("traffic contract")
			}
			w.WriteHeader(202)
			if badAck {
				w.Write([]byte(`{"data":{"accepted":false}}`))
			} else {
				w.Write([]byte(`{"data":{"accepted":true}}`))
			}
		case "online-users":
			var body struct {
				Records []struct {
					IPs []string `json:"ips"`
				} `json:"records"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Records) != 1 || body.Records[0].IPs[0] != "127.0.0.1" {
				t.Error("online contract")
			}
			w.Write([]byte(`{"data":{"accepted":true}}`))
		default:
			t.Error("unexpected path")
		}
	}))
	defer s.Close()
	c, err := New(&conf.NodeConfig{APIHost: s.URL, NodeType: "v2box", NodeID: 7, Key: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.GetNodeInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Common.ServerPort != 443 || n.Common.TlsSettings.ServerName != "example.org" || len(n.Common.Routes) != 1 || len(n.Common.SingBox) == 0 {
		t.Fatal("configuration mapping")
	}
	u, err := c.GetUserList(ctx)
	if err != nil || len(u) != 1 || u[0].SpeedLimit != 10 {
		t.Fatalf("users: %v %v", u, err)
	}
	u, err = c.GetUserList(ctx)
	if err != nil || u != nil {
		t.Fatal("304 semantics")
	}
	a, err := c.GetUserAlive(ctx)
	if err != nil || a[42] != 2 {
		t.Fatal("online counts")
	}
	if err = c.ReportUserTraffic(ctx, []UserTraffic{{UID: 42, Upload: 123}}); err != nil {
		t.Fatal(err)
	}
	badAck = true
	if c.ReportUserTraffic(ctx, []UserTraffic{{UID: 42, Upload: 123}}) == nil {
		t.Fatal("missing ack accepted")
	}
	data := map[int][]string{42: {"127.0.0.1_node"}}
	if err = c.ReportNodeOnlineUsers(ctx, &data); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"data":[]}`, `{"data":null}`, `{}`, `{"data":[{"id":1,"uuid":"a"},{"id":1,"uuid":"b"}]}`} {
		usersBody = body
		c.InvalidateUserCache()
		u, err = c.GetUserList(ctx)
		if body == `{"data":[]}` {
			if err != nil || u == nil || len(u) != 0 {
				t.Fatal("empty table")
			}
		} else if err == nil {
			t.Fatal("malformed response accepted")
		}
	}
}
