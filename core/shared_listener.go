package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/conf"
	hotnaive "github.com/lenaanderson5566-ops/v2box/internal/inbound/naive"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	sj "github.com/sagernet/sing/common/json"
)

// SharedListeners owns listeners, never panel policy or accounting. Independent
// panel workers may join an explicitly named group without sharing lifecycles.
type SharedListeners struct {
	mu     sync.RWMutex
	groups map[string]*sharedListener
}
type sharedListener struct {
	tag, signature string
	server         *box.Box
	cancel         context.CancelFunc
	members        map[string]*sharedMember
	certificates   *sharedCertificateStore
}
type sharedMember struct {
	tracker *trafficTracker
	state   *nodeState
}
type sharedOwner struct {
	tag     string
	tracker *trafficTracker
}

func NewSharedListeners() *SharedListeners {
	return &SharedListeners{groups: make(map[string]*sharedListener)}
}

func sharedOptions(v *V2Core, info *panel.NodeInfo) (option.Options, string, error) {
	if info == nil || info.Common == nil || !conf.ValidSharedListener(info.SharedListener) || info.SharedListener == "" {
		return option.Options{}, "", fmt.Errorf("SharedListener requires an explicit group and valid node")
	}
	switch info.Type {
	case "anytls", "vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic", "naive":
	default:
		return option.Options{}, "", fmt.Errorf("unsupported shared listener protocol")
	}
	// Certificate ownership and renewal stay with the panel controllers; the
	// listener selects their validated pairs through a live SNI provider.
	if info.Security == panel.Tls {
		if info.Common.CertInfo == nil {
			return option.Options{}, "", fmt.Errorf("shared TLS certificate is required")
		}
		mode := info.Common.CertInfo.CertMode
		if mode != "file" && mode != "remote" && mode != "http" && mode != "dns" {
			return option.Options{}, "", fmt.Errorf("unsupported shared TLS certificate mode")
		}
	}
	o, err := buildOptions(info, nil, v.Config)
	if err != nil {
		return o, "", err
	}
	if info.Security == panel.Tls {
		wrapper, ok := o.Inbounds[0].Options.(option.InboundTLSOptionsWrapper)
		if !ok {
			return o, "", fmt.Errorf("shared protocol lacks TLS options")
		}
		tlsOptions := wrapper.TakeInboundTLSOptions()
		tlsOptions.ServerName = ""
		tlsOptions.Certificate, tlsOptions.Key = nil, nil
		tlsOptions.CertificatePath, tlsOptions.KeyPath = "", ""
		tlsOptions.CertificateProvider = &option.CertificateProviderOptions{Type: sharedCertificateProviderType, Options: &struct{}{}}
	}
	o.Inbounds[0].Tag = ""
	fingerprintOptions := o
	fingerprintOptions.Log = nil
	raw, err := sj.MarshalContext(boxContext(context.Background()), fingerprintOptions)
	if err != nil {
		return o, "", err
	}
	var normalized any
	if err = json.Unmarshal(raw, &normalized); err != nil {
		return o, "", err
	}
	raw, err = json.Marshal(normalized)
	return o, fmt.Sprintf("%x", sha256.Sum256(raw)), err
}

