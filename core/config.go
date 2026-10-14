package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/internal/landing"

	"github.com/sagernet/sing-box/option"
	sj "github.com/sagernet/sing/common/json"
)

type object = map[string]any

var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func buildOptions(n *panel.NodeInfo, users []panel.UserInfo, c *conf.Conf) (option.Options, error) {
	var options option.Options
	in, err := buildInbound(n, users)
	if err != nil {
		return options, err
	}
	config := object{"log": object{"level": c.LogConfig.Level}, "inbounds": []any{in}, "outbounds": []any{object{"type": "direct", "tag": "direct"}}}
	if c.LogConfig.Level == "warning" {
		config["log"] = object{"level": "warn"}
	}
	if err = buildRoutes(config, n); err != nil {
		return options, err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return options, err
	}
	err = sj.UnmarshalContext(boxContext(context.Background()), raw, &options)
	return options, err
}

func buildInbound(n *panel.NodeInfo, users []panel.UserInfo) (object, error) {
	if n == nil || n.Common == nil {
		return nil, fmt.Errorf("missing node settings")
	}
	c := n.Common
	if c.ServerPort < 1 || c.ServerPort > 65535 {
		return nil, fmt.Errorf("invalid server port %d", c.ServerPort)
	}
	listen := c.ListenIP
	if listen == "" {
		listen = "::"
	}
	if net.ParseIP(listen) == nil {
		return nil, fmt.Errorf("listen_ip must be an IP address")
	}
	if c.Encryption != "" && c.Encryption != "none" {
		return nil, fmt.Errorf("Xray VLESS encryption %q is not supported by sing-box", c.Encryption)
	}
	if len(c.TrustedXForwardedFor) > 0 {
		return nil, fmt.Errorf("trusted_x_forwarded_for requires a separate sing-box-compatible proxy configuration")
	}
	in := object{"type": n.Type, "tag": n.Tag, "listen": listen, "listen_port": c.ServerPort}
	if c.LandingPage.Enabled {
		if n.Type != "naive" && n.Type != "hysteria2" {
			return nil, fmt.Errorf("LandingPage is only supported for naive and hysteria2")
		}
		if n.Type == "hysteria2" && c.Obfs != "" {
			return nil, fmt.Errorf("Hysteria2 LandingPage requires obfs to be disabled for ordinary HTTP3 clients")
		}
		if _, err := landing.New(c.LandingPage.Directory); err != nil {
			return nil, err
		}
		if n.Type == "naive" {
			in["landing_page"] = c.LandingPage
		} else if c.LandingPage.Directory != "" {
			in["masquerade"] = object{"type": "file", "directory": c.LandingPage.Directory}
		} else {
			in["masquerade"] = object{"type": "string", "status_code": 200, "content": landing.DefaultHTML, "headers": object{"Content-Type": "text/html; charset=utf-8", "X-Content-Type-Options": "nosniff"}}
		}
	}
	list := []object{}
	ids := map[int]bool{}
	uuids := map[string]bool{}
	credentials := map[string]bool{}
	for _, u := range sortedUsers(users) {
		if u.Id <= 0 || u.Uuid == "" || ids[u.Id] || uuids[u.Uuid] || u.SpeedLimit < 0 || u.DeviceLimit < 0 {
			return nil, fmt.Errorf("invalid or duplicate user %d", u.Id)
		}
		ids[u.Id] = true
		uuids[u.Uuid] = true
		if (n.Type == "vmess" || n.Type == "vless" || n.Type == "tuic") && !canonicalUUID.MatchString(u.Uuid) {
			return nil, fmt.Errorf("user %d has an invalid UUID", u.Id)
		}
		user := object{"name": u.Uuid}
		switch n.Type {
		case "vmess":
			user["uuid"] = u.Uuid
		case "vless":
			user["uuid"] = u.Uuid
			user["flow"] = c.Flow
		case "naive":
			user["username"] = u.Uuid
			user["password"] = u.Uuid
		case "trojan", "anytls", "hysteria2":
			user["password"] = u.Uuid
		case "tuic":
			user["uuid"] = u.Uuid
			user["password"] = u.Uuid
		case "shadowsocks":
			password := u.Uuid
			if strings.HasPrefix(c.Cipher, "2022-") {
				size := 32
				if c.Cipher == "2022-blake3-aes-128-gcm" {
					size = 16
				}
				if len(password) < size {
					return nil, fmt.Errorf("user %d UUID too short for SS2022 key", u.Id)
				}
				password = base64.StdEncoding.EncodeToString([]byte(password[:size]))
			}
			user["password"] = password
		default:
			return nil, fmt.Errorf("unsupported protocol %q", n.Type)
		}
		list = append(list, user)
		credential := u.Uuid
		if n.Type == "vmess" || n.Type == "vless" || n.Type == "tuic" {
			credential = strings.ToLower(credential)
		}
		if n.Type == "shadowsocks" {
			credential = user["password"].(string)
		}
		if credentials[credential] {
			return nil, fmt.Errorf("duplicate effective credential for user %d", u.Id)
		}
		credentials[credential] = true
	}
	in["users"] = list
	switch n.Type {
	case "vmess", "vless", "trojan":
		transport, err := buildTransport(c)
		if err != nil {
			return nil, err
		}
		if transport != nil {
			in["transport"] = transport
		}
	case "shadowsocks":
		if c.Network != "" && c.Network != "tcp" {
			return nil, fmt.Errorf("Shadowsocks transport %q is unsupported", c.Network)
		}
		if err := plainSettings(c.NetworkSettings); err != nil {
			return nil, err
		}
		switch c.Cipher {
		case "aes-128-gcm", "aes-192-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305":
			if c.ServerKey != "" {
				return nil, fmt.Errorf("server_key is only valid for SS2022")
			}
		case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
			if c.ServerKey == "" {
				return nil, fmt.Errorf("SS2022 requires server_key")
			}
			in["password"] = c.ServerKey
		default:
			return nil, fmt.Errorf("unsupported multi-user Shadowsocks cipher %q", c.Cipher)
		}
		in["method"] = c.Cipher
	case "naive":
		if c.Network != "" && c.Network != "tcp" {
			return nil, fmt.Errorf("Naive requires TCP (HTTPS/HTTP2)")
		}
		if err := plainSettings(c.NetworkSettings); err != nil {
			return nil, err
		}
		in["network"] = "tcp"
	case "hysteria2", "tuic", "anytls":
		if c.Network != "" && c.Network != "tcp" && !(c.Network == "hysteria" && n.Type == "hysteria2") {
			return nil, fmt.Errorf("%s does not support transport %q", n.Type, c.Network)
		}
		if err := plainSettings(c.NetworkSettings); err != nil {
			return nil, err
		}
		if n.Type == "hysteria2" {
			in["up_mbps"] = c.UpMbps
			in["down_mbps"] = c.DownMbps
			in["ignore_client_bandwidth"] = c.Ignore_Client_Bandwidth
			if c.Obfs != "" {
				in["obfs"] = object{"type": c.Obfs, "password": c.ObfsPassword}
			}
		}
		if n.Type == "tuic" {
			in["congestion_control"] = c.CongestionControl
			in["zero_rtt_handshake"] = c.ZeroRTTHandshake
		}
		if n.Type == "anytls" && len(c.PaddingScheme) > 0 {
			in["padding_scheme"] = c.PaddingScheme
		}
	default:
		return nil, fmt.Errorf("unsupported protocol %q", n.Type)
	}
	tls, err := buildTLS(n)
	if err != nil {
		return nil, err
	}
	if tls != nil {
		in["tls"] = tls
	}
	if (n.Type == "naive" || n.Type == "hysteria2" || n.Type == "tuic" || n.Type == "anytls") && tls == nil {
		return nil, fmt.Errorf("%s requires TLS", n.Type)
	}
	return in, nil
}

