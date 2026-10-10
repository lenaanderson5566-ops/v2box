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
Local changes, 2026-10-10:

- Inbounds publish stable string authentication identities rather than array
  indices, and expose typed user updates without replacing their listeners.
- Authentication maps are built before publication. Short mutex-protected
  snapshots permit concurrent handshakes without holding locks over network I/O.
- Shadowsocks uses the upstream concurrent multi-user service even for an empty
  list; it never falls back to a single-user listener.
- AnyTLS stream closure errors are published atomically. Its outbound adapter
  uses the same patched transport, so concurrent remote closure is safe on both
  sides. Other outbounds remain supplied by official sing-box.
- Local transport imports are rewritten to keep the patched server services
  together. Protocol framing and cryptography are inherited from upstream.

The outer core assigns an authentication generation to each UID/credential pair.
Removing or replacing an account invalidates its generation, closes its tracked
TCP/UDP streams, and rejects new streams on old authenticated multiplex sessions.
Unchanged identities retain their connections and accounting. QUIC/multiplex
transport containers may remain until their normal idle timeout, but revoked
identities cannot forward traffic. Failed validation does not publish a table.

When upgrading sing-box or its transport dependencies, compare these snapshots
with upstream and reapply the concurrency/identity changes. Run the full race
suite and the separate Reality/Vision interoperability test before release.
