package node

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/go-acme/lego/v4/certificate"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"math/big"
	"os"
	"time"

	"github.com/lenaanderson5566-ops/v2box/common/file"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) renewCertTask(_ context.Context) error {
	cert := c.info.Common.CertInfo
	pem, err := os.ReadFile(cert.CertFile)
	if err != nil {
		return err
	}
	due, err := (&Lego{}).CheckCert(pem)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	err = guardedACME(cert, func() error {
		l, err := NewLego(cert)
		if err != nil {
			return err
		}
		return l.RenewCert()
	})
	if err != nil {
		log.WithField("tag", c.tag).WithError(err).Warn("Certificate renewal paused or failed; existing certificate retained")
	}

	return nil
}

// validateCertificate always verifies the key pair, identity and validity. Public
// certificate modes additionally verify the full chain against system roots.
func validateCertificate(certPEM, keyPEM []byte, domain string, public, legacyCN bool) (string, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("invalid certificate/key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	if err = leaf.VerifyHostname(domain); err != nil && !(legacyCN && len(leaf.DNSNames) == 0 && leaf.Subject.CommonName == domain) {
		return "", err
	}
	if len(leaf.ExtKeyUsage) > 0 {
		serverAuth := false
		for _, usage := range leaf.ExtKeyUsage {
			if usage == x509.ExtKeyUsageAny || usage == x509.ExtKeyUsageServerAuth {
				serverAuth = true
			}
		}
		if !serverAuth {
			return "", fmt.Errorf("certificate does not permit server authentication")
		}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Add(time.Minute).Before(leaf.NotAfter) {
		return "", fmt.Errorf("certificate is not currently valid")
	}
	if public {
		intermediates := x509.NewCertPool()
		for _, der := range pair.Certificate[1:] {
			cert, e := x509.ParseCertificate(der)
			if e != nil {
				return "", e
			}
			intermediates.AddCert(cert)
		}
		if _, err = leaf.Verify(x509.VerifyOptions{DNSName: domain, Intermediates: intermediates}); err != nil {
			return "", fmt.Errorf("certificate trust verification: %w", err)
		}
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(fingerprint[:]), nil
}

func (c *Controller) requestCert() error {
	if c.certPrepared {
		return nil
	}
	cert := c.info.Common.CertInfo
	transition := c.info.Common.DomainTransition
	if transition != nil {
		if transition.Domain != cert.CertDomain {
			return fmt.Errorf("certificate transition identity mismatch")
		}
		if err := verifyTransitionDNS(transition); err != nil {
			return err
		}
	}
	switch cert.CertMode {
	case "none", "":
		return fmt.Errorf("TLS requires a certificate mode")
	case "file", "dns", "http", "self":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("certificate and key paths are required")
		}
		exists := file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile)
		if !exists {
			// A partial pair must never be overwritten silently.
			if file.IsExist(cert.CertFile) || file.IsExist(cert.KeyFile) {
				return fmt.Errorf("incomplete certificate/key pair")
			}
			switch cert.CertMode {
			case "dns", "http":
				if err := guardedACME(cert, func() error {
					l, e := NewLego(cert)
					if e != nil {
						return e
					}
					return l.CreateCert()
				}); err != nil {
					return err
				}
			case "self":
				if err := generateSelfSslCertificate(cert.CertDomain, cert.CertFile, cert.KeyFile); err != nil {
					return err
				}
			}
		}
	case "remote":
	default:
		return fmt.Errorf("unsupported certmode: %s", cert.CertMode)
	}
	certPEM, keyPEM := []byte(cert.TlsCert), []byte(cert.TlsKey)
	if cert.CertMode != "remote" {
		var err error
		certPEM, err = os.ReadFile(cert.CertFile)
		if err != nil {
			return err
		}
		keyPEM, err = os.ReadFile(cert.KeyFile)
		if err != nil {
			return err
		}
	}
	fingerprint, err := validateCertificate(certPEM, keyPEM, cert.CertDomain, cert.CertMode != "self" && cert.CertMode != "remote", transition == nil && cert.CertMode == "remote")
	if err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}
	c.info.CertificateFingerprint = fingerprint
	c.certPrepared = true
	return nil
}

func generateSelfSslCertificate(domain, certPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		Version:      3,
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject: pkix.Name{
			CommonName: domain,
		},
		DNSNames:              []string{domain},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(30, 0, 0),
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return err
	}
	if err := checkPath(certPath); err != nil {
		return err
	}
	if err := checkPath(keyPath); err != nil {
		return err
	}
	return (&Lego{config: &panel.CertInfo{CertFile: certPath, KeyFile: keyPath, CertDomain: domain}}).writeCert(&certificate.Resource{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}),
		PrivateKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	})
}
