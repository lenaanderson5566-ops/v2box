# sing-box user hot-update adapters

These packages are maintained source adaptations, not an unmodified upstream
binary. They retain the original GPL-3.0-or-later terms. Source snapshots:

| Directory | Upstream |
| --- | --- |
| inbound, transport/trojan | SagerNet/sing-box v1.14.3 |
| transport/vmess | SagerNet/sing-vmess 9b95ab8c9478 (go.mod pinned pseudo-version) |
| transport/quic | SagerNet/sing-quic 75c3ac4fa12b (go.mod pinned pseudo-version), TUIC and Hysteria2 packages |
| transport/anytls | anytls/sing-anytls v0.0.11 |

License texts and upstream copyright notices are retained in `licenses/`.
Local changes, 2026-10-10–2026-10-11:

- Inbounds publish stable string authentication identities rather than array
  indices, and expose typed user updates without replacing their listeners.
- Authentication maps are built before publication. Short mutex-protected
  snapshots permit concurrent handshakes without holding locks over network I/O.
- Shadowsocks uses the upstream concurrent multi-user service even for an empty
  list; it never falls back to a single-user listener.
- Naive adapts sing-box v1.14.3 inbound/padding code for HTTPS/HTTP2 only.
  Each authenticated HTTP2 transport is bound to one opaque account generation;
  atomic credential replacement revokes active and idle transports. Request
  identities are separated from the public username/password, empty tables deny
  all requests, and forwarded IP headers are ignored. Hijacked HTTP2 transports
  remain tracked until their handler exits and are explicitly closed on shutdown.
  Padding readers retain framing until the final payload and padding are drained.
  Idle transport timers are owned locally and stopped after handler teardown,
  avoiding Go 1.27 HTTP2 shutdown rearming a timer on a closed connection.
  Opt-in local landing pages handle only GET/HEAD; CONNECT authentication is
  unchanged. Hysteria2 file masquerade shares the confined static file handler;
  string masquerade writes headers before the status and omits HEAD bodies.
- AnyTLS stream closure errors are published atomically and close hooks support
  installation racing with remote closure. Its outbound adapter
  uses the same patched transport, so concurrent remote closure is safe on both
  sides. Other outbounds remain supplied by official sing-box.
  Client idle cleanup uses a cancellable ticker and exits before Close returns.
- Local transport imports are rewritten to keep the patched server services
  together. Protocol framing and cryptography are inherited from upstream.

The outer core assigns an authentication generation to each UID/credential pair.
Removing or replacing an account invalidates its generation, closes its tracked
TCP/UDP streams, and rejects new streams on old authenticated multiplex sessions.
Unchanged identities retain their connections and accounting. `authconn` also
tracks the underlying authenticated TCP/QUIC transport and schedules closure
when its identity is revoked, including idle transports with no routed streams.
Transport closure runs outside the authentication publication barrier to avoid
blocking updates on protocol callbacks. Failed validation does not publish a table.

When upgrading sing-box or its transport dependencies, compare these snapshots
with upstream and reapply the concurrency/identity changes. Run the full race
suite and the separate Reality/Vision interoperability test before release.
