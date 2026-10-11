package node

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
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

func (c *Controller) requestCert() error {
	cert := c.info.Common.CertInfo
	switch cert.CertMode {
	case "none", "":
	case "file":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path not exist")
		}
	case "dns", "http":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path not exist")
		}
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		if err := guardedACME(cert, func() error {
			l, err := NewLego(cert)
			if err != nil {
				return err
			}
			return l.CreateCert()
		}); err != nil {
			return err
		}

	case "self":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path not exist")
		}
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		err := generateSelfSslCertificate(
			cert.CertDomain,
			cert.CertFile,
			cert.KeyFile)
		if err != nil {
			return fmt.Errorf("generate self cert error: %s", err)
		}
	case "remote":
		return nil // sing-box reads the certificate from memory
	default:
		return fmt.Errorf("unsupported certmode: %s", cert.CertMode)
	}
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
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
}
