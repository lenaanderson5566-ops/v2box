package cmd

import (
	"encoding/json"
	"errors"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/node"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReloadRejectsBeforeClosingCore(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"protocol":"vless","tls":0,"server_port":443,"base_config":{"push_interval":60,"pull_interval":60},"sing_box":{"route":{"final":"missing"}}}`))
	}))
	defer s.Close()
	name := filepath.Join(t.TempDir(), "config.json")
	body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test", "ApiVersion": "legacy"}}})
	if err := os.WriteFile(name, body, 0600); err != nil {
		t.Fatal(err)
	}
	nodes := &node.Node{}
	v := core.New(conf.New())
	defer v.Close()
	old := v
	err := reload(name, &nodes, &v)
	var rejected *reloadRejected
	if !errors.As(err, &rejected) || v != old {
		t.Fatalf("invalid config not rejected before shutdown: %v", err)
	}
	if err := v.AddNode("still-open", &panel.NodeInfo{Tag: "still-open", Type: "vless", Common: &panel.CommonNode{ServerPort: 12345, ListenIP: "127.0.0.1", Network: "tcp"}}); err != nil {
		t.Fatalf("old core closed: %v", err)
	}
}
