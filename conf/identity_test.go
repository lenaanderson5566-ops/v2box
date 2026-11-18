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

func TestOnlyV2BoxIdentity(t *testing.T) {
	entry := NodeConfig{APIHost: "https://panel.example.test", NodeID: 42, Key: "fixture"}
	implicit, err := NodeIdentity(entry)
	if err != nil {
		t.Fatal(err)
	}
	implicitDirectory := NodeCertificateDirectory(entry)
	entry.NodeType = "v2box"
	explicit, err := NodeIdentity(entry)
	if err != nil || implicit != explicit || implicitDirectory != NodeCertificateDirectory(entry) {
		t.Fatal("default backend changes node identity or certificate storage")
	}
	entry.NodeType = "v2node"
	if _, err := NodeIdentity(entry); err == nil {
		t.Fatal("retired backend accepted")
	}
}
