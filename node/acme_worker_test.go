package node

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
)

func TestACMEWorkerProcess(t *testing.T) {
	mode := os.Getenv("V2BOX_TEST_ACME_WORKER")
	if mode == "" {
		return
	}
	if mode == "stall" {
		os.WriteFile(os.Getenv("V2BOX_TEST_STARTED"), []byte("started"), 0600)
		time.Sleep(2 * time.Second)
		os.WriteFile(os.Getenv("V2BOX_TEST_LATE"), []byte("late write"), 0600)
		os.Exit(0)
	}
	var request acmeRequest
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(2)
	}
	json.NewEncoder(os.Stdout).Encode(acmeResponse{Certificate: &acmeCertificate{
		Certificate: []byte(request.Config.TlsCert), PrivateKey: []byte(request.Config.TlsKey),
	}})
	os.Exit(0)
}

func helperACMECommand(ctx context.Context, mode string) *exec.Cmd {
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestACMEWorkerProcess$")
	command.Env = append(os.Environ(), "V2BOX_TEST_ACME_WORKER="+mode)
	return command
}

func TestACMEWorkerCancellationReapsProcessWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	cert := &panel.CertInfo{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), CertDomain: "fixture.example"}
	started, late := filepath.Join(dir, "started"), filepath.Join(dir, "late")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := helperACMECommand(ctx, "stall")
	command.Env = append(command.Env, "V2BOX_TEST_STARTED="+started, "V2BOX_TEST_LATE="+late)
	done := make(chan error, 1)
	go func() {
		done <- guardedACMEContext(ctx, cert, func() error { return runACMECommand(ctx, cert, false, command) })
	}()
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("worker exited early: %v", err)
		case <-deadline:
			t.Fatal("worker failed to start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	if command.ProcessState == nil {
		t.Fatal("worker was not reaped")
	}
	for _, name := range []string{cert.CertFile, cert.KeyFile, late, acmeStatePath(cert) + ".lock"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("unexpected file: %s, %v", name, err)
		}
	}
	// Cancellation after starting an order must retain the manual recovery guard.
	data, err := os.ReadFile(acmeStatePath(cert))
	if err != nil {
		t.Fatal(err)
	}
	var state acmeState
	if json.Unmarshal(data, &state) != nil || !state.Stopped {
		t.Fatalf("lost interrupted-order protection: %s", data)
	}
}

func TestACMEWorkerPublishesOnlyValidatedPair(t *testing.T) {
	dir := t.TempDir()
	sourceCert, sourceKey := filepath.Join(dir, "source.pem"), filepath.Join(dir, "source.key")
	if err := generateSelfSslCertificate("fixture.example", sourceCert, sourceKey); err != nil {
		t.Fatal(err)
	}
	pem, _ := os.ReadFile(sourceCert)
	key, _ := os.ReadFile(sourceKey)
	cert := &panel.CertInfo{CertMode: "self", CertDomain: "fixture.example", CertFile: filepath.Join(dir, "active.pem"), KeyFile: filepath.Join(dir, "active.key"), TlsCert: string(pem), TlsKey: string(key)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runACMECommand(ctx, cert, false, helperACMECommand(ctx, "success")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cert.KeyFile)
	cert.TlsKey = "invalid"
	if err := runACMECommand(ctx, cert, false, helperACMECommand(ctx, "success")); err == nil {
		t.Fatal("invalid pair accepted")
	}
	after, _ := os.ReadFile(cert.KeyFile)
	if string(before) != string(after) {
		t.Fatal("failed worker changed active key")
	}
}

func TestACMEGuardCanceledWaitDoesNotConsumeAttempt(t *testing.T) {
	cert := &panel.CertInfo{CertDomain: "fixture.example", CertFile: filepath.Join(t.TempDir(), "cert.pem")}
	gate := acmeGateFor(cert)
	gate <- struct{}{}
	defer func() { <-gate }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	err := guardedACMEContext(ctx, cert, func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("guard ignored cancellation: %v", err)
	}
	if _, err := os.Stat(acmeStatePath(cert)); !os.IsNotExist(err) {
		t.Fatal("waiting consumed an ACME attempt")
	}
}

func TestACMEWorkerPreservesRetryClassification(t *testing.T) {
	retry := "7200"
	now := time.Now()
	for _, tc := range []struct {
		response acmeResponse
		failures int
		stopped  bool
	}{
		{acmeResponse{Failed: true, RetryAfter: &retry}, 1, true},
		{acmeResponse{Failed: true, Status: 503}, 1, false},
		{acmeResponse{Failed: true, Transient: true}, 1, false},
		{acmeResponse{Failed: true, Transient: true}, 3, true},
		{acmeResponse{Failed: true, Status: 400}, 1, true},
	} {
		state := nextACMEState(tc.response.err(), tc.failures, now)
		if state.Stopped != tc.stopped {
			t.Fatalf("wrong retry classification: %+v", state)
		}
	}
	state := nextACMEState((acmeResponse{Failed: true, RetryAfter: &retry}).err(), 1, now)
	if !state.NextAttempt.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("Retry-After lost")
	}
}

func TestTransitionDNSHonorsCanceledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyTransitionDNSContext(ctx, &panel.DomainTransition{Domain: "fixture.example", Addresses: map[string]string{"ipv4": "192.0.2.1"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DNS lookup proceeded: %v", err)
	}
}

func TestACMEWorkerDeadlineReapsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	dir := t.TempDir()
	cert := &panel.CertInfo{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	command := helperACMECommand(ctx, "stall")
	command.Env = append(command.Env, "V2BOX_TEST_STARTED="+filepath.Join(dir, "started"), "V2BOX_TEST_LATE="+filepath.Join(dir, "late"))
	err := runACMECommand(ctx, cert, false, command)
	if !errors.Is(err, context.DeadlineExceeded) || command.ProcessState == nil {
		t.Fatalf("deadline did not reap worker: %v", err)
	}
	if _, err := os.Stat(cert.CertFile); !os.IsNotExist(err) {
		t.Fatal("deadline published certificate")
	}
}

func acmeFixturePair(t *testing.T, domain string, remaining time.Duration) *panel.CertInfo {
	t.Helper()
	dir := t.TempDir()
	info := &panel.CertInfo{CertMode: "http", CertDomain: domain, CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	if err := generateSelfSslCertificate(domain, info.CertFile, info.KeyFile); err != nil {
		t.Fatal(err)
	}
	rewriteFixtureExpiry(t, info, remaining)
	return info
}
func rewriteFixtureExpiry(t *testing.T, info *panel.CertInfo, remaining time.Duration) {
	t.Helper()
	data, err := os.ReadFile(info.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(info.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(data)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leaf.NotBefore = time.Now().Add(-time.Hour)
	leaf.NotAfter = time.Now().Add(remaining)
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(info.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestSharedACMERenewalCoalescesAndRetainsKey(t *testing.T) {
	for _, mode := range []string{"http", "dns"} {
		t.Run(mode, func(t *testing.T) {
			cert := acmeFixturePair(t, "shared.example.test", 15*24*time.Hour)
			cert.CertMode = mode
			key, _ := os.ReadFile(cert.KeyFile)
			var calls atomic.Int32
			operation := func(context.Context) error { calls.Add(1); rewriteFixtureExpiry(t, cert, 90*24*time.Hour); return nil }
			var group sync.WaitGroup
			errors := make(chan error, 12)
			for range 12 {
				group.Go(func() { errors <- coordinatedACME(context.Background(), cert, true, operation) })
			}
			group.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("duplicate renewal orders: %d", calls.Load())
			}
			after, _ := os.ReadFile(cert.KeyFile)
			if string(after) != string(key) {
				t.Fatal("renewal replaced retained private key")
			}
			if _, err := os.Stat(acmeStatePath(cert)); !os.IsNotExist(err) {
				t.Fatal("successful renewal left a guard failure")
			}
		})
	}
}
func TestSharedHTTPOrdersDifferentDomainsSerializePort80(t *testing.T) {
	first := acmeFixturePair(t, "first.example.test", 15*24*time.Hour)
	second := acmeFixturePair(t, "second.example.test", 15*24*time.Hour)
	var running, maximum, calls atomic.Int32
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for _, cert := range []*panel.CertInfo{first, second} {
		group.Go(func() {
			errors <- coordinatedACME(context.Background(), cert, true, func(context.Context) error {
				count := running.Add(1)
				for old := maximum.Load(); count > old; old = maximum.Load() {
					if maximum.CompareAndSwap(old, count) {
						break
					}
				}
				defer running.Add(-1)
				time.Sleep(15 * time.Millisecond)
				calls.Add(1)
				rewriteFixtureExpiry(t, cert, 90*24*time.Hour)
				return nil
			})
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("HTTP challenge ownership: max=%d orders=%d", maximum.Load(), calls.Load())
	}
}
func TestSharedHTTPWaitCancellationPreservesGuardAndPair(t *testing.T) {
	cert := acmeFixturePair(t, "cancel.example.test", 15*24*time.Hour)
	before, _ := os.ReadFile(cert.CertFile)
	key, _ := os.ReadFile(cert.KeyFile)
	httpACMEGate <- struct{}{}
	defer func() { <-httpACMEGate }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := coordinatedACME(ctx, cert, true, func(context.Context) error { t.Error("canceled waiter launched an order"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cert.CertFile)
	afterKey, _ := os.ReadFile(cert.KeyFile)
	if string(before) != string(after) || string(key) != string(afterKey) {
		t.Fatal("canceled wait changed certificate")
	}
	if _, err := os.Stat(acmeStatePath(cert)); !os.IsNotExist(err) {
		t.Fatal("waiting consumed a renewal attempt")
	}
}
func TestSharedRenewalFailureRetainsCertificateAndGuard(t *testing.T) {
	cert := acmeFixturePair(t, "failure.example.test", 15*24*time.Hour)
	before, _ := os.ReadFile(cert.CertFile)
	key, _ := os.ReadFile(cert.KeyFile)
	err := coordinatedACME(context.Background(), cert, true, func(context.Context) error { return errors.New("controlled validation failure") })
	if err == nil {
		t.Fatal("failed renewal accepted")
	}
	after, _ := os.ReadFile(cert.CertFile)
	afterKey, _ := os.ReadFile(cert.KeyFile)
	if string(before) != string(after) || string(key) != string(afterKey) {
		t.Fatal("failed renewal changed certificate")
	}
	data, err := os.ReadFile(acmeStatePath(cert))
	if err != nil {
		t.Fatal(err)
	}
	var state acmeState
	if json.Unmarshal(data, &state) != nil || !state.Stopped {
		t.Fatal("failure lost retry guard")
	}
}

func TestRenewalResourceRetainsKeyAndRejectsWrongPair(t *testing.T) {
	cert := acmeFixturePair(t, "retained.example.test", 15*24*time.Hour)
	key, _ := os.ReadFile(cert.KeyFile)
	resource, err := renewalResource(cert)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.PrivateKey) == 0 || string(resource.PrivateKey) != string(key) {
		t.Fatal("renewal would generate a different key")
	}
	cert.CertDomain = "wrong.example.test"
	if _, err := renewalResource(cert); err == nil {
		t.Fatal("renewal accepted an unrelated domain")
	}
	cert.CertDomain = "retained.example.test"
	if err := os.WriteFile(cert.KeyFile, []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := renewalResource(cert); err == nil {
		t.Fatal("renewal ignored a broken private key")
	}
}

func TestRenewalWorkerCannotReplaceOriginalPrivateKey(t *testing.T) {
	original := acmeFixturePair(t, "worker.example.test", 15*24*time.Hour)
	original.CertMode = "self"
	beforeCert, _ := os.ReadFile(original.CertFile)
	beforeKey, _ := os.ReadFile(original.KeyFile)
	other := acmeFixturePair(t, "worker.example.test", 90*24*time.Hour)
	newCert, _ := os.ReadFile(other.CertFile)
	newKey, _ := os.ReadFile(other.KeyFile)
	original.TlsCert, original.TlsKey = string(newCert), string(newKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runACMECommand(ctx, original, true, helperACMECommand(ctx, "success")); err == nil {
		t.Fatal("renewal replaced a retained private key")
	}
	after, _ := os.ReadFile(original.CertFile)
	key, _ := os.ReadFile(original.KeyFile)
	if string(after) != string(beforeCert) || string(key) != string(beforeKey) {
		t.Fatal("mismatched renewal changed the existing pair")
	}
}

func TestSameKeyRenewalPreservesKeyInodeAndPermissions(t *testing.T) {
	cert := acmeFixturePair(t, "inode.example.test", 15*24*time.Hour)
	cert.CertMode = "self"
	old, _ := os.ReadFile(cert.CertFile)
	key, _ := os.ReadFile(cert.KeyFile)
	rewriteFixtureExpiry(t, cert, 90*24*time.Hour)
	next, _ := os.ReadFile(cert.CertFile)
	if err := os.WriteFile(cert.CertFile, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cert.KeyFile, 0400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cert.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Lego{config: cert}).writeCert(&certificate.Resource{Certificate: next, PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(cert.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0400 {
		t.Fatal("renewal rewrote the retained private key file")
	}
}
