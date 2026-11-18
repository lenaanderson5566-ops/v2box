package core

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
)

func domainCertificate(t *testing.T, domain, mode string) *panel.CertInfo {
	t.Helper()
	info := testCertificate(t)
	block, _ := pem.Decode([]byte(info.TlsCert))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode([]byte(info.TlsKey))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leaf.DNSNames, leaf.Subject.CommonName = []string{domain}, domain
	leaf.SerialNumber, _ = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	info.TlsCert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	info.CertDomain, info.CertMode = domain, mode
	dir := t.TempDir()
	info.CertFile, info.KeyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(info.CertFile, []byte(info.TlsCert), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(info.KeyFile, []byte(info.TlsKey), 0600); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestSharedSNIAutomaticCertificateReload(t *testing.T) {
	limiter.Init()
	for _, protocol := range []string{"vless", "vmess", "trojan", "anytls", "hysteria2", "tuic", "naive"} {
		t.Run(protocol, func(t *testing.T) {
			a := testNode(t, protocol)
			a.Tag = "acme-panel-a"
			a.SharedListener = "automatic"
			a.Security = panel.Tls
			a.Common.CertInfo = domainCertificate(t, "a.example.test", "http")
			b := *a
			common := *a.Common
			b.Common = &common
			b.Tag = "acme-panel-b"
			b.Common.CertInfo = domainCertificate(t, "b.example.test", "dns")
			hub := NewSharedListeners()
			va, vb := New(testConf()), New(testConf())
			va.UseSharedListeners(hub)
			vb.UseSharedListeners(hub)
			usersA := []panel.UserInfo{{Id: 42, Uuid: testUUID}}
			usersB := []panel.UserInfo{{Id: 42, Uuid: "a8389d79-d204-4f90-b8f0-cb4878a14933"}}
			for _, item := range []struct {
				v     *V2Core
				n     *panel.NodeInfo
				users []panel.UserInfo
			}{{va, a, usersA}, {vb, &b, usersB}} {
				limiter.AddLimiter(protocol, item.n.Tag, item.users, nil)
				t.Cleanup(func() { item.v.Close(); limiter.DeleteLimiter(item.n.Tag) })
				if err := item.v.AddNode(item.n.Tag, item.n); err != nil {
					t.Fatal(err)
				}
				if err := item.v.SetUsers(item.n.Tag, item.users); err != nil {
					t.Fatal(err)
				}
			}
			echo := echoServer(t)
			exchangeNode := func(n *panel.NodeInfo, credential string) {
				t.Helper()
				if protocol == "naive" {
					stream, err := naiveConnect(naiveClient(t, n), credential, echo.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					defer stream.Close()
					if err := naiveEcho(stream, []byte("separate-domain")); err != nil {
						t.Fatal(err)
					}
				} else {
					exchange(t, clientBox(t, n, credential), echo.Addr())
				}
			}
			exchangeNode(a, testUUID)
			exchangeNode(&b, usersB[0].Uuid)
			var checkExisting, checkExistingB func()
			if protocol == "naive" {
				sa, err := naiveConnect(naiveClient(t, a), testUUID, echo.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer sa.Close()
				sb, err := naiveConnect(naiveClient(t, &b), usersB[0].Uuid, echo.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer sb.Close()
				checkExistingB = func() {
					if err := naiveEcho(sb, []byte("keep-existing")); err != nil {
						t.Fatal(err)
					}
				}
				checkExisting = func() {
					if err := naiveEcho(sa, []byte("keep-existing")); err != nil {
						t.Fatal(err)
					}
					checkExistingB()
				}
			} else {
				sa := persistent(t, clientBox(t, a, testUUID), echo.Addr())
				defer sa.Close()
				sb := persistent(t, clientBox(t, &b, usersB[0].Uuid), echo.Addr())
				defer sb.Close()
				checkExistingB = func() {
					if err := roundTrip(sb); err != nil {
						t.Fatal(err)
					}
				}
				checkExisting = func() {
					if err := roundTrip(sa); err != nil {
						t.Fatal(err)
					}
					checkExistingB()
				}
			}
			checkExisting()
			awaitTraffic(t, va, a.Tag, 1)
			awaitTraffic(t, vb, b.Tag, 1)
			// The key is replaced first. An incomplete new pair must not evict
			// the last valid B certificate, and A must remain independently live.
			next := domainCertificate(t, b.Common.CertInfo.CertDomain, "dns")
			if err := os.WriteFile(b.Common.CertInfo.KeyFile, []byte(next.TlsKey), 0600); err != nil {
				t.Fatal(err)
			}
			checkExisting()
			exchangeNode(&b, usersB[0].Uuid)
			exchangeNode(a, testUUID)
			if err := os.WriteFile(b.Common.CertInfo.CertFile, []byte(next.TlsCert), 0600); err != nil {
				t.Fatal(err)
			}
			newB := b
			newCommon := *b.Common
			newB.Common = &newCommon
			newCert := *b.Common.CertInfo
			newB.Common.CertInfo = &newCert
			newCert.TlsCert = next.TlsCert
			// This fresh client trusts only the new self-signed fixture, proving
			// the listener selected the renewed pair without a service restart.
			checkExisting()
			exchangeNode(&newB, usersB[0].Uuid)
			exchangeNode(a, testUUID)
			awaitTraffic(t, va, a.Tag, 1)
			awaitTraffic(t, vb, b.Tag, 1)
			exchangeNode(&newB, usersB[0].Uuid)
			awaitTraffic(t, vb, b.Tag, 1)
			if got, _ := va.GetUserTrafficSlice(a.Tag, 0); len(got) != 0 {
				t.Fatal("SNI peer traffic crossed panels")
			}
			if err := va.Close(); err != nil {
				t.Fatal(err)
			}
			checkExistingB()
			exchangeNode(&newB, usersB[0].Uuid)
		})
	}
}

// Retained certificates must not require a new key merely to join a group.
func TestSharedCertificateBindingPreservesPrivateKey(t *testing.T) {
	first := domainCertificate(t, "same.example.test", "http")
	second := *first
	a := &panel.NodeInfo{Security: panel.Tls, Common: &panel.CommonNode{CertInfo: first}}
	b := &panel.NodeInfo{Security: panel.Tls, Common: &panel.CommonNode{CertInfo: &second}}
	store := &sharedCertificateStore{}
	before, _ := os.ReadFile(first.KeyFile)
	set, err := store.prepare([]*panel.NodeInfo{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.domains) != 1 {
		t.Fatal("same-domain pair was duplicated")
	}
	after, _ := os.ReadFile(first.KeyFile)
	if string(before) != string(after) {
		t.Fatal("shared binding changed private key")
	}
}
