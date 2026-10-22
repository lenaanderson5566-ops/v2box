package core

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// Only routing sections are configurable. Inbounds, authentication, accounting,
// endpoints, services and local certificate paths remain owned by V2Box.
func buildNativeRoutes(config object, raw json.RawMessage) error {
	var native object
	if err := json.Unmarshal(raw, &native); err != nil {
		return err
	}
	if native == nil {
		return fmt.Errorf("singBox must be an object")
	}
	for k := range native {
		if k != "dns" && k != "route" && k != "outbounds" {
			return fmt.Errorf("unsupported singBox section %s", k)
		}
	}
	if err := validateNativeValues(native); err != nil {
		return err
	}
	dns := object{"servers": []any{object{"type": "local", "tag": "local"}}, "final": "local"}
	if value, ok := native["dns"]; ok {
		var valid bool
		dns, valid = value.(map[string]any)
		if !valid {
			return fmt.Errorf("dns must be an object")
		}
		if err := allowedKeys(dns, "servers", "rules", "final", "strategy", "disable_cache", "disable_expire", "independent_cache", "cache_capacity", "client_subnet"); err != nil {
			return err
		}
		servers, ok := dns["servers"].([]any)
		if !ok || len(servers) == 0 {
			return fmt.Errorf("dns.servers must be a nonempty array")
		}
		for _, v := range servers {
			s, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid DNS server")
			}
			switch s["type"] {
			case "local", "udp", "tcp", "tls", "https":
			default:
				return fmt.Errorf("unsupported DNS type %v", s["type"])
			}
		}
	}
	route := object{"rules": []any{}, "final": "direct"}
	if value, ok := native["route"]; ok {
		var valid bool
		route, valid = value.(map[string]any)
		if !valid {
			return fmt.Errorf("route must be an object")
		}
	}
	if err := allowedKeys(route, "rules", "rule_set", "final", "default_domain_resolver"); err != nil {
		return err
	}
	if _, ok := route["default_domain_resolver"]; !ok {
		resolver, _ := dns["final"].(string)
		if resolver == "" {
			servers, _ := dns["servers"].([]any)
			if len(servers) > 0 {
				s, _ := servers[0].(map[string]any)
				resolver, _ = s["tag"].(string)
			}
		}
		if resolver == "" {
			return fmt.Errorf("set DNS final or route.default_domain_resolver")
		}
		route["default_domain_resolver"] = resolver
	}
	if _, ok := route["final"]; !ok {
		route["final"] = "direct"
	}
	if rs, ok := route["rule_set"]; ok {
		sets, ok := rs.([]any)
		if !ok {
			return fmt.Errorf("rule_set must be an array")
		}
		for _, value := range sets {
			s, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid rule set")
			}
			switch s["type"] {
			case "inline":
			case "remote":
				address, _ := s["url"].(string)
				u, err := url.Parse(address)
				if err != nil || u.Scheme != "https" || u.Host == "" {
					return fmt.Errorf("remote rule set requires HTTPS URL")
				}
			default:
				return fmt.Errorf("only inline and HTTPS remote rule sets are supported")
			}
		}
	}
	outbounds := []any{object{"type": "direct", "tag": "direct"}}
	seen := map[string]bool{"direct": true}
	if v, ok := native["outbounds"]; ok {
		outs, ok := v.([]any)
		if !ok {
			return fmt.Errorf("outbounds must be an array")
		}
		for _, value := range outs {
			o, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid outbound")
			}
			tag, _ := o["tag"].(string)
			if tag == "" || seen[tag] {
				return fmt.Errorf("missing or duplicate/reserved outbound tag %q", tag)
			}
			seen[tag] = true
			switch o["type"] {
			case "direct", "socks", "http", "vmess", "vless", "trojan", "shadowsocks", "hysteria2", "tuic", "anytls":
			default:
				return fmt.Errorf("unsupported outbound %v", o["type"])
			}
			outbounds = append(outbounds, o)
		}
	}
	if err := validateNativeReferences(dns, route, outbounds); err != nil {
		return err
	}
	config["dns"], config["route"], config["outbounds"] = dns, route, outbounds
	return nil
}

func validateNativeReferences(dns, route object, outbounds []any) error {
	outs, servers, sets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range outbounds {
		o := v.(map[string]any)
		outs[o["tag"].(string)] = true
	}
	for _, v := range dns["servers"].([]any) {
		o := v.(map[string]any)
		tag, _ := o["tag"].(string)
		if tag == "" || servers[tag] {
			return fmt.Errorf("DNS tags must be nonempty and unique")
		}
		servers[tag] = true
	}
	if rs, ok := route["rule_set"].([]any); ok {
		for _, v := range rs {
			o := v.(map[string]any)
			tag, _ := o["tag"].(string)
			if tag == "" || sets[tag] {
				return fmt.Errorf("rule-set tags must be nonempty and unique")
			}
			sets[tag] = true
		}
	}
	check := func(v any, known map[string]bool) error {
		name, ok := v.(string)
		if !ok || !known[name] {
			return fmt.Errorf("unknown configuration reference %v", v)
		}
		return nil
	}
	if err := check(route["final"], outs); err != nil {
		return err
	}
	if v, ok := dns["final"]; ok {
		if err := check(v, servers); err != nil {
			return err
		}
	}
	var walk func(any, string) error
	walk = func(v any, scope string) error {
		switch v := v.(type) {
		case []any:
			for _, v := range v {
				if err := walk(v, scope); err != nil {
					return err
				}
			}
		case map[string]any:
			for k, value := range v {
				var known map[string]bool
				switch k {
				case "outbound", "detour", "download_detour":
					known = outs
				case "server":
					if scope == "rules" || scope == "resolver" {
						known = servers
					}
				case "rule_set":
					if scope == "rules" {
						if list, ok := value.([]any); ok {
							for _, tag := range list {
								if err := check(tag, sets); err != nil {
									return err
								}
							}
						} else if err := check(value, sets); err != nil {
							return err
						}
						continue
					}
				case "default_domain_resolver", "domain_resolver":
					if _, ok := value.(string); ok {
						if err := check(value, servers); err != nil {
							return err
						}
					} else if err := walk(value, "resolver"); err != nil {
						return err
					}
					continue
				}
				if known != nil {
					if err := check(value, known); err != nil {
						return err
					}
				}
				next := scope
				if k == "rules" {
					next = "rules"
				}
				if err := walk(value, next); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, v := range []any{dns, route, outbounds} {
		if err := walk(v, ""); err != nil {
			return err
		}
	}
	return nil
}

func allowedKeys(obj object, keys ...string) error {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("unsupported native configuration field %s", k)
		}
	}
	return nil
}

func validateNativeValues(v any) error {
	switch v := v.(type) {
	case map[string]any:
		for k, value := range v {
			switch k {
			case "certificate_path", "client_certificate_path", "client_key_path", "key_path", "auth_user":
				return fmt.Errorf("native field %s is managed locally or unsupported", k)
			}
			if err := validateNativeValues(value); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range v {
			if err := validateNativeValues(value); err != nil {
				return err
			}
		}
	}
	return nil
}
