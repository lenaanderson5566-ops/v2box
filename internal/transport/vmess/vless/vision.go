package vless

import upstream "github.com/sagernet/sing-vmess/vless"

// Keep Vision framing and TLS integration on the pinned upstream implementation.
// Local changes are limited to authentication publication and stable identities.
var NewVisionConn = upstream.NewVisionConn