func (h *SharedListeners) validateLocked(v *V2Core, info *panel.NodeInfo, users []panel.UserInfo) (option.Options, string, error) {
	o, signature, err := sharedOptions(v, info)
	if err != nil {
		return o, "", err
	}
	if group := h.groups[info.SharedListener]; group != nil {
		for tag, member := range group.members {
			if tag == info.Tag {
				continue
			}
			if info.Security == panel.Tls && member.state.info.Security == panel.Tls {
				next, previous := info.Common.CertInfo, member.state.info.Common.CertInfo
				sameDomain := certificateDomain(next.CertDomain) == certificateDomain(previous.CertDomain)
				sameSource := sameCertificateSource(next, previous)
				if sameDomain && !sameSource {
					return o, "", fmt.Errorf("shared TLS identity has conflicting certificate sources; use one coordinated certificate binding")
				}
				if !sameDomain && sameSource {
					return o, "", fmt.Errorf("different TLS identities must use independent certificate paths")
				}
				managed := next.CertMode == "http" || next.CertMode == "dns" || previous.CertMode == "http" || previous.CertMode == "dns"
				if sameSource && managed && (next.CertMode != previous.CertMode || next.Provider != previous.Provider || !reflect.DeepEqual(next.DNSEnv, previous.DNSEnv)) {
					return o, "", fmt.Errorf("shared ACME certificate has incompatible renewal owners")
				}
			}
			if group.signature != signature {
				return o, "", fmt.Errorf("shared listener configuration conflict: protocol, listener, TLS/Reality, transport, padding and routing must match")
			}
			passwords := make(map[string]bool, len(member.state.users))
			for _, user := range member.state.users {
				passwords[sharedCredential(info, user)] = true
			}
			for _, user := range users {
				if passwords[sharedCredential(info, user)] {
					return o, "", fmt.Errorf("shared listener credential conflict between panel nodes")
				}
			}
		}
	}
	return o, signature, nil
}
func (h *SharedListeners) validate(v *V2Core, info *panel.NodeInfo, users []panel.UserInfo) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, _, err := h.validateLocked(v, info, users)
	return err
}

// Called with a core's access lock. Group mutation, admission publication and
// authentication swaps are serialized; no user can acquire another panel's tag.
func (h *SharedListeners) setUsers(ctx context.Context, v *V2Core, n *nodeState, users []panel.UserInfo, identities map[string]panel.UserInfo, commit func()) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, signature, err := h.validateLocked(v, n.info, users)
	if err != nil {
		return err
	}
	key, tag := n.info.SharedListener, n.info.Tag
	g := h.groups[key]
	certificateStore := &sharedCertificateStore{}
	if g != nil {
		certificateStore = g.certificates
	}
	infos := []*panel.NodeInfo{n.info}
	if g != nil {
		for memberTag, member := range g.members {
			if memberTag != tag {
				infos = append(infos, member.state.info)
			}
		}
	}
	certificates, err := certificateStore.prepare(infos)
	if err != nil {
		return err
	}
	if g != nil && g.members[tag] != nil && g.members[tag].state != n {
		return fmt.Errorf("shared listener node is still owned by a live controller")
	}
	if g == nil {
		g = &sharedListener{tag: fmt.Sprintf("shared-listener/%x", sha256.Sum256([]byte(key))), signature: signature, members: make(map[string]*sharedMember), certificates: certificateStore}
		o.Inbounds[0].Tag = g.tag
		if err = mergeSharedUsers(v, &o, g, tag, n.info, identities); err != nil {
			return err
		}
		if err = v.tracker.updateUsers(tag, identities, func() error { return nil }); err != nil {
			return err
		}
		certificateStore.current.Store(certificates)
		b, cancel, startErr := v.newBox(ctx, o, certificateStore)
		if startErr != nil {
			_ = v.tracker.updateUsers(tag, n.identities, func() error { return nil })
			return startErr
		}
		g.server, g.cancel = b, cancel
		g.members[tag] = &sharedMember{tracker: v.tracker, state: n}
		h.groups[key] = g
		if commit != nil {
			commit()
		}
	} else {
		if err = mergeSharedUsers(v, &o, g, tag, n.info, identities); err != nil {
			return err
		}
		in, ok := g.server.Inbound().Get(g.tag)
		if !ok {
			return fmt.Errorf("missing shared listener")
		}
		err = v.tracker.updateUsers(tag, identities, func() error {
			if e := updateInboundUsers(in, o.Inbounds[0].Options); e != nil {
				return e
			}
			if commit != nil {
				commit()
			}
			return nil
		})
		if err != nil {
			return err
		}
		g.members[tag] = &sharedMember{tracker: v.tracker, state: n}
		certificateStore.current.Store(certificates)
	}
	n.shared = true
	n.users, n.identities = append([]panel.UserInfo(nil), users...), identities
	return nil
}

// Credential equivalence follows the protocol, including UUID case and SS2022 truncation.
func sharedCredential(info *panel.NodeInfo, user panel.UserInfo) string {
	switch info.Type {
	case "vless", "vmess", "tuic":
		return strings.ToLower(user.Uuid)
	case "shadowsocks":
		if strings.HasPrefix(info.Common.Cipher, "2022-") {
			size := 32
			if info.Common.Cipher == "2022-blake3-aes-128-gcm" {
				size = 16
			}
			if len(user.Uuid) >= size {
				return user.Uuid[:size]
			}
		}
	}
	return user.Uuid
}

