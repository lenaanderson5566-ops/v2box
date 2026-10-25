package node

import (
	"context"
	"errors"
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
	complete := false
	defer func() {
		if !complete {
			_ = n.Close()
		}
	}()
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, err
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			p.CloseIdleConnections()
			return nil, err
		}
		n.controllers[i] = NewController(p, &node, info)
		n.NodeInfos[i] = info
	}
	complete = true
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
		if c == nil {
			continue
		}
		if closeErr := c.Close(); closeErr != nil {
			log.Errorf("close controller failed: %v", closeErr)
			err = errors.Join(err, closeErr)
		}
	}
	n.controllers = nil
	return err
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
func (n *Node) ReportConfiguration(code string) error {
	var transitionError error
	deadline := 3 * time.Second
	for _, c := range n.controllers {
		if c != nil && c.info != nil && c.info.Common.DomainTransition != nil {
			deadline = 12 * time.Second
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	for _, c := range n.controllers {
		if c != nil && c.info != nil {
			id := ""
			if c.info.Common.DomainTransition != nil {
				id = c.info.Common.DomainTransition.ID
			}
			if err := c.apiClient.ReportConfiguration(ctx, c.info.ConfigurationHash, code, c.info.CertificateFingerprint, id); err != nil && id != "" {
				transitionError = errors.Join(transitionError, err)
			}
		}
	}
	return transitionError
}

// Snapshot retains validated authentication and certificate state for local rollback.
// It is used only after all controller tasks have been stopped.
func (n *Node) Snapshot() *Node { copy := *n; return &copy }
func (n *Node) Restore(server *core.V2Core) error {
	for _, c := range n.controllers {
		if err := c.Start(server); err != nil {
			return err
		}
	}
	return nil
}

// ConfirmPublication also absorbs a reload signal queued by the previous polling
// controller while certificate preparation was running.
func (n *Node) ConfirmPublication(next *Node) bool {
	if len(n.NodeInfos) == 0 || len(n.NodeInfos) != len(next.NodeInfos) {
		return false
	}
	for i, info := range n.NodeInfos {
		if !completedTransitionOnly(info, next.NodeInfos[i]) {
			return false
		}
	}
	for i, info := range n.NodeInfos {
		next.NodeInfos[i].CertificateFingerprint = info.CertificateFingerprint
	}
	_ = next.ReportConfiguration("")
	return true
}
