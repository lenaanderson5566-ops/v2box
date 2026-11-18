package conf

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	OutboundAllowCIDRs []string     `mapstructure:"OutboundAllowCIDRs"`
	LogConfig          LogConfig    `mapstructure:"Log"`
	NodeConfigs        []NodeConfig `mapstructure:"Nodes"`
	PprofPort          int          `mapstructure:"PprofPort"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
	Access string `mapstructure:"Access"`
}

// CertificateBinding reuses an existing ACME pair without changing the panel's
// renewal mode. Domain pinning prevents a later panel edit repurposing its paths.
type CertificateBinding struct {
	LegacyCertFile string `mapstructure:"LegacyCertFile"`
	LegacyKeyFile  string `mapstructure:"LegacyKeyFile"`
	Domain         string `mapstructure:"Domain"`
	CertFile       string `mapstructure:"CertFile"`
	KeyFile        string `mapstructure:"KeyFile"`
}
type NodeConfig struct {
	SharedCertificate     *CertificateBinding `mapstructure:"SharedCertificate"`
	SharedListener        string              `mapstructure:"SharedListener"`
	CertificateDirectory  string              `mapstructure:"CertificateDirectory"`
	TrafficSpoolDirectory string              `mapstructure:"TrafficSpoolDirectory"`
	APIVersion            string              `mapstructure:"ApiVersion"`
	LandingPage           LandingPage         `mapstructure:"LandingPage"`
	NodeType              string              `mapstructure:"NodeType"`
	APIHost               string              `mapstructure:"ApiHost"`
	NodeID                int                 `mapstructure:"NodeID"`
	Key                   string              `mapstructure:"ApiKey"`
	Timeout               int                 `mapstructure:"Timeout"`
	RetryCount            *int                `mapstructure:"RetryCount"`
}

// LandingPage is local to the node; paths are never supplied by panel responses.
type LandingPage struct {
	Enabled   bool   `mapstructure:"Enabled" json:"enabled"`
	Directory string `mapstructure:"Directory" json:"directory"`
}

var sharedListenerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func ValidSharedListener(name string) bool { return name == "" || sharedListenerName.MatchString(name) }

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
			Access: "none",
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	if len(p.NodeConfigs) == 0 {
		return fmt.Errorf("at least one node is required")
	}
	for i := range p.NodeConfigs {
		node := p.NodeConfigs[i]
		if !ValidSharedListener(node.SharedListener) {
			return fmt.Errorf("invalid SharedListener for node %d", i+1)
		}
		host, err := url.Parse(node.APIHost)
		if err != nil || host.Hostname() == "" || (host.Scheme != "https" && host.Scheme != "http") || host.User != nil || host.RawQuery != "" || host.Fragment != "" || strings.EqualFold(strings.TrimSuffix(host.Hostname(), "."), "panel.example.com") || node.NodeID <= 0 || strings.TrimSpace(node.Key) == "" || node.Key == "REPLACE_ME" {
			return fmt.Errorf("node %d has incomplete or example enrollment configuration; resume deployment from the panel before starting V2Box", i+1)
		}
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	return nil
}

func intPtr(v int) *int {
	return &v
}

// Canonical panel origins keep aliases from creating duplicate runtime owners.
func CanonicalPanelURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("ApiHost must be an HTTPS URL without credentials, query or fragment")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}
func NodeIdentity(entry NodeConfig) (string, error) {
	host, err := CanonicalPanelURL(entry.APIHost)
	if err != nil {
		return "", err
	}
	kind := entry.NodeType
	if kind == "" {
		kind = "v2box"
	}
	if (kind != "v2box") || entry.NodeID <= 0 || strings.TrimSpace(entry.Key) == "" {
		return "", fmt.Errorf("invalid node identity")
	}
	return fmt.Sprintf("%s|%s|%d", host, kind, entry.NodeID), nil
}
func NodeCertificateDirectory(entry NodeConfig) string {
	host, _ := CanonicalPanelURL(entry.APIHost)
	digest := sha256.Sum256([]byte(host))
	kind := entry.NodeType
	if kind == "" {
		kind = "v2box"
	}
	return filepath.Join("/etc/v2box/panels", fmt.Sprintf("%x", digest[:8]), fmt.Sprintf("%s-%d", kind, entry.NodeID))
}
