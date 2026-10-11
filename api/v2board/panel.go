package panel

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/lenaanderson5566-ops/v2box/conf"
)

// Panel is the interface for different panel's api.

type Client struct {
	v10              bool
	landingPage      conf.LandingPage
	client           *resty.Client
	APIHost          string
	Token            string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
}

func New(c *conf.NodeConfig) (*Client, error) {
	nodeType := c.NodeType
	if nodeType == "" {
		nodeType = "v2node"
	} // Preserve existing deployments.
	if nodeType != "v2node" && nodeType != "v2box" {
		return nil, fmt.Errorf("unsupported NodeType %q", nodeType)
	}
	client := resty.New()
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
		client.SetHeader("X-V2Box-Capabilities", "native-routing-v1,dns-v1,outbounds-v1")
	} else {
		client.SetQueryParams(map[string]string{"node_type": nodeType, "node_id": strconv.Itoa(c.NodeID), "token": c.Key})
	}

	return &Client{
		v10:         version == "v10",
		landingPage: c.LandingPage,
		client:      client,
		Token:       c.Key,
		APIHost:     c.APIHost,
		NodeId:      c.NodeID,
		UserList:    &UserListBody{},
		AliveMap:    &AliveMap{},
	}, nil
}
