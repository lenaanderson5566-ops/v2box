package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
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

// Use the standard library's public test CA, also used by the HTTPS panel
// fixtures. The actual subprocess/parent publication and sing-box TLS listener
// run normally; no ACME authority, DNS provider or real certificate is contacted.
func TestAutomaticCertificateMaintenanceServesRenewedPair(t *testing.T) {
	caServer := httptest.NewTLSServer(nil)
	ca := caServer.Certificate()
	caKey := caServer.TLS.Certificates[0].PrivateKey
	caServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	fixtureRootsOnce.Do(func() { x509.SetFallbackRoots(roots) })
	for _, mode := range []string{"http", "dns"} {
		for _, phase := range []string{"periodic", "restart-after-expiry"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				remaining := 15 * 24 * time.Hour
				if phase == "restart-after-expiry" {
					remaining = -time.Hour
				}
				cert := acmeFixturePair(t, "renew.example.test", remaining)
				cert.CertMode = mode
				keyPEM, err := os.ReadFile(cert.KeyFile)
				if err != nil {
					t.Fatal(err)
				}
				block, _ := pem.Decode(keyPEM)
				key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				issue := func(duration time.Duration) []byte {
					t.Helper()
					leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{cert.CertDomain}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(duration), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
					der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
					if err != nil {
						t.Fatal(err)
					}
					return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})...)
				}
				before, next := issue(remaining), issue(90*24*time.Hour)
				if err = os.WriteFile(cert.CertFile, before, 0600); err != nil {
					t.Fatal(err)
				}
				// Only the synthetic worker response uses these fields. File modes
				// continue to serve the pair on disk until publication succeeds.
				cert.TlsCert, cert.TlsKey = string(next), string(keyPEM)
				probe, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := probe.Addr().(*net.TCPAddr).Port
				probe.Close()
				info := &panel.NodeInfo{Tag: "certificate-" + mode + "-" + phase, Type: "vless", Security: panel.Tls, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: port, Network: "tcp", CertInfo: cert}}
				controller := &Controller{info: info, tag: info.Tag}
				var calls int
				acquire := func(ctx context.Context, requested *panel.CertInfo, renew bool) error {
					calls++
					if !renew || requested != cert {
						t.Fatal("maintenance did not renew the existing identity/key")
					}
					return coordinatedACME(ctx, requested, renew, func(ctx context.Context) error {
						return runACMECommand(ctx, requested, renew, helperACMECommand(ctx, "success"))
					})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if err = controller.prepareCertificate(ctx, acquire); err != nil {
					t.Fatal(err)
				}
				if phase == "periodic" && calls != 0 {
					t.Fatal("usable certificate delayed startup for an ACME order")
				}
				if phase == "restart-after-expiry" && calls != 1 {
					t.Fatal("expired automatic certificate was not recovered before startup")
				}
				// A final domain-publication hash can arrive while certificate
				// maintenance runs. Subsequent receipts must keep that final hash.
				controller.info.ConfigurationHash = strings.Repeat("a", 64)
				controller.updateReceipt(strings.Repeat("b", 64), "")
				var received []configurationReceipt
				var receivedMu sync.Mutex
				receipts := func() []configurationReceipt {
					receivedMu.Lock()
					defer receivedMu.Unlock()
					return append([]configurationReceipt(nil), received...)
				}
				apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/configuration") {
						w.WriteHeader(304)
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/capabilities") {
						t.Errorf("unexpected receipt fixture endpoint %s", r.URL.Path)
						w.WriteHeader(404)
						return
					}
					var payload struct {
						Configuration struct {
							Hash        string `json:"hash"`
							Fingerprint string `json:"certificateFingerprint"`
						} `json:"configuration"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					receivedMu.Lock()
					received = append(received, configurationReceipt{payload.Configuration.Hash, payload.Configuration.Fingerprint})
					count := len(received)
					receivedMu.Unlock()
					if count == 1 {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"data":{"accepted":true}}`))
				}))
				defer apiServer.Close()
				retries := 0
				controller.apiClient, err = panel.New(&conf.NodeConfig{APIHost: apiServer.URL, NodeID: 1, Key: "fixture-only", APIVersion: "v10", RetryCount: &retries})
				if err != nil {
					t.Fatal(err)
				}
				defer controller.apiClient.CloseIdleConnections()
				cfg := conf.New()
				cfg.OutboundAllowCIDRs = []string{"127.0.0.0/8"}
				server := core.New(cfg)
				defer server.Close()
				if err = server.AddNode(info.Tag, info); err != nil {
					t.Fatal(err)
				}
				if err = server.SetUsers(info.Tag, nil); err != nil {
					t.Fatal(err)
				}
				peek := func() ([]byte, error) {
					conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{RootCAs: roots, ServerName: cert.CertDomain})
					if err != nil {
						return nil, err
					}
					defer conn.Close()
					return conn.ConnectionState().PeerCertificates[0].Raw, nil
				}
				if phase == "periodic" {
					got, err := peek()
					oldBlock, _ := pem.Decode(before)
					if err != nil || !bytes.Equal(got, oldBlock.Bytes) {
						t.Fatalf("listener did not initially serve old usable pair: %v", err)
					}
					// A transient provider failure retains the valid pair. Its guard
					// permits the next scheduled attempt only after the backoff.
					failed := func(ctx context.Context, requested *panel.CertInfo, renew bool) error {
						return coordinatedACME(ctx, requested, renew, func(context.Context) error { return &net.DNSError{Err: "synthetic timeout", IsTimeout: true} })
					}
					if err = controller.renewCertificate(ctx, failed); err != nil {
						t.Fatal(err)
					}
					retained, _ := os.ReadFile(cert.CertFile)
					if !bytes.Equal(retained, before) {
						t.Fatal("provider failure changed active certificate")
					}
					if err = controller.renewCertificate(ctx, acquire); err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatal("paused guard was bypassed")
					}
					if err = saveACMEState(acmeStatePath(cert), acmeState{Failures: 1, NextAttempt: time.Now().Add(-time.Second)}); err != nil {
						t.Fatal(err)
					}
					if err = controller.renewCertificate(ctx, acquire); err != nil {
						t.Fatal(err)
					}
					if calls != 2 {
						t.Fatal("eligible automatic retry did not run")
					}
				}
				deadline := time.Now().Add(10 * time.Second)
				nextBlock, _ := pem.Decode(next)
				for {
					got, err := peek()
					if err == nil && bytes.Equal(got, nextBlock.Bytes) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("renewed certificate was not served without restarting: %v", err)
					}
					time.Sleep(25 * time.Millisecond)
				}
				fingerprint, err := validateCertificate(next, keyPEM, cert.CertDomain, true, false)
				if err != nil || controller.currentCertificateFingerprint() != fingerprint {
					t.Fatal("renewal receipt retained old certificate fingerprint")
				}
				if phase == "periodic" {
					if len(receipts()) != 1 {
						t.Fatal("renewal did not attempt its certificate receipt")
					}
					if _, err = controller.apiClient.GetNodeInfo(ctx); err != nil {
						t.Fatal(err)
					}
					if len(receipts()) != 2 {
						t.Fatal("normal capabilities polling did not retry the pending receipt")
					}
					for _, receipt := range receipts() {
						if receipt.hash != strings.Repeat("b", 64) || receipt.fingerprint != fingerprint {
							t.Fatal("renewal receipt regressed final configuration hash or certificate fingerprint")
						}
					}
				}
				afterKey, _ := os.ReadFile(cert.KeyFile)
				if !bytes.Equal(afterKey, keyPEM) {
					t.Fatal("automatic renewal changed the retained private key")
				}
			})
		}
	}
}

