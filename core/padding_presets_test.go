package core

import (
	"encoding/json"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"os"
	"testing"
)

// Opt-in integration against the exact presets shipped by the panel.
func TestPanelAnyTLSPresets(t *testing.T) {
	path := os.Getenv("V2BOX_PADDING_PRESETS")
	if path == "" {
		t.Skip("set V2BOX_PADDING_PRESETS to panel preset JSON")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var presets []struct {
		ID     string   `json:"id"`
		Scheme []string `json:"scheme"`
	}
	if err := json.Unmarshal(raw, &presets); err != nil {
		t.Fatal(err)
	}
	if len(presets) == 0 {
		t.Fatal("empty presets")
	}
	limiter.Init()
	echo := echoServer(t)
	cert := testCertificate(t)
	for _, preset := range presets {
		t.Run(preset.ID, func(t *testing.T) {
			n := testNode(t, "anytls")
			n.Security = panel.Tls
			n.Common.CertInfo = cert
			n.Common.PaddingScheme = preset.Scheme
			users := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
			limiter.AddLimiter("anytls", n.Tag, users, nil)
			t.Cleanup(func() { limiter.DeleteLimiter(n.Tag) })
			cfg := conf.New()
			cfg.LogConfig.Level = "error"
			v := New(cfg)
			t.Cleanup(func() { v.Close() })
			if err := v.Start([]*panel.NodeInfo{n}); err != nil {
				t.Fatal(err)
			}
			if err := v.AddNode(n.Tag, n); err != nil {
				t.Fatal(err)
			}
			if err := v.SetUsers(n.Tag, users); err != nil {
				t.Fatal(err)
			}
			b := clientBox(t, n, testUUID)
			exchange(t, b, echo.Addr())
			expected := int64(len("sing-box-backend-echo") * 256)
			traffic := awaitTraffic(t, v, n.Tag, expected)
			if len(traffic) != 1 || traffic[0].Upload != expected || traffic[0].Download != expected {
				t.Fatalf("padding affected payload accounting: %+v", traffic)
			}
		})
	}
}
