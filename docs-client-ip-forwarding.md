# Validated client IP forwarding to applications

App HTTP requests and WebSocket handshakes receive an SDK v0.98.0 signed client
IP assertion, bound to the final method and destination URI with a one-minute
expiry. The gateway resolves the client before its reverse proxy appends the
internal hop, then signs after all URL rewrites. The destination installation
uses APTEVA_APP_TOKEN to verify it. This does not modify user authorization.

All application proxy entry points use the same helper: ordinary app routes,
bound app callbacks, environment app gateways, managed environment installs,
host routes owned by the destination app installation, and runtime attachments.
Legacy tokenless app and ordinary non-app targets have assertions scrubbed.
Host signing is independent of optional bearer replacement and requires matching
route ownership. Caller-supplied signed and unsigned assertions are removed.

Configure the existing APTEVA_TRUSTED_PROXY_CIDRS with the actual forwarding
chain. Signed metadata certifies Server's resolved address, not VPN use or an
adviser's identity. Apps must verify once before upgrading, retain the result for
the connection, and resolve anew on reconnect. An absent/invalid assertion must
not interrupt audio; Telephony falls back to its socket peer.

Publishing this source does not activate it anywhere. The deployed Server must
include forwarding before a new Telephony app can see the public browser exit
instead of its proxy peer. No integration catalog or carrier setting is required.
