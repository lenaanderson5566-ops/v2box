package node

import (
	"context"
	"fmt"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	log "github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
}

func New(nodes []conf.NodeConfig) (*Node, error) {
	n := &Node{
		controllers: make([]*Controller, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(nodes)),
	}
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, err
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			return nil, err
		}
		n.controllers[i] = NewController(p, &node, info)
		n.NodeInfos[i] = info
	}
	return n, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) Close() error {
	var err error
	for _, c := range n.controllers {
		if err = c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			return err
		}
	}
	n.controllers = nil
	return nil
}

// PrepareCertificates runs before live nodes are stopped during a reload.
func (n *Node) PrepareCertificates() error {
	for _, c := range n.controllers {
		if c.info.Security == panel.Tls {
			if err := c.requestCert(); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrepareUsers validates panel authentication data while old nodes are still serving.
func (n *Node) PrepareUsers(server *core.V2Core) error {
	for _, c := range n.controllers {
		if err := c.prepareUsers(); err != nil {
			return err
		}
		if err := server.ValidateNodeUsers(c.info, c.userList); err != nil {
			return err
		}
	}
	return nil
}

// ReportConfiguration runs only after all controllers started, or on a failed
// preparation stage. A shared deadline bounds the cost across multiple nodes.
func (n *Node) ReportConfiguration(code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, c := range n.controllers {
		if c != nil && c.info != nil {
			c.apiClient.ReportConfiguration(ctx, c.info.ConfigurationHash, code)
		}
	}
}
