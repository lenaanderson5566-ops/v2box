package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/acme"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
)

// Serialize ACME operations, including providers which use process environment.
var acmeGate = make(chan struct{}, 1) // only legacy in-process environment users
var acmeDomainGates sync.Map

func acmeGateFor(c *panel.CertInfo) chan struct{} {
	gate, _ := acmeDomainGates.LoadOrStore(acmeStatePath(c), make(chan struct{}, 1))
	return gate.(chan struct{})
}

type acmeState struct {
	Failures    int       `json:"failures"`
	Stopped     bool      `json:"stopped"`
	NextAttempt time.Time `json:"nextAttempt"`
}

func acmeStatePath(c *panel.CertInfo) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSuffix(c.CertDomain, "."))))
	return filepath.Join(filepath.Dir(c.CertFile), ".acme-"+hex.EncodeToString(sum[:16])+".json")
}

func saveACMEState(name string, state acmeState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".acme-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, name)
}

// Three total attempts for known transient failures. Unknown/provider/validation
// failures require operator repair and explicit removal of the state file.
func nextACMEState(err error, failures int, now time.Time) acmeState {
	s := acmeState{Failures: failures, Stopped: true}
	var rate *acme.RateLimitedError
	var problem *acme.ProblemDetails
	var network net.Error
	if errors.As(err, &rate) {
		s.NextAttempt = now.Add(24 * time.Hour)
		if secs, e := strconv.ParseInt(rate.RetryAfter, 10, 32); e == nil && secs >= 0 {
			s.NextAttempt = now.Add(time.Duration(secs) * time.Second)
		} else if t, e := http.ParseTime(rate.RetryAfter); e == nil {
			s.NextAttempt = t
		}
		if s.NextAttempt.Before(now.Add(time.Hour)) {
			s.NextAttempt = now.Add(time.Hour)
		}
		// Rate limits always require manual recovery, even after Retry-After.
		return s
	}
	transient := errors.As(err, &network)
	if errors.As(err, &problem) {
		transient = problem.HTTPStatus >= 500
	}
	if transient && failures < 3 {
		s.Stopped = false
		delay := time.Hour
		if failures > 1 {
			delay = 6 * time.Hour
		}
		s.NextAttempt = now.Add(delay)
	}
	return s
}

func guardedACME(c *panel.CertInfo, operation func() error) error {
	acmeGate <- struct{}{}
	defer func() { <-acmeGate }()
	return guardedACMEContext(context.Background(), c, func() error { return withACMEEnvironment(c.DNSEnv, operation) })
}

func guardedACMEContext(ctx context.Context, c *panel.CertInfo, operation func() error) error {
	gate := acmeGateFor(c)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := acmeStatePath(c)
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(name+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("ACME locked; check other processes or interrupted attempt: %s.lock", name)
	}
	lock.Close()
	defer os.Remove(name + ".lock")
	var state acmeState
	b, err := os.ReadFile(name)
	if err == nil {
		if err = json.Unmarshal(b, &state); err != nil || state.Failures < 1 || state.Failures > 3 || (!state.Stopped && (state.NextAttempt.IsZero() || state.Failures >= 3)) {
			return fmt.Errorf("invalid ACME guard state; repair %s", name)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if state.Stopped || time.Now().Before(state.NextAttempt) {
		return fmt.Errorf("ACME paused (failures=%d, stopped=%t, not-before=%s); fix the cause before manually removing %s", state.Failures, state.Stopped, state.NextAttempt.Format(time.RFC3339), name)
	}
	// Crash/restart must not silently launch another order.
	if err = saveACMEState(name, acmeState{Failures: state.Failures + 1, Stopped: true}); err != nil {
		return err
	}
	err = operation()
	if err == nil {
		if e := os.Remove(name); e != nil && !os.IsNotExist(e) {
			return e
		}
		return nil
	}
	state = nextACMEState(err, state.Failures+1, time.Now())
	if e := saveACMEState(name, state); e != nil {
		return fmt.Errorf("ACME failed and guard persistence failed: %w", e)
	}
	return fmt.Errorf("ACME failed (stopped=%t, not-before=%s); protection saved to %s: %w", state.Stopped, state.NextAttempt.Format(time.RFC3339), name, err)
}

func withACMEEnvironment(values map[string]string, operation func() error) error {
	previous := make(map[string]*string)
	defer func() {
		for k, v := range previous {
			if v == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *v)
			}
		}
	}()
	for k, v := range values {
		if old, ok := os.LookupEnv(k); ok {
			previous[k] = &old
		} else {
			previous[k] = nil
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return operation()
}

// A directory migration must not bypass an interrupted or paused ACME order.
func inheritACMEGuard(cert *panel.CertInfo) error {
	if cert.LegacyCertFile == "" {
		return nil
	}
	legacy := *cert
	legacy.CertFile = cert.LegacyCertFile
	old, name := acmeStatePath(&legacy), acmeStatePath(cert)
	if old == name {
		return nil
	}
	_, lockErr := os.Stat(old + ".lock")
	data, err := os.ReadFile(old)
	if os.IsNotExist(err) {
		if lockErr != nil {
			return nil
		}
		// Permit an existing valid pair to start while pausing any new ACME order.
		data, err = json.Marshal(acmeState{Failures: 1, Stopped: true})
	}
	if err != nil {
		return err
	}
	var state acmeState
	if len(data) > 4096 || json.Unmarshal(data, &state) != nil {
		return fmt.Errorf("invalid legacy ACME guard; repair before migration")
	}
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
