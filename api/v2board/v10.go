package panel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/lenaanderson5566-ops/v2box/internal/buildinfo"
	"runtime/debug"
	"strings"
	"time"
)

var v10Fields = map[string]string{
	"domainTransition":      "domain_transition",
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
	body := map[string]any{"agentVersion": buildinfo.Version, "coreVersion": version, "features": []string{"native-routing-v1", "dns-v1", "outbounds-v1", "server-traffic-budget-v1", "target-statistics-v1", "target-statistics-policy-v2", "target-statistics-interval-v1", "configuration-receipt-v1", "certificate-transition-v1"}}
	report := c.configurationReport.Load()
	if report != nil {
		body["configuration"] = report
	}
	r, err := c.client.R().SetContext(ctx).SetBody(body).Post(c.nodePath("capabilities"))
	if err != nil {
		return err
	}
	if r.StatusCode() != 200 {
		return fmt.Errorf("V10 capability registration returned HTTP %d", r.StatusCode())
	}
	if report != nil && report.TransitionID != "" && report.State == "applied" {
		var result struct {
			Data struct {
				Accepted bool `json:"domainTransitionAccepted"`
			} `json:"data"`
		}
		if json.Unmarshal(r.Body(), &result) != nil || !result.Data.Accepted {
			return fmt.Errorf("panel did not confirm domain publication")
		}
		confirmed := *report
		confirmed.TransitionID = ""
		c.configurationReport.CompareAndSwap(report, &confirmed)
	}
	return nil
}

// The application version belongs to the response body. Transport ETags may be
// weakened or removed by compression proxies; retain them only for HTTP caching.
// Older panels supply the same SHA-256 in a strong or weak ETag.
func configurationReceiptHash(body []byte, etag string) (string, error) {
	var envelope struct {
		Meta struct {
			ConfigurationHash json.RawMessage `json:"configurationHash"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("invalid configuration response")
	}
	var hash string
	if len(envelope.Meta.ConfigurationHash) > 0 {
		if err := json.Unmarshal(envelope.Meta.ConfigurationHash, &hash); err != nil {
			return "", fmt.Errorf("invalid configuration hash metadata")
		}
	} else {
		tag := strings.TrimSpace(etag)
		tag = strings.TrimPrefix(tag, "W/")
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
			hash = tag[1 : len(tag)-1]
		}
	}
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", fmt.Errorf("missing or invalid configuration hash; update panel and check proxy response handling")
	}
	return hash, nil
}

// ConfigurationReport contains only a panel-issued version and a bounded error code.
type ConfigurationReport struct {
	TransitionID           string `json:"transitionId,omitempty"`
	CertificateFingerprint string `json:"certificateFingerprint,omitempty"`
	Hash                   string `json:"hash"`
	State                  string `json:"state"`
	ErrorCode              string `json:"errorCode,omitempty"`
}

// Ordinary receipts are best-effort. Domain transitions require a publication
// acknowledgement; bounded retries recover a lost response using the same ID.
func (c *Client) ReportConfiguration(ctx context.Context, hash, code string, fingerprint ...string) error {
	if !c.v10 {
		return nil
	}
	if len(hash) != 64 {
		return fmt.Errorf("invalid configuration hash")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return err
	}
	switch code {
	case "", "decode", "core", "users", "certificate", "start":
	default:
		return nil
	}
	state := "applied"
	if code != "" {
		state = "failed"
	}
	report := &ConfigurationReport{Hash: hash, State: state, ErrorCode: code}
	if code == "" && len(fingerprint) > 0 && len(fingerprint[0]) == 64 {
		if _, err := hex.DecodeString(fingerprint[0]); err == nil {
			report.CertificateFingerprint = fingerprint[0]
		}
	}
	if len(fingerprint) > 1 {
		report.TransitionID = fingerprint[1]
	}
	c.configurationReport.Store(report)
	attempts := 1
	if report.TransitionID != "" && report.State == "applied" {
		if len(report.TransitionID) != 32 || report.CertificateFingerprint == "" {
			return fmt.Errorf("incomplete transition receipt")
		}
		attempts = 3
	}
	var err error
	for i := 0; i < attempts; i++ {
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = c.announceCapabilities(bounded)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}
