package panel

import (
	"net/http/httptest"
	"testing"

	"github.com/lenaanderson5566-ops/v2box/conf"
)

func newTestPanel(t *testing.T, server *httptest.Server, config *conf.NodeConfig) (*Client, error) {
	t.Helper()
	client, err := New(config)
	if err == nil {
		client.client.SetTransport(server.Client().Transport)
	}
	return client, err
}
