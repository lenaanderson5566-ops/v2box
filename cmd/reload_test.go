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

func TestReloadPanelAuthenticationFailurePreservesCore(t *testing.T) {
	for _, failure := range []string{"users-unavailable", "invalid-users", "alive-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/server/config" {
					w.Write([]byte(`{"protocol":"vless","tls":0,"server_port":12345,"base_config":{"push_interval":60,"pull_interval":60}}`))
				} else if r.URL.Path == "/api/v1/server/UniProxy/user" {
					if failure == "users-unavailable" {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if failure == "invalid-users" {
						w.Write([]byte(`{"users":[{"id":1,"uuid":"invalid"}]}`))
					} else {
						w.Write([]byte(`{"users":[{"id":1,"uuid":"b831381d-6324-4d53-ad4f-8cda48b30811"}]}`))
					}
				} else {
					if failure == "alive-unavailable" {
						w.WriteHeader(503)
						return
					}
					w.Write([]byte(`{"alive":{}}`))
				}
			}))
			defer s.Close()
			name := filepath.Join(t.TempDir(), "config.json")
			body, _ := json.Marshal(map[string]any{"Nodes": []any{map[string]any{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test", "ApiVersion": "legacy", "RetryCount": 0}}})
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
				t.Fatalf("replacement was not rejected safely: %v", err)
			}
			if err := v.AddNode("still-open", &panel.NodeInfo{Tag: "still-open", Type: "vless", Common: &panel.CommonNode{ServerPort: 12345, ListenIP: "127.0.0.1", Network: "tcp"}}); err != nil {
				t.Fatalf("old core closed: %v", err)
			}
		})
	}
}
