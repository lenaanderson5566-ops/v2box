package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	box "github.com/sagernet/sing-box"

	"github.com/sagernet/sing-box/option"
)

// Each panel node owns a Box. Authentication updates preserve its listeners
// and unaffected sessions; accounting lives independently of the Box.
type V2Core struct {
	Config     *conf.Conf
	ReloadCh   chan struct{}
	access     sync.Mutex
	nodes      map[string]*nodeState
	tracker    *trafficTracker
	closed     bool
	generation uint64
}
type nodeState struct {
	info       *panel.NodeInfo
	users      []panel.UserInfo
	server     *box.Box
	cancel     context.CancelFunc
	identities map[string]panel.UserInfo
}
type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

func New(c *conf.Conf) *V2Core {
	return &V2Core{Config: c, nodes: make(map[string]*nodeState), tracker: newTrafficTracker()}
}
func (v *V2Core) Start(infos []*panel.NodeInfo) error {
	for _, n := range infos {
		if _, err := buildOptions(n, nil, v.Config); err != nil {
			return fmt.Errorf("node %s: %w", n.Tag, err)
		}
	}
	return nil
}
func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	v.access.Lock()
	defer v.access.Unlock()
	if v.closed {
		return errors.New("core closed")
	}
	if _, ok := v.nodes[tag]; ok {
		return fmt.Errorf("duplicate node %s", tag)
	}
	if _, err := buildOptions(info, nil, v.Config); err != nil {
		return err
	}
	v.nodes[tag] = &nodeState{info: info}
	return nil
}
func (v *V2Core) newBox(options option.Options) (*box.Box, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(boxContext(context.Background()))
	b, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	b.Router().AppendTracker(v.tracker)
	if err = b.Start(); err != nil {
		cancel()
		_ = b.Close()
		return nil, nil, err
	}
	return b, cancel, nil
}
func (v *V2Core) SetUsers(tag string, users []panel.UserInfo) error {
	return v.SetUsersWithCommit(tag, users, nil)
}

// commit updates controller limits only after authentication validation succeeds.
// It runs before new streams are admitted and must not call back into the core.
func (v *V2Core) SetUsersWithCommit(tag string, users []panel.UserInfo, commit func()) error {
	v.access.Lock()
	defer v.access.Unlock()
	n, ok := v.nodes[tag]
	if !ok || v.closed {
		return fmt.Errorf("unknown or closed node %s", tag)
	}
	options, err := buildOptions(n.info, users, v.Config)
	if err != nil {
		return err
	}
	identities := v.identities(n, users)
	nameUsers(options.Inbounds[0].Options, identities)
	if n.server != nil {
		in, found := n.server.Inbound().Get(tag)
		if !found {
			return fmt.Errorf("missing inbound %s", tag)
		}
		if err = v.tracker.updateUsers(tag, identities, func() error {
			if e := updateInboundUsers(in, options.Inbounds[0].Options); e != nil {
				return e
			}
			if commit != nil {
				commit()
			}
			return nil
		}); err != nil {
			return err
		}
		n.users, n.identities = append([]panel.UserInfo(nil), users...), identities
		return nil
	}
	// Keep an empty authentication table, including for Shadowsocks. An empty
	// list never selects the upstream single-user/anonymous inbound variant.
	if err = v.tracker.updateUsers(tag, identities, func() error { return nil }); err != nil {
		return err
	}
	b, cancel, err := v.newBox(options)
	if err != nil {
		_ = v.tracker.updateUsers(tag, n.identities, func() error { return nil })
		return err
	}
	n.server, n.cancel, n.users = b, cancel, append([]panel.UserInfo(nil), users...)
	n.identities = identities
	if commit != nil {
		commit()
	}
	return nil
}
func (v *V2Core) AddUsers(p *AddUsersParams) (int, error) {
	err := v.SetUsers(p.Tag, p.Users)
	if err != nil {
		return 0, err
	}
	return len(p.Users), nil
}
func (v *V2Core) DelNode(tag string) error {
	v.access.Lock()
	defer v.access.Unlock()
	n, ok := v.nodes[tag]
	if !ok {
		return nil
	}
	delete(v.nodes, tag)
	v.revokeNode(tag, n)
	v.tracker.setUsers(tag, nil)
	if n.cancel != nil {
		n.cancel()
	}
	v.tracker.closeNode(tag)
	if n.server != nil {
		n.cancel()
		return n.server.Close()
	}
	return nil
}
func (v *V2Core) Close() error {
	v.access.Lock()
	defer v.access.Unlock()
	v.closed = true
	var errs []error
	for tag, n := range v.nodes {
		v.revokeNode(tag, n)
		v.tracker.setUsers(tag, nil)
		if n.cancel != nil {
			n.cancel()
		}
		v.tracker.closeNode(tag)
		if n.server != nil {
			n.cancel()
			errs = append(errs, n.server.Close())
		}
	}
	v.nodes = make(map[string]*nodeState)
	return errors.Join(errs...)
}

// Also revoke authenticated transport containers with no currently routed
// streams, so node shutdown does not leave idle multiplex sessions behind.
func (v *V2Core) revokeNode(tag string, n *nodeState) {
	if n.server == nil {
		return
	}
	if options, err := buildOptions(n.info, nil, v.Config); err == nil {
		if in, ok := n.server.Inbound().Get(tag); ok {
			_ = updateInboundUsers(in, options.Inbounds[0].Options)
		}
	}
}
func (v *V2Core) GetUserTrafficSlice(tag string, minTraffic int) ([]panel.UserTraffic, error) {
	return v.tracker.takeTraffic(tag, minTraffic), nil
}
func (v *V2Core) RestoreUserTraffic(tag string, traffic []panel.UserTraffic) {
	v.tracker.restoreTraffic(tag, traffic)
}
func sortedUsers(users []panel.UserInfo) []panel.UserInfo {
	result := append([]panel.UserInfo(nil), users...)
	sort.Slice(result, func(i, j int) bool { return result[i].Id < result[j].Id })
	return result
}

func (v *V2Core) TakeOnlineActivity(tag string) map[int]int64 { return v.tracker.takeActivity(tag) }

// PreserveAccountingFrom is used only after old.Close has stopped its nodes.
// Failed final reports for retained node tags can then retry after a reload.
func (v *V2Core) PreserveAccountingFrom(old *V2Core) { v.tracker = old.tracker }
