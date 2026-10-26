package node

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/certificate"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
)

func TestACMEGuardPersistsStop(t *testing.T) {
	c := &panel.CertInfo{CertDomain: "test.example", CertFile: filepath.Join(t.TempDir(), "cert.pem")}
	calls := 0
	operation := func() error { calls++; return errors.New("invalid DNS credentials") }
	if guardedACME(c, operation) == nil {
		t.Fatal("failure accepted")
	}
	// New controller/config object, same on-disk identity (simulated restart).
	copy := *c
	copy.Provider = "changed"
	if guardedACME(&copy, operation) == nil || calls != 1 {
		t.Fatal("restart bypassed protection")
	}
	if err := os.Remove(acmeStatePath(c)); err != nil {
		t.Fatal(err)
	}
	if err := guardedACME(c, func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if _, err := os.Stat(acmeStatePath(c)); !os.IsNotExist(err) {
		t.Fatal("successful operation did not clear state")
	}
}

func TestACMECertificateWriteRollback(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := generateSelfSslCertificate("localhost", certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	oldKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	newCertPath, newKeyPath := filepath.Join(dir, "new.pem"), filepath.Join(dir, "new.key")
	if err := generateSelfSslCertificate("localhost", newCertPath, newKeyPath); err != nil {
		t.Fatal(err)
	}
	newCert, err := os.ReadFile(newCertPath)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := os.ReadFile(newKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	l := &Lego{config: &panel.CertInfo{CertFile: blocked, KeyFile: keyPath}}
	if err := l.writeCert(&certificate.Resource{Certificate: newCert, PrivateKey: newKey}); err == nil {
		t.Fatal("rename to directory accepted")
	}
	got, err := os.ReadFile(keyPath)
	if err != nil || string(got) != string(oldKey) {
		t.Fatal("old key was not restored")
	}
	l.config.CertFile = certPath
	if err := l.writeCert(&certificate.Resource{Certificate: newCert, PrivateKey: newKey}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(keyPath)
	if err != nil || string(got) != string(newKey) {
		t.Fatal("new key missing")
	}
	got, err = os.ReadFile(certPath)
	if err != nil || string(got) != string(newCert) {
		t.Fatal("new certificate missing")
	}
}

func TestACMEGuardBackoffAndCrash(t *testing.T) {
	c := &panel.CertInfo{CertDomain: "test.example", CertFile: filepath.Join(t.TempDir(), "cert.pem")}
	transient := &net.DNSError{Err: "timeout", IsTimeout: true}
	calls := 0
	op := func() error { calls++; return transient }
	for attempt := 1; attempt <= 3; attempt++ {
		if err := guardedACME(c, op); err == nil {
			t.Fatal("failure accepted")
		}
		if err := guardedACME(c, op); err == nil || calls != attempt {
			t.Fatal("backoff bypassed")
		}
		if attempt < 3 {
			if err := saveACMEState(acmeStatePath(c), acmeState{Failures: attempt, NextAttempt: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := guardedACME(c, op); err == nil || calls != 3 {
		t.Fatal("attempt limit bypassed")
	}
	if err := os.WriteFile(acmeStatePath(c), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if guardedACME(c, op) == nil || calls != 3 {
		t.Fatal("corrupt state must fail closed")
	}
	if err := os.WriteFile(acmeStatePath(c)+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if guardedACME(c, op) == nil || calls != 3 {
		t.Fatal("cross-process lock ignored")
	}
}

func TestACMERetryAfterAndClassification(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, header := range []string{"172800", now.Add(48 * time.Hour).Format("Mon, 02 Jan 2006 15:04:05 GMT")} {
		err := errors.Join(errors.New("wrapped"), &acme.RateLimitedError{ProblemDetails: &acme.ProblemDetails{HTTPStatus: 429}, RetryAfter: header})
		s := nextACMEState(err, 1, now)
		if !s.Stopped || !s.NextAttempt.Equal(now.Add(48*time.Hour)) {
			t.Fatalf("rate limit: %+v", s)
		}
	}
	if s := nextACMEState(&acme.ProblemDetails{HTTPStatus: 400, Type: "urn:ietf:params:acme:error:unauthorized"}, 1, now); !s.Stopped {
		t.Fatal("validation failure retried")
	}
	if s := nextACMEState(&acme.ProblemDetails{HTTPStatus: 503}, 1, now); s.Stopped || !s.NextAttempt.Equal(now.Add(time.Hour)) {
		t.Fatal("transient backoff")
	}
	if _, err := (&Lego{}).CheckCert([]byte("invalid")); err == nil {
		t.Fatal("invalid certificate accepted")
	}
	if _, err := (&User{}).DecodePrivate("invalid"); err == nil {
		t.Fatal("invalid private key accepted")
	}
}

func TestACMEProviderEnvironmentScope(t *testing.T) {
	t.Setenv("V2BOX_TEST_DNS_TOKEN", "before")
	err := withACMEEnvironment(map[string]string{"V2BOX_TEST_DNS_TOKEN": "during"}, func() error {
		if os.Getenv("V2BOX_TEST_DNS_TOKEN") != "during" {
			t.Fatal("challenge environment missing")
		}
		return errors.New("failed challenge")
	})
	if err == nil || os.Getenv("V2BOX_TEST_DNS_TOKEN") != "before" {
		t.Fatal("provider environment was not restored")
	}
}

func TestCertificateIdentityAndTrustBeforeSwitch(t *testing.T) {
	dir := t.TempDir()
	oldCert, oldKey := filepath.Join(dir, "old.pem"), filepath.Join(dir, "old.key")
	if err := generateSelfSslCertificate("old.example.test", oldCert, oldKey); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(oldCert)
	key, _ := os.ReadFile(oldKey)
	if _, err := validateCertificate(before, key, "new.example.test", false, false); err == nil {
		t.Fatal("wrong certificate identity accepted")
	}
	if _, err := validateCertificate(before, key, "old.example.test", true, false); err == nil {
		t.Fatal("untrusted public certificate accepted")
	}
	fp, err := validateCertificate(before, key, "old.example.test", false, false)
	if err != nil || len(fp) != 64 {
		t.Fatalf("valid pinned certificate: %v", err)
	}
	candidate := &panel.CertInfo{CertMode: "self", CertDomain: "new.example.test", CertFile: filepath.Join(dir, "new", "cert.pem"), KeyFile: filepath.Join(dir, "new", "key.pem")}
	c := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: candidate}}}
	if err := c.requestCert(); err != nil {
		t.Fatal(err)
	}
	if c.info.CertificateFingerprint == fp || len(c.info.CertificateFingerprint) != 64 {
		t.Fatal("new certificate fingerprint missing")
	}
	after, _ := os.ReadFile(oldCert)
	afterKey, _ := os.ReadFile(oldKey)
	if string(before) != string(after) || string(key) != string(afterKey) {
		t.Fatal("previous certificate overwritten")
	}
	certBlock, _ := pem.Decode(before)
	keyBlock, _ := pem.Decode(key)
	expired, _ := x509.ParseCertificate(certBlock.Bytes)
	private, _ := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	expired.NotBefore = time.Now().Add(-2 * time.Hour)
	expired.NotAfter = time.Now().Add(-time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, expired, expired, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCertificate(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, "old.example.test", false, false); err == nil {
		t.Fatal("expired certificate accepted")
	}
	writer := &Lego{config: &panel.CertInfo{CertMode: "http", CertDomain: "new.example.test", CertFile: oldCert, KeyFile: oldKey}}
	if writer.writeCert(&certificate.Resource{Certificate: before, PrivateKey: key}) == nil {
		t.Fatal("ACME writer accepted a mismatched certificate")
	}
	unchanged, _ := os.ReadFile(oldCert)
	if string(unchanged) != string(before) {
		t.Fatal("rejected renewal changed old certificate")
	}
	// An existing but incorrect file must not count as successful preparation.
	bad := *candidate
	bad.CertDomain = "other.example.test"
	failed := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: &bad}}}
	if failed.requestCert() == nil || failed.certPrepared {
		t.Fatal("existing wrong-domain certificate accepted")
	}
	if _, err := validateCertificate(before, []byte("invalid"), "old.example.test", false, false); err == nil {
		t.Fatal("invalid private key accepted")
	}
}
func TestTransitionDNSAddresses(t *testing.T) {
	for _, tc := range []struct {
		got      []string
		expected string
		ok       bool
	}{
		{[]string{"8.8.8.8"}, "8.8.8.8", true}, {nil, "", true}, {nil, "8.8.8.8", false},
		{[]string{"1.1.1.1"}, "8.8.8.8", false}, {[]string{"8.8.8.8", "1.1.1.1"}, "8.8.8.8", false},
		{[]string{"2001:4860:4860::8888"}, "", false},
	} {
		if (checkDNSAddresses(tc.got, tc.expected) == nil) != tc.ok {
			t.Fatalf("unexpected DNS validation for %#v", tc)
		}
	}
	if verifyTransitionDNS(&panel.DomainTransition{Domain: "example.test"}) == nil {
		t.Fatal("empty addresses accepted")
	}
}

type transitionDNSRoundTrip func(*http.Request) (*http.Response, error)

func (f transitionDNSRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRelayCertificateDNSUsesEntryAndRejectsRedirects(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = transitionDNSRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("name") != "entry.example.test" {
			t.Fatal("origin hostname used for entry certificate")
		}
		body := `{"Status":0,"Answer":[]}`
		if r.URL.Query().Get("type") == "A" {
			body = `{"Status":0,"Answer":[{"Type":1,"Data":"1.1.1.1"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	transition := &panel.DomainTransition{Domain: "entry.example.test", Addresses: map[string]string{"ipv4": "1.1.1.1"}}
	if err := lookupTransitionDNS(context.Background(), transition); err != nil || calls != 4 {
		t.Fatalf("relay entry verification: %v, calls %d", err, calls)
	}
	transition.Addresses["ipv4"] = "8.8.8.8"
	if lookupTransitionDNS(context.Background(), transition) == nil {
		t.Fatal("origin IP substituted for ingress IP")
	}
	calls = 0
	http.DefaultTransport = transitionDNSRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{"Location": []string{"https://unexpected.example.test/"}}, Request: r}, nil
	})
	if lookupTransitionDNS(context.Background(), transition) == nil || calls != 1 {
		t.Fatal("DNS resolver redirect followed")
	}
}

func TestACMEGuardIndependentCertificateDirectories(t *testing.T) {
	a := &panel.CertInfo{CertDomain: "same.example", CertFile: filepath.Join(t.TempDir(), "cert.pem")}
	b := *a
	b.CertFile = filepath.Join(t.TempDir(), "cert.pem")
	gate := acmeGateFor(a)
	gate <- struct{}{}
	defer func() { <-gate }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := guardedACMEContext(ctx, &b, func() error { return nil }); err != nil {
		t.Fatalf("another backend blocked ACME: %v", err)
	}
}
func TestACMEGuardSurvivesDirectoryMigration(t *testing.T) {
	old := &panel.CertInfo{CertDomain: "same.example", CertFile: filepath.Join(t.TempDir(), "old.pem")}
	if err := guardedACME(old, func() error { return errors.New("invalid provider") }); err == nil {
		t.Fatal("failure accepted")
	}
	next := *old
	next.LegacyCertFile = old.CertFile
	next.CertFile = filepath.Join(t.TempDir(), "new", "cert.pem")
	if err := inheritACMEGuard(&next); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := guardedACME(&next, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("directory migration bypassed ACME protection")
	}
}
func TestLegacyCertificateMigrationVerifiesIdentity(t *testing.T) {
	dir := t.TempDir()
	oldCert, oldKey := filepath.Join(dir, "old.cer"), filepath.Join(dir, "old.key")
	if err := generateSelfSslCertificate("a.example", oldCert, oldKey); err != nil {
		t.Fatal(err)
	}
	previous, _ := os.ReadFile(oldCert)
	for _, domain := range []string{"a.example", "b.example"} {
		cert := &panel.CertInfo{CertMode: "self", CertDomain: domain, CertFile: filepath.Join(dir, domain, "cert.pem"), KeyFile: filepath.Join(dir, domain, "key.pem"), LegacyCertFile: oldCert, LegacyKeyFile: oldKey}
		controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: cert}}}
		if err := controller.requestCert(); err != nil {
			t.Fatal(err)
		}
		contents, _ := os.ReadFile(cert.CertFile)
		if (string(contents) == string(previous)) != (domain == "a.example") {
			t.Fatal("legacy certificate migration ignored identity")
		}
	}
	after, _ := os.ReadFile(oldCert)
	if string(previous) != string(after) {
		t.Fatal("legacy pair was overwritten")
	}
}
