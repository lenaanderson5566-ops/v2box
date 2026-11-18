package core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/log"
)

const sharedCertificateProviderType = "v2box-shared"

type sharedCertificateContextKey struct{}
type sharedCertificateStore struct {
	current atomic.Pointer[sharedCertificateSet]
}
type sharedCertificateSet struct {
	domains map[string]*sharedCertificateSource
	names   []string
}
type sharedCertificateSource struct {
	info              panel.CertInfo
	mu                sync.Mutex
	last              *tls.Certificate
	certTime, keyTime time.Time
	certSize, keySize int64
}

type sharedCertificateProvider struct {
	certificate.Adapter
	store *sharedCertificateStore
}

func (p *sharedCertificateProvider) Start(adapter.StartStage, *adapter.Scope) error { return nil }
func (p *sharedCertificateProvider) Close() error                                   { return nil }
func (p *sharedCertificateProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	set := p.store.current.Load()
	if set == nil {
		return nil, fmt.Errorf("shared TLS certificates are not ready")
	}
	name := certificateDomain(hello.ServerName)
	if source := set.domains[name]; source != nil {
		return source.load()
	}
	if name == "" && len(set.names) == 1 {
		return set.domains[set.names[0]].load()
	}
	for _, domain := range set.names {
		if strings.HasPrefix(domain, "*.") && strings.HasSuffix(name, domain[1:]) && strings.Count(name, ".") == strings.Count(domain, ".") {
			return set.domains[domain].load()
		}
	}
	return nil, fmt.Errorf("no shared TLS certificate for requested SNI")
}
func registerSharedCertificateProvider(registry *certificate.Registry) {
	certificate.Register[struct{}](registry, sharedCertificateProviderType,
		func(ctx context.Context, _ log.ContextLogger, tag string, _ struct{}) (adapter.CertificateProviderService, error) {
			store, ok := ctx.Value(sharedCertificateContextKey{}).(*sharedCertificateStore)
			if !ok {
				return nil, fmt.Errorf("missing shared TLS certificate store")
			}
			return &sharedCertificateProvider{Adapter: certificate.NewAdapter(sharedCertificateProviderType, tag), store: store}, nil
		})
}
func certificateDomain(value string) string { return strings.ToLower(strings.TrimSuffix(value, ".")) }
func sameCertificateSource(a, b *panel.CertInfo) bool {
	if a.CertMode == "remote" || b.CertMode == "remote" {
		return a.CertMode == b.CertMode && a.TlsCert == b.TlsCert && a.TlsKey == b.TlsKey
	}
	return a.CertFile == b.CertFile && a.KeyFile == b.KeyFile
}

// Prepare a complete SNI table before publishing it. Reuse source caches so a
// partially replaced external pair cannot evict a working certificate.
func (s *sharedCertificateStore) prepare(infos []*panel.NodeInfo) (*sharedCertificateSet, error) {
	set := &sharedCertificateSet{domains: make(map[string]*sharedCertificateSource)}
	previous := s.current.Load()
	for _, info := range infos {
		if info.Security != panel.Tls {
			continue
		}
		cert := info.Common.CertInfo
		name := certificateDomain(cert.CertDomain)
		if name == "" {
			return nil, fmt.Errorf("shared TLS requires a certificate domain")
		}
		if other := set.domains[name]; other != nil {
			if !sameCertificateSource(&other.info, cert) {
				return nil, fmt.Errorf("shared TLS identity has conflicting certificate sources")
			}
			continue
		}
		source := &sharedCertificateSource{info: *cert}
		reused := false
		if previous != nil {
			if old := previous.domains[name]; old != nil && sameCertificateSource(&old.info, cert) {
				source = old
				reused = true
			}
		}
		if !reused {
			if _, err := source.load(); err != nil {
				return nil, err
			}
		}
		set.domains[name] = source
		set.names = append(set.names, name)
	}
	sort.Strings(set.names)
	return set, nil
}
func (s *sharedCertificateSource) load() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	certPEM, keyPEM := []byte(s.info.TlsCert), []byte(s.info.TlsKey)
	var certStat, keyStat os.FileInfo
	var err error
	if s.info.CertMode != "remote" {
		certStat, err = os.Stat(s.info.CertFile)
		if err == nil {
			keyStat, err = os.Stat(s.info.KeyFile)
		}
		if err == nil && s.last != nil && certStat.ModTime() == s.certTime && keyStat.ModTime() == s.keyTime && certStat.Size() == s.certSize && keyStat.Size() == s.keySize {
			return s.validLast()
		}
		if err == nil {
			certPEM, err = os.ReadFile(s.info.CertFile)
		}
		if err == nil {
			keyPEM, err = os.ReadFile(s.info.KeyFile)
		}
	} else if s.last != nil {
		return s.validLast()
	}
	var pair tls.Certificate
	if err == nil {
		pair, err = tls.X509KeyPair(certPEM, keyPEM)
	}
	if err == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	}
	if err == nil {
		err = pair.Leaf.VerifyHostname(s.info.CertDomain)
	}
	if err == nil && (time.Now().Before(pair.Leaf.NotBefore) || !time.Now().Before(pair.Leaf.NotAfter)) {
		err = fmt.Errorf("shared TLS certificate is not currently valid")
	}
	if err != nil {
		if s.last != nil {
			return s.validLast()
		}
		return nil, fmt.Errorf("shared TLS certificate/key pair is unavailable or invalid")
	}
	s.last = &pair
	if certStat != nil {
		s.certTime, s.keyTime = certStat.ModTime(), keyStat.ModTime()
		s.certSize, s.keySize = certStat.Size(), keyStat.Size()
	}
	return &pair, nil
}
func (s *sharedCertificateSource) validLast() (*tls.Certificate, error) {
	if s.last != nil && !time.Now().Before(s.last.Leaf.NotBefore) && time.Now().Before(s.last.Leaf.NotAfter) {
		return s.last, nil
	}
	return nil, fmt.Errorf("shared TLS certificate has expired")
}
