package conf

import "testing"

func TestPanelIdentityAndCertificateNamespaces(t *testing.T) {
	a := NodeConfig{APIHost: "https://a.example.test", NodeType: "v2box", NodeID: 1, Key: "fixture"}
	b := a
	b.APIHost = "https://b.example.test"
	if NodeCertificateDirectory(a) == NodeCertificateDirectory(b) {
		t.Fatal("panels share default certificates")
	}
	alias := a
	alias.APIHost = "https://A.example.test/"
	first, _ := NodeIdentity(a)
	second, _ := NodeIdentity(alias)
	if first != second || NodeCertificateDirectory(a) != NodeCertificateDirectory(alias) {
		t.Fatal("URL alias changed node ownership")
	}
}
