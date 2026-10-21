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
	targetPolicy          atomic.Pointer[cachedTargetPolicy]
	trafficMu             sync.Mutex
	trafficDrainMu        sync.Mutex
	trafficDedup          atomic.Bool
	budgetSupported       atomic.Bool
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

func New(c *conf.NodeConfig) (*Client, error) {
	endpoint, err := url.Parse(c.APIHost)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return nil, errors.New("ApiHost must be an HTTPS URL without credentials, query or fragment; update the panel address before upgrading this node")
	}
	nodeType := c.NodeType
	if nodeType == "" {
		nodeType = "v2node"
	} // Preserve existing deployments.
	if nodeType != "v2node" && nodeType != "v2box" {
		return nil, fmt.Errorf("unsupported NodeType %q", nodeType)
	}
	client := resty.New()
	client.SetRedirectPolicy(resty.NoRedirectPolicy())
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("v2box go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			logrus.Error(v.Err)
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
		client.SetHeader("X-Node-Capabilities", "native-routing-v1,dns-v1,outbounds-v1,server-traffic-budget-v1,target-statistics-v1,target-statistics-policy-v2,target-statistics-interval-v1")
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
