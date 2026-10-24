package node

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/lego"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/file"
)

type Lego struct {
	client *lego.Client
	config *panel.CertInfo
}

func NewLego(config *panel.CertInfo) (*Lego, error) {
	user, err := NewLegoUser(path.Join(path.Dir(config.CertFile),
		"user",
		fmt.Sprintf("user-%s.json", config.Email)),
		config.Email)
	if err != nil {
		return nil, fmt.Errorf("create user error: %w", err)
	}
	c := lego.NewConfig(user)
	c.Certificate.KeyType = certcrypto.RSA2048
	client, err := lego.NewClient(c)
	if err != nil {
		return nil, err
	}
	l := Lego{
		client: client,
		config: config,
	}
	err = l.SetProvider()
	if err != nil {
		return nil, fmt.Errorf("set provider error: %w", err)
	}
	return &l, nil
}

func checkPath(p string) error {
	if !file.IsExist(path.Dir(p)) {
		err := os.MkdirAll(path.Dir(p), 0755)
		if err != nil {
			return fmt.Errorf("create dir error: %w", err)
		}
	}
	return nil
}

func (l *Lego) SetProvider() error {
	switch l.config.CertMode {
	case "http":
		err := l.client.Challenge.SetHTTP01Provider(http01.NewProviderServer("", "80"))
		if err != nil {
			return err
		}
	case "dns":
		// guardedACME keeps provider environment in scope for the entire challenge.

		p, err := dns.NewDNSChallengeProviderByName(l.config.Provider)
		if err != nil {
			return fmt.Errorf("create dns challenge provider error: %w", err)
		}
		err = l.client.Challenge.SetDNS01Provider(p)
		if err != nil {
			return fmt.Errorf("set dns provider error: %w", err)
		}
	}
	return nil
}

func (l *Lego) CreateCert() (err error) {
	request := certificate.ObtainRequest{
		Domains: []string{l.config.CertDomain},
		Bundle:  true,
	}
	certificates, err := l.client.Certificate.Obtain(request)
	if err != nil {
		return fmt.Errorf("obtain certificate error: %w", err)
	}
	err = l.writeCert(certificates)
	if err != nil {
		return fmt.Errorf("write certificate error: %w", err)
	}
	return nil
}

func (l *Lego) RenewCert() error {
	file, err := os.ReadFile(l.config.CertFile)
	if err != nil {
		return fmt.Errorf("read cert file error: %w", err)
	}
	if due, err := l.CheckCert(file); err != nil {
		return fmt.Errorf("check cert error: %w", err)
	} else if !due {
		return nil
	}
	res, err := l.client.Certificate.Renew(certificate.Resource{
		Domain:      l.config.CertDomain,
		Certificate: file,
	}, true, false, "")
	if err != nil {
		return err
	}
	err = l.writeCert(res)
	if err != nil {
		return fmt.Errorf("write certificate error: %w", err)
	}
	return nil
}

