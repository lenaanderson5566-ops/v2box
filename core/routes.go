package core

import (
	"encoding/json"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"net/netip"
	"strconv"
	"strings"
)

func domainMatch(matches []string) (object, error) {
	result := object{}
	for _, m := range matches {
		field, value := "domain_keyword", m
		for prefix, key := range map[string]string{"full:": "domain", "domain:": "domain_suffix", "regexp:": "domain_regex", "keyword:": "domain_keyword"} {
			if strings.HasPrefix(m, prefix) {
				field, value = key, strings.TrimPrefix(m, prefix)
				break
			}
		}
		if strings.HasPrefix(m, "geosite:") || strings.HasPrefix(m, "ext:") {
			return nil, fmt.Errorf("legacy domain database %q must be migrated to sing-box rule sets", m)
		}
		if value == "" {
			return nil, fmt.Errorf("empty domain match")
		}
		values, _ := result[field].([]string)
		result[field] = append(values, value)
	}
	return result, nil
}
func buildRoutes(config object, n *panel.NodeInfo) error {
	if len(n.Common.SingBox) > 0 && string(n.Common.SingBox) != "null" {
		return buildNativeRoutes(config, n.Common.SingBox)
	}
	rules := []object{{"action": "sniff"}}
	outbounds := config["outbounds"].([]any)
	known := map[string]string{"direct": ""}
	dnsServers := []object{{"type": "local", "tag": "local"}}
	dnsRules := []object{}
	for _, r := range n.Common.Routes {
		var match object
		var err error
		switch r.Action {
		case "block", "route", "dns":
			match, err = domainMatch(r.Match)
		case "block_ip", "route_ip":
			cidrs := []string{}
			for _, v := range r.Match {
				if ip, e := netip.ParseAddr(v); e == nil {
					bits := 128
					if ip.Is4() {
						bits = 32
					}
					v = netip.PrefixFrom(ip, bits).String()
				}
				if _, e := netip.ParsePrefix(v); e != nil {
					return fmt.Errorf("route %d: invalid CIDR %q (legacy GeoIP is unsupported)", r.Id, v)
				}
				cidrs = append(cidrs, v)
			}
			match = object{"ip_cidr": cidrs}
		case "block_port":
			ports := []int{}
			ranges := []string{}
			for _, v := range r.Match {
				v = strings.ReplaceAll(v, "-", ":")
				if strings.Contains(v, ":") {
					parts := strings.Split(v, ":")
					if len(parts) != 2 {
						return fmt.Errorf("invalid port range %q", v)
					}
					a, e1 := strconv.Atoi(parts[0])
					b, e2 := strconv.Atoi(parts[1])
					if e1 != nil || e2 != nil || a < 1 || b > 65535 || a > b {
						return fmt.Errorf("invalid port range %q", v)
					}
					ranges = append(ranges, v)
				} else {
					p, e := strconv.Atoi(v)
					if e != nil || p < 1 || p > 65535 {
						return fmt.Errorf("invalid port %q", v)
					}
					ports = append(ports, p)
				}
			}
			match = object{}
			if len(ports) > 0 {
				match["port"] = ports
			}
			if len(ranges) > 0 {
				match["port_range"] = ranges
			}
		case "protocol":
			match = object{"protocol": r.Match}
		case "default_out":
			match = object{}
		default:
			return fmt.Errorf("unknown route action %q", r.Action)
		}
		if err != nil {
			return fmt.Errorf("route %d: %w", r.Id, err)
		}
		if r.Action != "default_out" && r.Action != "dns" && len(r.Match) == 0 {
			return fmt.Errorf("route %d has empty match", r.Id)
		}
		switch r.Action {
		case "dns":
			if r.ActionValue == nil {
				return fmt.Errorf("DNS route requires action_value")
			}
			addr := *r.ActionValue
			if _, err := netip.ParseAddr(addr); err != nil {
				return fmt.Errorf("DNS route %d: use a plain DNS server IP", r.Id)
			}
			tag := fmt.Sprintf("dns-%d-%d", r.Id, len(dnsServers))
			dnsServers = append(dnsServers, object{"type": "udp", "tag": tag, "server": addr})
			match["action"] = "route"
			match["server"] = tag
			dnsRules = append(dnsRules, match)
		case "route", "route_ip", "default_out":
			if r.ActionValue == nil {
				return fmt.Errorf("route requires sing-box outbound JSON")
			}
			var out object
			if err := json.Unmarshal([]byte(*r.ActionValue), &out); err != nil {
				return err
			}
			typ, _ := out["type"].(string)
			tag, _ := out["tag"].(string)
			if typ == "" || tag == "" {
				return fmt.Errorf("route %d: action_value must be sing-box outbound JSON with type and tag; Xray outbound JSON is unsupported", r.Id)
			}
			if previous, ok := known[tag]; ok {
				if previous != *r.ActionValue {
					return fmt.Errorf("conflicting outbound tag %q", tag)
				}
			} else {
				known[tag] = *r.ActionValue
				outbounds = append(outbounds, out)
			}
			match["action"] = "route"
			match["outbound"] = tag
			rules = append(rules, match)
		default:
			match["action"] = "reject"
			rules = append(rules, match)
		}
	}
	if len(dnsRules) > 0 {
		rules = append([]object{{"action": "sniff"}, {"action": "resolve"}}, rules[1:]...)
	}
	config["dns"] = object{"servers": dnsServers, "rules": dnsRules, "final": "local"}
	config["route"] = object{"rules": rules, "final": "direct", "default_domain_resolver": "local"}
	config["outbounds"] = outbounds
	return nil
}
