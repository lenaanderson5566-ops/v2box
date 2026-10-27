package core

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
)

// Snapshot of the panel's node-route-presets.json, checked by the pinned core parser.
func TestPanelRoutingPresets(t *testing.T) {
	raw, err := os.ReadFile("testdata/panel-routing-presets.json")
	if err != nil {
		t.Fatal(err)
	}
	var presets []struct {
		ID     string          `json:"id"`
		Config json.RawMessage `json:"config"`
	}
	if err = json.Unmarshal(raw, &presets); err != nil {
		t.Fatal(err)
	}
	if len(presets) == 0 {
		t.Fatal("panel routing presets must not be empty")
	}
	for _, preset := range presets {
		t.Run(preset.ID, func(t *testing.T) {
			n := testNode(t, "vless")
			n.Common.SingBox = preset.Config
			if _, err := buildOptions(n, nil, conf.New()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeRoutingAndAccounting(t *testing.T) {
	limiter.Init()
	n := testNode(t, "vless")
	n.Common.SingBox = json.RawMessage(`{"dns":{"servers":[{"type":"local","tag":"resolver"}],"final":"resolver"},"route":{"rule_set":[{"type":"inline","tag":"loopback","rules":[{"ip_cidr":["127.0.0.0/8"]}]}],"rules":[{"rule_set":["loopback"],"action":"route","outbound":"exit"}],"final":"direct"},"outbounds":[{"type":"direct","tag":"exit"}]}`)
	v := startTestNode(t, n)
	echo := echoServer(t)
	client := clientBox(t, n, testUUID)
	exchange(t, client, echo.Addr())
	traffic, _ := v.GetUserTrafficSlice(n.Tag, 0)
	if len(traffic) != 1 || traffic[0].UID != 42 || traffic[0].Upload == 0 {
		t.Fatalf("accounting lost: %+v", traffic)
	}
}

func TestNativeConfigurationValidation(t *testing.T) {
	for _, raw := range []string{
		`{"inbounds":[]}`, `{"route":{"rule_set":[{"type":"local","path":"/etc/passwd"}]}}`,
		`{"route":{"rule_set":[{"type":"remote","url":"http://example.org/rules"}]}}`,
		`{"dns":{"servers":[{"type":"fakeip"}]}}`, `{"outbounds":[{"type":"wireguard","tag":"vpn"}]}`,
		`{"outbounds":[{"type":"direct","tag":"direct"}]}`, `{"route":{"rules":[{"auth_user":["x"],"action":"reject"}]}}`,
		`{"outbounds":[{"type":"http","tag":"proxy","tls":{"certificate_path":"/etc/passwd"}}]}`,
	} {
		n := testNode(t, "vless")
		n.Common.SingBox = json.RawMessage(raw)
		if _, err := buildOptions(n, nil, conf.New()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, typ := range []string{"udp", "tcp", "tls", "https"} {
		n := testNode(t, "vless")
		n.Common.SingBox = json.RawMessage(`{"dns":{"servers":[{"type":"` + typ + `","tag":"resolver","server":"1.1.1.1"}],"final":"resolver"}}`)
		if _, err := buildOptions(n, nil, conf.New()); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
}