func plainSettings(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var s object
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	for k, v := range s {
		if k == "acceptProxyProtocol" && v == false {
			continue
		}
		if k == "header" {
			h, ok := v.(map[string]any)
			if ok && len(h) == 1 && h["type"] == "none" {
				continue
			}
		}
		return fmt.Errorf("unsupported TCP/obfuscation setting %q; remove or migrate it explicitly", k)
	}
	return nil
}
func buildTransport(c *panel.CommonNode) (object, error) {
	if c.Network == "" || c.Network == "tcp" {
		return nil, plainSettings(c.NetworkSettings)
	}
	if c.Network != "ws" && c.Network != "grpc" && c.Network != "httpupgrade" {
		return nil, fmt.Errorf("transport %q is not supported (including Xray XHTTP)", c.Network)
	}
	var s object
	if len(c.NetworkSettings) > 0 {
		if err := json.Unmarshal(c.NetworkSettings, &s); err != nil {
			return nil, err
		}
	}
	t := object{"type": c.Network}
	for k, v := range s {
		if k == "acceptProxyProtocol" && v == false {
			continue
		}
		switch k {
		case "path", "host", "headers":
			if c.Network == "grpc" {
				return nil, fmt.Errorf("unsupported grpc field %s", k)
			}
			t[k] = v
		case "serviceName":
			if c.Network != "grpc" {
				return nil, fmt.Errorf("serviceName requires grpc")
			}
			t["service_name"] = v
		case "maxEarlyData":
			if c.Network != "ws" {
				return nil, fmt.Errorf("maxEarlyData requires ws")
			}
			t["max_early_data"] = v
		case "earlyDataHeaderName":
			if c.Network != "ws" {
				return nil, fmt.Errorf("earlyDataHeaderName requires ws")
			}
			t["early_data_header_name"] = v
		default:
			return nil, fmt.Errorf("unsupported %s setting %q", c.Network, k)
		}
	}
	return t, nil
}
func buildTLS(n *panel.NodeInfo) (object, error) {
	c := n.Common
	s := c.TlsSettings
	switch n.Security {
	case panel.None:
		return nil, nil
	case panel.Tls:
		cert := c.CertInfo
		if cert == nil {
			return nil, fmt.Errorf("missing certificate settings")
		}
		if cert.CertMode == "" || cert.CertMode == "none" {
			return nil, nil
		}
		if cert.RejectUnknownSni {
			return nil, fmt.Errorf("reject_unknown_sni cannot be translated to sing-box without changing semantics")
		}
		tls := object{"enabled": true, "server_name": cert.CertDomain}
		if cert.CertMode == "remote" {
			if cert.TlsCert == "" || cert.TlsKey == "" {
				return nil, fmt.Errorf("empty remote certificate")
			}
			tls["certificate"] = []string{cert.TlsCert}
			tls["key"] = []string{cert.TlsKey}
		} else {
			tls["certificate_path"] = cert.CertFile
			tls["key_path"] = cert.KeyFile
		}
		if n.Type == "hysteria2" || n.Type == "tuic" {
			tls["alpn"] = []string{"h3"}
		}
		return tls, nil
	case panel.Reality:
		if n.Type != "vless" {
			return nil, fmt.Errorf("Reality is supported for VLESS nodes only")
		}
		if s.Mldsa65Seed != "" || s.Xver != 0 {
			return nil, fmt.Errorf("Xray Reality mldsa65Seed/xver are unsupported")
		}
		if len(s.EffectiveServerNames()) > 1 {
			return nil, fmt.Errorf("multiple Reality server_names require explicit migration to one handshake server")
		}
		host := s.Dest
		if host == "" {
			host = s.PrimaryServerName()
		}
		port := 443
		if s.ServerPort != "" {
			p, err := strconv.Atoi(s.ServerPort)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("invalid Reality handshake port")
			}
			port = p
		}
		if host == "" || s.PrivateKey == "" {
			return nil, fmt.Errorf("Reality requires handshake server and private key")
		}
		return object{"enabled": true, "server_name": s.PrimaryServerName(), "reality": object{"enabled": true, "handshake": object{"server": host, "server_port": port}, "private_key": s.PrivateKey, "short_id": s.EffectiveShortIds()}}, nil
	default:
		return nil, fmt.Errorf("unknown security mode %d", n.Security)
	}
}
