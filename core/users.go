package core

import (
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	hotnaive "github.com/lenaanderson5566-ops/v2box/internal/inbound/naive"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
)

// The authentication identity includes a generation. A revoked multiplex/QUIC
// session must not regain access if the same UUID is subsequently re-added.
func (v *V2Core) identities(n *nodeState, users []panel.UserInfo) map[string]panel.UserInfo {
	result := make(map[string]panel.UserInfo, len(users))
	previousNames := make(map[string]string, len(n.identities))
	for name, old := range n.identities {
		previousNames[fmt.Sprintf("%s/%d", old.Uuid, old.Id)] = name
	}
	for _, u := range users {
		name := previousNames[fmt.Sprintf("%s/%d", u.Uuid, u.Id)]
		if name == "" {
			v.generation++
			name = fmt.Sprintf("%s/%d/%d", u.Uuid, u.Id, v.generation)
		}
		result[name] = u
	}
	return result
}

// Validate/build options first, then assign the opaque authenticated identity.
func nameUsers(options any, identities map[string]panel.UserInfo) {
	names := make(map[string]string, len(identities))
	for name, u := range identities {
		names[u.Uuid] = name
	}
	switch o := options.(type) {
	case *hotnaive.InboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.VMessInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.VLESSInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.TrojanInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.ShadowsocksInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.Hysteria2InboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.TUICInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	case *option.AnyTLSInboundOptions:
		for i := range o.Users {
			o.Users[i].Name = names[o.Users[i].Name]
		}
	}
}

func updateInboundUsers(in adapter.Inbound, options any) error {
	switch o := options.(type) {
	case *hotnaive.InboundOptions:
		return in.(interface{ UpdateUsers([]hotnaive.User) error }).UpdateUsers(o.Users)
	case *option.VMessInboundOptions:
		return in.(interface {
			UpdateUsers([]option.VMessUser) error
		}).UpdateUsers(o.Users)
	case *option.VLESSInboundOptions:
		return in.(interface {
			UpdateUsers([]option.VLESSUser) error
		}).UpdateUsers(o.Users)
	case *option.TrojanInboundOptions:
		return in.(interface {
			UpdateUsers([]option.TrojanUser) error
		}).UpdateUsers(o.Users)
	case *option.ShadowsocksInboundOptions:
		return in.(interface {
			UpdateUsers([]option.ShadowsocksUser) error
		}).UpdateUsers(o.Users)
	case *option.Hysteria2InboundOptions:
		return in.(interface {
			UpdateUsers([]option.Hysteria2User) error
		}).UpdateUsers(o.Users)
	case *option.TUICInboundOptions:
		return in.(interface{ UpdateUsers([]option.TUICUser) error }).UpdateUsers(o.Users)
	case *option.AnyTLSInboundOptions:
		return in.(interface {
			UpdateUsers([]option.AnyTLSUser) error
		}).UpdateUsers(o.Users)
	default:
		return fmt.Errorf("unsupported user update %T", options)
	}
}
