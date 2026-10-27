package core

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/miekg/dns"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
)

func nativeExchange(t *testing.T, client *box.Box, target, payload string, allowed bool) {
	t.Helper()
	out, _ := client.Outbound().Outbound("proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := out.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
	if err == nil {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = conn.Write([]byte(payload))
		if err == nil {
			data := make([]byte, len(payload))
			_, err = io.ReadFull(conn, data)
			if err == nil && !bytes.Equal(data, []byte(payload)) {
				t.Fatal("corrupt routed payload")
			}
		}
	}
	if allowed && err != nil {
		t.Fatalf("allowed route failed: %v", err)
	}
	if !allowed && err == nil {
		t.Fatal("rejected route reached destination")
	}
}

func nativeTestDNS(t *testing.T, hits *atomic.Int32) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		hits.Add(1)
		response := new(dns.Msg)
		response.SetReply(request)
		for _, question := range request.Question {
			if question.Qtype == dns.TypeA {
				response.Answer = append(response.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("127.0.0.1")})
			}
		}
		_ = w.WriteMsg(response)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown(); _ = conn.Close() })
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func TestNativeRuleEffects(t *testing.T) {
	limiter.Init()
	for _, match := range []string{"domain_suffix", "domain", "domain_keyword", "domain_regex", "ip_cidr", "port", "port_range", "protocol"} {
		t.Run(match, func(t *testing.T) {
			echo := echoServer(t)
			other := echoServer(t)
			port := echo.Addr().(*net.TCPAddr).Port
			value := any([]string{"blocked.example"})
			if match == "domain_keyword" {
				value = []string{"blocked"}
			}
			if match == "domain_regex" {
				value = []string{`^b[a-z]{1,7}\.example$`}
			}
			if match == "ip_cidr" {
				value = []string{"127.0.0.1/32"}
			}
			if match == "port" {
				value = []int{port}
			}
			if match == "port_range" {
				value = []string{strconv.Itoa(port) + ":" + strconv.Itoa(port)}
			}
			if match == "protocol" {
				value = []string{"http"}
			}
			rules := []any{}
			if match == "protocol" || len(match) >= 6 && match[:6] == "domain" {
				rules = append(rules, object{"action": "sniff"})
			}
			if match == "ip_cidr" {
				rules = append(rules, object{"action": "resolve"})
			}
			rules = append(rules, object{match: value, "action": "reject"})
			n := testNode(t, "vless")
			n.Common.SingBox, _ = json.Marshal(object{"route": object{"rules": rules}})
			startTestNode(t, n)
			client := clientBox(t, n, testUUID)
			allowed := "GET / HTTP/1.1\r\nHost: allowed.example\r\n\r\n"
			if match == "protocol" {
				allowed = "plain non-http echo"
			}
			if match != "ip_cidr" {
				nativeExchange(t, client, other.Addr().String(), allowed, true)
			}
			nativeExchange(t, client, echo.Addr().String(), "GET / HTTP/1.1\r\nHost: blocked.example\r\n\r\n", false)
		})
	}
}

func TestNativeProxyAndDNSRouting(t *testing.T) {
	limiter.Init()
	t.Run("HTTP proxy and default outbound", func(t *testing.T) {
		var hits atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				http.Error(w, "CONNECT required", 400)
				return
			}
			target, err := net.DialTimeout("tcp", r.Host, time.Second)
			if err != nil {
				http.Error(w, "target unavailable", 502)
				return
			}
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				target.Close()
				return
			}
			defer conn.Close()
			defer target.Close()
			hits.Add(1)
			_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffer.Flush()
			go func() { _, _ = io.Copy(target, buffer) }()
			_, _ = io.Copy(conn, target)
		}))
		defer proxy.Close()
		for _, final := range []bool{false, true} {
			t.Run(strconv.FormatBool(final), func(t *testing.T) {
				echo := echoServer(t)
				route := object{"rules": []any{object{"ip_cidr": []string{"127.0.0.1/32"}, "action": "route", "outbound": "exit"}}}
				if final {
					route = object{"rules": []any{}, "final": "exit"}
				}
				n := testNode(t, "vless")
				n.Common.SingBox, _ = json.Marshal(object{"route": route, "outbounds": []any{object{"type": "http", "tag": "exit", "server": "127.0.0.1", "server_port": proxy.Listener.Addr().(*net.TCPAddr).Port}}})
				startTestNode(t, n)
				before := hits.Load()
				nativeExchange(t, clientBox(t, n, testUUID), echo.Addr().String(), "routed through CONNECT", true)
				if hits.Load() != before+1 {
					t.Fatal("proxy outbound was bypassed")
				}
			})
		}
	})
	t.Run("DNS split with fallback", func(t *testing.T) {
		var selected, fallback atomic.Int32
		a, b := nativeTestDNS(t, &selected), nativeTestDNS(t, &fallback)
		echo := echoServer(t)
		port := strconv.Itoa(echo.Addr().(*net.TCPAddr).Port)
		n := testNode(t, "vless")
		n.Common.SingBox, _ = json.Marshal(object{"dns": object{"servers": []any{object{"type": "udp", "tag": "local", "server": "127.0.0.1", "server_port": b}, object{"type": "udp", "tag": "split", "server": "127.0.0.1", "server_port": a}}, "rules": []any{object{"domain_suffix": []string{"split.routing.test"}, "action": "route", "server": "split"}}, "final": "local"}, "route": object{"rules": []any{object{"action": "sniff"}, object{"action": "resolve"}}}})
		startTestNode(t, n)
		client := clientBox(t, n, testUUID)
		nativeExchange(t, client, "split.routing.test:"+port, "split DNS echo", true)
		if selected.Load() == 0 || fallback.Load() != 0 {
			t.Fatal("DNS matching server was not selected")
		}
		nativeExchange(t, client, "fallback.routing.test:"+port, "fallback DNS echo", true)
		if fallback.Load() == 0 {
			t.Fatal("DNS fallback was not used")
		}
	})
}

func TestNativeHTTPSRuleSetEffect(t *testing.T) {
	limiter.Init()
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":3,"rules":[{"ip_cidr":["127.0.0.1/32"]}]}`)
	}))
	defer server.Close()
	echo := echoServer(t)
	n := testNode(t, "vless")
	n.Common.SingBox, _ = json.Marshal(object{"route": object{"rule_set": []any{object{"type": "remote", "tag": "remote", "format": "source", "url": server.URL, "download_detour": "direct", "update_interval": "1d"}}, "rules": []any{object{"rule_set": []string{"remote"}, "action": "reject"}}}})
	users := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
	limiter.AddLimiter(n.Type, n.Tag, users, nil)
	t.Cleanup(func() { limiter.DeleteLimiter(n.Tag) })
	cfg := testConf()
	options, err := buildOptions(n, users, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Trust only the test CA for this Box; production TLS verification stays enabled.
	options.Certificate = &option.CertificateOptions{Store: "none", Certificate: []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}}
	v := New(cfg)
	b, cancel, err := v.newBox(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = b.Close(); _ = v.Close() })
	nativeExchange(t, clientBox(t, n, testUUID), echo.Addr().String(), "remote set must reject", false)
	if hits.Load() == 0 {
		t.Fatal("remote HTTPS rule set was not downloaded")
	}
}

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
