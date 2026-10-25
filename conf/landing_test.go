package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLandingPagePerNodeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"Nodes":[{"ApiHost":"https://panel.example.test","ApiKey":"test","NodeID":1,"LandingPage":{"Enabled":true,"Directory":"/site"}},{"ApiHost":"https://panel.example.test","ApiKey":"test","NodeID":2}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if !c.NodeConfigs[0].LandingPage.Enabled || c.NodeConfigs[0].LandingPage.Directory != "/site" || c.NodeConfigs[1].LandingPage.Enabled {
		t.Fatal("node-local landing configuration lost or enabled globally")
	}
}
