package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
)

var v10Fields = map[string]string{
	"protocol":              "protocol",
	"listenAddress":         "listen_ip",
	"listenPort":            "server_port",
	"network":               "network",
	"networkSettings":       "network_settings",
	"trustedXForwardedFor":  "trusted_x_forwarded_for",
	"tls":                   "tls",
	"tlsSettings":           "tls_settings",
	"encryption":            "encryption",
	"encryptionSettings":    "encryption_settings",
	"flow":                  "flow",
	"cipher":                "cipher",
	"congestionControl":     "congestion_control",
	"zeroRttHandshake":      "zero_rtt_handshake",
	"upMbps":                "up_mbps",
	"downMbps":              "down_mbps",
	"obfs":                  "obfs",
	"obfsPassword":          "obfs_password",
	"paddingScheme":         "padding_scheme",
	"serverKey":             "server_key",
	"ignoreClientBandwidth": "ignore_client_bandwidth",
	"intervals":             "base_config",
	"routes":                "routes",
	"singBox":               "sing_box",
}
var v10Nested = map[string]string{
	"serverName":             "server_name",
	"serverNames":            "server_names",
	"serverPort":             "server_port",
	"shortId":                "short_id",
	"shortIds":               "short_ids",
	"privateKey":             "private_key",
	"certMode":               "cert_mode",
	"certFile":               "cert_file",
	"keyFile":                "key_file",
	"tlsCert":                "tls_cert",
	"tlsKey":                 "tls_key",
	"provider":               "provider",
	"dnsEnv":                 "dns_env",
	"rejectUnknownSni":       "reject_unknown_sni",
	"dest":                   "dest",
	"xver":                   "xver",
	"mldsa65Seed":            "mldsa65Seed",
	"pushInterval":           "push_interval",
	"pullInterval":           "pull_interval",
	"nodeReportMinTraffic":   "node_report_min_traffic",
	"deviceOnlineMinTraffic": "device_online_min_traffic",
	"id":                     "id",
	"match":                  "match",
	"action":                 "action",
	"actionValue":            "action_value",
}

func remapV10(source map[string]json.RawMessage, fields map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	for key, name := range fields {
		if value, ok := source[key]; ok {
			out[name] = value
		}
	}
	return out
}
func decodeV10Config(body []byte, out *CommonNode) error {
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope.Data["protocol"] == nil || envelope.Data["listenPort"] == nil {
		return fmt.Errorf("missing V10 node configuration")
	}
	data := remapV10(envelope.Data, v10Fields)
	for _, key := range []string{"tls_settings", "base_config"} {
		var nested map[string]json.RawMessage
		if len(data[key]) > 0 {
			if err := json.Unmarshal(data[key], &nested); err != nil {
				return err
			}
			data[key], _ = json.Marshal(remapV10(nested, v10Nested))
		}
	}
	if len(data["routes"]) > 0 {
		var routes []map[string]json.RawMessage
		if err := json.Unmarshal(data["routes"], &routes); err != nil {
			return err
		}
		for i := range routes {
			routes[i] = remapV10(routes[i], v10Nested)
		}
		data["routes"], _ = json.Marshal(routes)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (c *Client) nodePath(resource string) string {
	return "/api/v10/server/" + resource
}
func (c *Client) announceCapabilities(ctx context.Context) error {
	if !c.v10 {
		return nil
	}
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/sagernet/sing-box" {
				version = dep.Version
			}
		}
	}
	r, err := c.client.R().SetContext(ctx).SetBody(map[string]any{"coreVersion": version, "features": []string{"native-routing-v1", "dns-v1", "outbounds-v1", "server-traffic-budget-v1", "target-statistics-v1"}}).Post(c.nodePath("capabilities"))
	if err != nil {
		return err
	}
	if r.StatusCode() != 200 {
		return fmt.Errorf("V10 capability registration returned HTTP %d", r.StatusCode())
	}
	return nil
}
