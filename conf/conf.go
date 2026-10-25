package conf

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig   LogConfig    `mapstructure:"Log"`
	NodeConfigs []NodeConfig `mapstructure:"Nodes"`
	PprofPort   int          `mapstructure:"PprofPort"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
	Access string `mapstructure:"Access"`
}

type NodeConfig struct {
	TrafficSpoolDirectory string      `mapstructure:"TrafficSpoolDirectory"`
	APIVersion            string      `mapstructure:"ApiVersion"`
	LandingPage           LandingPage `mapstructure:"LandingPage"`
	NodeType              string      `mapstructure:"NodeType"`
	APIHost               string      `mapstructure:"ApiHost"`
	NodeID                int         `mapstructure:"NodeID"`
	Key                   string      `mapstructure:"ApiKey"`
	Timeout               int         `mapstructure:"Timeout"`
	RetryCount            *int        `mapstructure:"RetryCount"`
}

// LandingPage is local to the node; paths are never supplied by panel responses.
type LandingPage struct {
	Enabled   bool   `mapstructure:"Enabled" json:"enabled"`
	Directory string `mapstructure:"Directory" json:"directory"`
}

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
