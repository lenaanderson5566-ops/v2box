package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
