package node

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	return NewContext(context.Background(), nodes)
}

func NewContext(ctx context.Context, nodes []conf.NodeConfig) (*Node, error) {
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
		info, err := p.GetNodeInfo(ctx)
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
	return n.StartContext(context.Background(), nodes, core)
}

func (n *Node) StartContext(ctx context.Context, nodes []conf.NodeConfig, core *core.V2Core) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, node := range nodes {
		err := n.controllers[i].StartContext(ctx, core)
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
	// Cancel every controller before joining callbacks or flushing traffic.
	for _, c := range n.controllers {
		if c != nil {
			c.cancelTasks()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var err error
	for _, c := range n.controllers {
		if c == nil {
			continue
		}
		if closeErr := c.closeContext(ctx); closeErr != nil {
			log.Errorf("close controller failed: %v", closeErr)
			err = errors.Join(err, closeErr)
		}
	}
	n.controllers = nil
	return err
}

// PrepareCertificates runs before live nodes are stopped during a reload.
func (n *Node) PrepareCertificates() error {
	return n.PrepareCertificatesContext(context.Background())
}

func (n *Node) PrepareCertificatesContext(ctx context.Context) error {
	for _, c := range n.controllers {
		if c.info.Security == panel.Tls {
			if err := c.requestCertContext(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrepareUsers validates panel authentication data while old nodes are still serving.
func (n *Node) PrepareUsers(server *core.V2Core) error {
	return n.PrepareUsersContext(context.Background(), server)
}

func (n *Node) PrepareUsersContext(ctx context.Context, server *core.V2Core) error {
	for _, c := range n.controllers {
		if err := c.prepareUsersContext(ctx); err != nil {
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
	return n.ReportConfigurationContext(context.Background(), code)
}

func (n *Node) ReportConfigurationContext(parent context.Context, code string) error {
	var transitionError error
	deadline := 3 * time.Second
	for _, c := range n.controllers {
		if c != nil && c.info != nil && c.info.Common.DomainTransition != nil {
			deadline = 12 * time.Second
			break
		}
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
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
	return errors.Join(transitionError, parent.Err())
}

// Snapshot retains validated authentication and certificate state for local rollback.
// It is used only after all controller tasks have been stopped.
func (n *Node) Snapshot() *Node { copy := *n; return &copy }
func (n *Node) Restore(server *core.V2Core) error {
	return n.RestoreContext(context.Background(), server)
}

func (n *Node) RestoreContext(ctx context.Context, server *core.V2Core) error {
	for _, c := range n.controllers {
		if err := c.StartContext(ctx, server); err != nil {
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

// Unchanged ignores the local certificate receipt, which is not panel config.
func (n *Node) Unchanged(next *Node) bool {
	if len(n.NodeInfos) != len(next.NodeInfos) {
		return false
	}
	for i, current := range n.NodeInfos {
		old, candidate := *current, *next.NodeInfos[i]
		old.CertificateFingerprint, candidate.CertificateFingerprint = "", ""
		if !reflect.DeepEqual(old, candidate) {
			return false
		}
	}
	return true
}
