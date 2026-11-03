package panel

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/lenaanderson5566-ops/v2box/conf"
)

// Panel is the interface for different panel's api.

type Client struct {
	sharedCertificate     *conf.CertificateBinding
	sharedListener        string
	configurationReport   atomic.Pointer[ConfigurationReport]
	targetPolicy          atomic.Pointer[cachedTargetPolicy]
	trafficMu             sync.Mutex
	trafficDrainMu        sync.Mutex
	trafficDedup          atomic.Bool
	budgetSupported       atomic.Bool
	certificateDirectory  string
	nodeType              string
	trafficSpoolDirectory string
	v10                   bool
	landingPage           conf.LandingPage
	client                *resty.Client
	APIHost               string
	Token                 string
	NodeId                int
	nodeEtag              string
	userEtag              string
	responseBodyHash      string
	UserList              *UserListBody
	AliveMap              *AliveMap
}

// Allow large user directories while bounding memory used by unexpected proxy
// responses. Streaming legacy responses enforce the same uncompressed limit.
const maxPanelResponseBytes = 64 * 1024 * 1024

type credentialSafeLogger struct{ key string }

func (l credentialSafeLogger) message(format string, args ...any) string {
	message := fmt.Sprintf(format, args...)
	if l.key != "" {
		message = strings.ReplaceAll(message, url.QueryEscape(l.key), "[REDACTED]")
		message = strings.ReplaceAll(message, l.key, "[REDACTED]")
	}
	return message
}
func (l credentialSafeLogger) Errorf(format string, args ...any) {
	logrus.Error(l.message(format, args...))
}
func (l credentialSafeLogger) Warnf(format string, args ...any) {
	logrus.Warn(l.message(format, args...))
}
func (l credentialSafeLogger) Debugf(format string, args ...any) {
	logrus.Debug(l.message(format, args...))
}

func New(c *conf.NodeConfig) (*Client, error) {
	if c.Timeout < 0 || int64(c.Timeout) > int64((1<<63-1)/time.Second) {
		return nil, errors.New("Timeout must be non-negative and fit a time.Duration in seconds")
	}
	if !conf.ValidSharedListener(c.SharedListener) {
		return nil, errors.New("invalid SharedListener")
	}
	endpoint, err := url.Parse(c.APIHost)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return nil, errors.New("ApiHost must be an HTTPS URL without credentials, query or fragment; update the panel address before upgrading this node")
	}
	nodeType := c.NodeType
	if nodeType == "" {
		nodeType = "v2box"
	} // V2Box is the only supported backend.
	if nodeType != "v2box" {
		return nil, fmt.Errorf("unsupported NodeType %q", nodeType)
	}
	client := resty.New()
	client.SetResponseBodyLimit(maxPanelResponseBytes)
	client.SetLogger(credentialSafeLogger{key: c.Key})
	client.SetRedirectPolicy(resty.NoRedirectPolicy())
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	if retryCount < 0 {
		return nil, errors.New("RetryCount must be non-negative")
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("v2box go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.OnError(func(_ *resty.Request, err error) {
		// The caller logs failures with node context. Strip legacy query-token
		// credentials from transport errors without breaking cancellation checks.
		var networkError *url.Error
		if errors.As(err, &networkError) {
			if endpoint, parseErr := url.Parse(networkError.URL); parseErr == nil {
				endpoint.User, endpoint.RawQuery, endpoint.Fragment = nil, "", ""
				endpoint.ForceQuery = false
				networkError.URL = endpoint.String()
			} else {
				networkError.URL = "[panel]"
			}
		}
	})
	client.SetBaseURL(c.APIHost)
	version := c.APIVersion
	if version == "" {
		if nodeType == "v2box" {
			version = "v10"
		} else {
			version = "legacy"
		}
	}
	if version != "legacy" && version != "v10" {
		return nil, fmt.Errorf("invalid ApiVersion %q", version)
	}
	if version == "v10" && nodeType != "v2box" {
		return nil, fmt.Errorf("V10 requires NodeType v2box")
	}
	if version == "v10" {
		client.SetAuthToken(c.Key)
		client.SetHeader("X-Node-Type", nodeType)
		client.SetHeader("X-Node-ID", strconv.Itoa(c.NodeID))
		client.SetHeader("X-Node-Capabilities", "native-routing-v1,dns-v1,outbounds-v1,server-traffic-budget-v1,target-statistics-v1,target-statistics-policy-v2,target-statistics-interval-v1,configuration-receipt-v1,certificate-transition-v1")
	} else {
		client.SetQueryParams(map[string]string{"node_type": nodeType, "node_id": strconv.Itoa(c.NodeID)})
		if strings.HasPrefix(c.Key, "nk_") {
			client.SetAuthToken(c.Key)
		} else {
			client.SetQueryParam("token", c.Key)
		}
	}

	return &Client{
		v10:                   version == "v10",
		sharedListener:        c.SharedListener,
		sharedCertificate:     c.SharedCertificate,
		certificateDirectory:  c.CertificateDirectory,
		nodeType:              nodeType,
		trafficSpoolDirectory: c.TrafficSpoolDirectory,
		landingPage:           c.LandingPage,
		client:                client,
		Token:                 c.Key,
		APIHost:               c.APIHost,
		NodeId:                c.NodeID,
		UserList:              &UserListBody{},
		AliveMap:              &AliveMap{},
	}, nil
}

// CloseIdleConnections releases pooled sockets without making the client unusable
// when an existing controller must be restored after a rejected reload.
func (c *Client) CloseIdleConnections() { c.client.GetClient().CloseIdleConnections() }