// Build each panel table separately: numeric user IDs are only unique within a panel.
func mergeSharedUsers(v *V2Core, options *option.Options, group *sharedListener, excluded string, info *panel.NodeInfo, identities map[string]panel.UserInfo) error {
	appendTable := func(node *panel.NodeInfo, table map[string]panel.UserInfo) error {
		users := make([]panel.UserInfo, 0, len(table))
		for _, user := range table {
			users = append(users, user)
		}
		built, err := buildOptions(node, users, v.Config)
		if err != nil {
			return err
		}
		nameUsers(built.Inbounds[0].Options, table)
		return appendSharedUsers(options.Inbounds[0].Options, built.Inbounds[0].Options)
	}
	for tag, member := range group.members {
		if tag != excluded {
			if err := appendTable(member.state.info, member.state.identities); err != nil {
				return err
			}
		}
	}
	if info != nil {
		return appendTable(info, identities)
	}
	return nil
}
func appendSharedUsers(destination, source any) error {
	switch d := destination.(type) {
	case *option.AnyTLSInboundOptions:
		d.Users = append(d.Users, source.(*option.AnyTLSInboundOptions).Users...)
	case *option.VLESSInboundOptions:
		d.Users = append(d.Users, source.(*option.VLESSInboundOptions).Users...)
	case *option.VMessInboundOptions:
		d.Users = append(d.Users, source.(*option.VMessInboundOptions).Users...)
	case *option.TrojanInboundOptions:
		d.Users = append(d.Users, source.(*option.TrojanInboundOptions).Users...)
	case *option.ShadowsocksInboundOptions:
		d.Users = append(d.Users, source.(*option.ShadowsocksInboundOptions).Users...)
	case *option.Hysteria2InboundOptions:
		d.Users = append(d.Users, source.(*option.Hysteria2InboundOptions).Users...)
	case *option.TUICInboundOptions:
		d.Users = append(d.Users, source.(*option.TUICInboundOptions).Users...)
	case *hotnaive.InboundOptions:
		d.Users = append(d.Users, source.(*hotnaive.InboundOptions).Users...)
	default:
		return fmt.Errorf("unsupported shared user table %T", destination)
	}
	return nil
}
func (h *SharedListeners) owner(inbound, user string) (sharedOwner, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, g := range h.groups {
		if g.tag != inbound {
			continue
		}
		for tag, member := range g.members {
			if _, ok := member.state.identities[user]; ok {
				return sharedOwner{tag: tag, tracker: member.tracker}, true
			}
		}
		break
	}
	return sharedOwner{}, false
}
func (h *SharedListeners) remove(v *V2Core, n *nodeState) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.groups[n.info.SharedListener]
	if g == nil || g.members[n.info.Tag] == nil || g.members[n.info.Tag].state != n {
		return nil
	}
	o, _, err := sharedOptions(v, n.info)
	if err != nil {
		return err
	}
	if err = mergeSharedUsers(v, &o, g, n.info.Tag, nil, nil); err != nil {
		return err
	}
	if in, ok := g.server.Inbound().Get(g.tag); ok {
		err = v.tracker.updateUsers(n.info.Tag, nil, func() error {
			return updateInboundUsers(in, o.Inbounds[0].Options)
		})
	}
	if err != nil {
		return err
	}
	infos := make([]*panel.NodeInfo, 0, len(g.members)-1)
	for tag, member := range g.members {
		if tag != n.info.Tag {
			infos = append(infos, member.state.info)
		}
	}
	certificates, prepareErr := g.certificates.prepare(infos)
	if prepareErr != nil {
		return prepareErr
	}
	g.certificates.current.Store(certificates)
	delete(g.members, n.info.Tag)
	v.tracker.setUsers(n.info.Tag, nil)
	v.tracker.closeNode(n.info.Tag)
	n.shared = false
	if len(g.members) == 0 {
		delete(h.groups, n.info.SharedListener)
		g.cancel()
		if e := g.server.Close(); err == nil {
			err = e
		}
	}
	return err
}
