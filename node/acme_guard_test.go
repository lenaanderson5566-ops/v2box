package node

import (
	"errors"
	"net"
	"os"
	"path/filepath"
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
