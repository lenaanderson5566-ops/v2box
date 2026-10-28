package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/certificate"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
)

// Providers have third-party polling loops without a context API. A separate
// process lets cancellation stop and reap them before releasing the ACME guard.
// Only the parent publishes certificates; killing a worker cannot replace a pair.
const acmeTimeout = 4 * time.Minute
const acmeResultLimit = 4 << 20

type acmeRequest struct {
	Config *panel.CertInfo
	Renew  bool
}

// lego.Resource intentionally excludes PEM fields from JSON; the private pipe
// protocol carries only the pair needed for parent-side validation/publication.
type acmeCertificate struct {
	Certificate []byte
	PrivateKey  []byte
}

type acmeResponse struct {
	Certificate *acmeCertificate
	Failed      bool
	Transient   bool
	Status      int
	RetryAfter  *string
}

type acmeNetworkError struct{}

func (acmeNetworkError) Error() string   { return "ACME provider network failure" }
func (acmeNetworkError) Timeout() bool   { return true }
func (acmeNetworkError) Temporary() bool { return true }

func (r acmeResponse) err() error {
	if !r.Failed {
		return nil
	}
	if r.RetryAfter != nil {
		return &acme.RateLimitedError{ProblemDetails: &acme.ProblemDetails{HTTPStatus: 429}, RetryAfter: *r.RetryAfter}
	}
	if r.Status != 0 {
		return &acme.ProblemDetails{HTTPStatus: r.Status}
	}
	if r.Transient {
		return acmeNetworkError{}
	}
	return errors.New("ACME operation failed; verify provider configuration and certificate guard state")
}

// HTTP-01 workers for different identities still need the same TCP port 80.
var httpACMEGate = make(chan struct{}, 1)

func runACME(ctx context.Context, cert *panel.CertInfo, renew bool) error {
	return coordinatedACME(ctx, cert, renew, func(ctx context.Context) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return runACMECommand(ctx, cert, renew, exec.CommandContext(ctx, executable, "internal-acme"))
	})
}

func coordinatedACME(ctx context.Context, cert *panel.CertInfo, renew bool, operation func(context.Context) error) error {
	if cert.CertMode == "http" {
		select {
		case httpACMEGate <- struct{}{}:
			defer func() { <-httpACMEGate }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Queuing behind another HTTP identity must not consume this order's
	// network timeout; shutdown/caller cancellation still interrupts the wait.
	ctx, cancel := context.WithTimeout(ctx, acmeTimeout)
	defer cancel()
	return guardedACMEContext(ctx, cert, func() error {
		// Peers share paths, not credentials. Recheck the pair under its lock:
		// another controller may have completed the order while we waited.
		// Trust is checked by requestCertContext and before any worker publishes.
		if pem, readErr := os.ReadFile(cert.CertFile); readErr == nil {
			if key, keyErr := os.ReadFile(cert.KeyFile); keyErr == nil {
				if _, validErr := validateCertificate(pem, key, cert.CertDomain, false, false); validErr == nil {
					due, dueErr := (&Lego{}).CheckCert(pem)
					if dueErr == nil && (!renew || !due) {
						return nil
					}
				}
			}
		}
		return operation(ctx)
	})
}

type limitedACMEOutput struct{ bytes.Buffer }

func (b *limitedACMEOutput) Write(p []byte) (int, error) {
	if len(p) > acmeResultLimit-b.Len() {
		return 0, errors.New("ACME result exceeds limit")
	}
	return b.Buffer.Write(p)
}

func runACMECommand(ctx context.Context, cert *panel.CertInfo, renew bool, command *exec.Cmd) error {
	var retainedKey []byte
	if renew {
		resource, err := renewalResource(cert)
		if err != nil {
			return err
		}
		retainedKey = resource.PrivateKey
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := json.Marshal(acmeRequest{Config: cert, Renew: renew})
	if err != nil {
		return err
	}
	command.Stdin = bytes.NewReader(request) // Credentials never enter command-line arguments.
	var output limitedACMEOutput
	command.Stdout = &output
	command.Stderr = io.Discard // Provider errors can include DNS credentials.
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ACME worker failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var result acmeResponse
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		return errors.New("invalid ACME worker result")
	}
	if err := result.err(); err != nil {
		return err
	}
	if result.Certificate == nil {
		return errors.New("missing ACME certificate result")
	}
	if renew {
		if _, err := tls.X509KeyPair(result.Certificate.Certificate, retainedKey); err != nil {
			return errors.New("renewed certificate does not match the retained private key")
		}
		result.Certificate.PrivateKey = retainedKey
	}
	return (&Lego{config: cert}).writeCert(&certificate.Resource{Certificate: result.Certificate.Certificate, PrivateKey: result.Certificate.PrivateKey})
}

// RunACMEWorker is the private subprocess protocol, never a server goroutine.
func RunACMEWorker(input io.Reader, output io.Writer) error {
	var request acmeRequest
	if err := json.NewDecoder(io.LimitReader(input, acmeResultLimit)).Decode(&request); err != nil {
		return errors.New("invalid ACME request")
	}
	if request.Config == nil || (request.Config.CertMode != "dns" && request.Config.CertMode != "http") {
		return errors.New("invalid ACME mode")
	}
	var response acmeResponse
	var obtained *certificate.Resource
	err := withACMEEnvironment(request.Config.DNSEnv, func() error {
		l, err := NewLego(request.Config)
		if err != nil {
			return err
		}
		if request.Renew {
			resource, err := renewalResource(request.Config)
			if err != nil {
				return err
			}
			obtained, err = l.client.Certificate.Renew(resource, true, false, "")
			return err
		}
		obtained, err = l.client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{request.Config.CertDomain}, Bundle: true})
		return err
	})
	if err == nil && obtained != nil {
		response.Certificate = &acmeCertificate{Certificate: obtained.Certificate, PrivateKey: obtained.PrivateKey}
	}
	if err != nil {
		response.Certificate = nil
		response.Failed = true
		var rate *acme.RateLimitedError
		var problem *acme.ProblemDetails
		var network net.Error
		if errors.As(err, &rate) {
			response.RetryAfter = &rate.RetryAfter
		}
		if errors.As(err, &problem) {
			response.Status = problem.HTTPStatus
		}
		response.Transient = errors.As(err, &network)
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return fmt.Errorf("write ACME result: %w", err)
	}
	return nil
}