func (l *Lego) CheckCert(file []byte) (bool, error) {
	cert, err := certcrypto.ParsePEMCertificate(file)
	if err != nil {
		return false, err
	}
	notAfter := int(time.Until(cert.NotAfter).Hours() / 24.0)
	if notAfter > 30 {
		return false, nil
	}
	return true, nil
}
func (l *Lego) parseParams(path string) string {
	r := strings.NewReplacer("{domain}", l.config.CertDomain,
		"{email}", l.config.Email)
	return r.Replace(path)
}
func stageCertFile(name string, data []byte, mode os.FileMode) (string, error) {
	if err := checkPath(name); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".acme-cert-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func (l *Lego) writeCert(certificates *certificate.Resource) error {
	if l.config.CertMode == "http" || l.config.CertMode == "dns" {
		if _, err := validateCertificate(certificates.Certificate, certificates.PrivateKey, l.config.CertDomain, true, false); err != nil {
			return err
		}
	}
	if _, err := tls.X509KeyPair(certificates.Certificate, certificates.PrivateKey); err != nil {
		return err
	}
	certPath, keyPath := l.parseParams(l.config.CertFile), l.parseParams(l.config.KeyFile)
	certAbs, _ := filepath.Abs(certPath)
	keyAbs, _ := filepath.Abs(keyPath)
	if certAbs == keyAbs {
		return fmt.Errorf("certificate and key paths must differ")
	}
	certTmp, err := stageCertFile(certPath, certificates.Certificate, 0644)
	if err != nil {
		return err
	}
	defer os.Remove(certTmp)
	keyTmp, err := stageCertFile(keyPath, certificates.PrivateKey, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(keyTmp)
	previous, err := os.ReadFile(keyPath)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	backup := ""
	if existed {
		backup, err = stageCertFile(keyPath, previous, 0600)
		if err != nil {
			return err
		}
		defer os.Remove(backup)
	}
	if err = os.Rename(keyTmp, keyPath); err != nil {
		return err
	}
	if err = os.Rename(certTmp, certPath); err != nil {
		var restore error
		if existed {
			restore = os.Rename(backup, keyPath)
		} else {
			restore = os.Remove(keyPath)
		}
		return errors.Join(err, restore)
	}
	return nil
}

type User struct {
	Email        string                 `json:"Email"`
	Registration *registration.Resource `json:"Registration"`
	key          crypto.PrivateKey
	KeyEncoded   string `json:"Key"`
}

func (u *User) GetEmail() string {
	return u.Email
}
func (u *User) GetRegistration() *registration.Resource {
	return u.Registration
}
func (u *User) GetPrivateKey() crypto.PrivateKey {
	return u.key
}

func NewLegoUser(path string, email string) (*User, error) {
	var user User
	if file.IsExist(path) {
		err := user.Load(path)
		if err != nil {
			return nil, err
		}
		if user.Email != email {
			user.Registration = nil
			user.Email = email
			err := registerUser(&user, path)
			if err != nil {
				return nil, err
			}
		}
	} else {
		user.Email = email
		err := registerUser(&user, path)
		if err != nil {
			return nil, err
		}
	}
	return &user, nil
}

func registerUser(user *User, path string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key error: %w", err)
	}
	user.key = privateKey
	c := lego.NewConfig(user)
	client, err := lego.NewClient(c)
	if err != nil {
		return fmt.Errorf("create lego client error: %w", err)
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return err
	}
	user.Registration = reg
	err = user.Save(path)
	if err != nil {
		return fmt.Errorf("save user error: %w", err)
	}
	return nil
}

func EncodePrivate(privKey *ecdsa.PrivateKey) (string, error) {
	encoded, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return "", err
	}
	pemEncoded := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})
	return string(pemEncoded), nil
}
func (u *User) Save(path string) error {
	err := checkPath(path)
	if err != nil {
		return fmt.Errorf("check path error: %w", err)
	}
	u.KeyEncoded, _ = EncodePrivate(u.key.(*ecdsa.PrivateKey))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	err = json.NewEncoder(f).Encode(u)
	if err != nil {
		return fmt.Errorf("marshal json error: %w", err)
	}
	u.KeyEncoded = ""
	return nil
}

func (u *User) DecodePrivate(pemEncodedPriv string) (*ecdsa.PrivateKey, error) {
	blockPriv, _ := pem.Decode([]byte(pemEncodedPriv))
	if blockPriv == nil {
		return nil, fmt.Errorf("invalid ACME account private key PEM")
	}
	x509EncodedPriv := blockPriv.Bytes
	privateKey, err := x509.ParseECPrivateKey(x509EncodedPriv)
	return privateKey, err
}

func (u *User) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("open file error: %w", err)
	}

	err = json.Unmarshal(data, u)
	if err != nil {
		return fmt.Errorf("unmarshal json error: %w", err)
	}
	u.key, err = u.DecodePrivate(u.KeyEncoded)
	if err != nil {
		return fmt.Errorf("decode private key error: %w", err)
	}
	return nil
}
