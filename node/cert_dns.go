package node

import (
	"context"
	"encoding/json"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// Only fixed HTTPS resolvers are contacted. Both address families are checked:
// an unexpected AAAA record can break clients even when IPv4 is correct.
func verifyTransitionDNS(t *panel.DomainTransition) error {
	if len(t.Addresses) == 0 || len(t.Addresses) > 2 {
		return fmt.Errorf("missing transition addresses")
	}
	for family, address := range t.Addresses {
		ip := net.ParseIP(address)
		if ip == nil || (family != "ipv4" && family != "ipv6") || (family == "ipv4") != (ip.To4() != nil) {
			return fmt.Errorf("invalid transition address")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		attempt, done := context.WithTimeout(ctx, 20*time.Second)
		err := lookupTransitionDNS(attempt, t)
		done()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("DNS propagation timed out: %w", err)
		case <-timer.C:
		}
	}
}
func lookupTransitionDNS(ctx context.Context, t *panel.DomainTransition) error {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, resolver := range []string{"https://cloudflare-dns.com/dns-query", "https://dns.google/resolve"} {
		for _, family := range []string{"ipv4", "ipv6"} {
			kind := "A"
			code := 1
			if family == "ipv6" {
				kind = "AAAA"
				code = 28
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolver+"?name="+url.QueryEscape(t.Domain)+"&type="+kind, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", "application/dns-json")
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("public DNS verification unavailable: %w", err)
			}
			var answer struct {
				Status int
				Answer []struct {
					Type int
					Data string
				}
			}
			err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&answer)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || (answer.Status != 0 && answer.Status != 3) {
				return fmt.Errorf("public DNS verification failed")
			}
			var got []string
			for _, a := range answer.Answer {
				if a.Type == code {
					ip := net.ParseIP(a.Data)
					if ip == nil {
						return fmt.Errorf("invalid DNS address")
					}
					got = append(got, ip.String())
				}
			}
			if err := checkDNSAddresses(got, t.Addresses[family]); err != nil {
				return err
			}
		}
	}
	return nil
}
func checkDNSAddresses(got []string, expected string) error {
	sort.Strings(got)
	if expected == "" && len(got) == 0 {
		return nil
	}
	ip := net.ParseIP(expected)
	if ip == nil || len(got) == 0 {
		return fmt.Errorf("DNS propagation is incomplete")
	}
	for _, v := range got {
		if v != ip.String() {
			return fmt.Errorf("DNS points to an unexpected address")
		}
	}
	return nil
}