func TestConcurrentCertificateReceiptUpdatesPreserveBothFields(t *testing.T) {
	controller := &Controller{info: &panel.NodeInfo{ConfigurationHash: "initial", CertificateFingerprint: "old"}}
	for range 100 {
		controller.appliedReceipt.Store(nil)
		var group sync.WaitGroup
		group.Go(func() { controller.updateReceipt("final", "") })
		group.Go(func() { controller.updateReceipt("", "renewed") })
		group.Wait()
		if receipt := controller.currentReceipt(); receipt.hash != "final" || receipt.fingerprint != "renewed" {
			t.Fatal("concurrent renewal/publication lost one receipt field")
		}
	}
}

func TestCertificateMaintenanceDoesNotAcquireForExternalModesOrWrongIdentity(t *testing.T) {
	for _, mode := range []string{"file", "remote", "self", "none", ""} {
		t.Run(mode, func(t *testing.T) {
			cert := acmeFixturePair(t, "external.example.test", -time.Hour)
			cert.CertMode = mode
			certPEM, _ := os.ReadFile(cert.CertFile)
			keyPEM, _ := os.ReadFile(cert.KeyFile)
			cert.TlsCert, cert.TlsKey = string(certPEM), string(keyPEM)
			controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: cert}}}
			acquire := func(context.Context, *panel.CertInfo, bool) error {
				t.Fatal("external certificate mode attempted ACME")
				return nil
			}
			if err := controller.renewCertificate(context.Background(), acquire); err != nil {
				t.Fatal(err)
			}
			if err := controller.prepareCertificate(context.Background(), acquire); err == nil {
				t.Fatal("expired external certificate was accepted")
			}
		})
	}
	cert := acmeFixturePair(t, "right.example.test", -time.Hour)
	cert.CertDomain = "wrong.example.test"
	controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: cert}}}
	if err := controller.prepareCertificate(context.Background(), func(context.Context, *panel.CertInfo, bool) error {
		t.Fatal("wrong identity launched renewal")
		return nil
	}); err == nil {
		t.Fatal("wrong certificate identity accepted")
	}
}
