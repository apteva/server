# Account devices and the Node connection

This implementation uses the current local server and Node sources. Nodes pair
with an account and inherit that user's normal API permissions across projects.
The selected project and agent are context, not credential restrictions.

## Pairing and management

- `POST /api/devices/pair/start`: `{name, version, hardware: [{id, kind, description?}]}`.
  Returns `{pairing_id, user_code, verify_url, expires_in, interval}`. Requests
  expire after ten minutes. The verification link opens Settings → Devices;
  its code is preserved through sign-in, including MFA.
- `GET /api/devices/pair/request?code=...`: authenticated preview of a pending
  request. Does not expose its private polling ID or credential.
- `POST /api/devices/pair/approve`: `{code, project_id?, agent_id?}`. Approval
  belongs to the authenticated account. Defaults must be accessible to it.
  Repeating approval by that account returns the same device.
- `POST /api/devices/pair/deny`: `{code}`.
- `POST /api/devices/pair/poll`: `{pairing_id}`. Returns `pending`, `approved`,
  `denied`, or `expired`. Approved responses include `token` and `device_id`.
  Poll retries recover the same credential until the first successful Node
  handshake. The temporary handoff is encrypted; permanent credentials and
  private polling IDs are stored only as hashes. Expired handoffs are removed
  when starting another pairing request.
- `GET /api/devices`: account-owned devices, including connected/last-seen
  status, declared hardware/tools, and revoked records.
- `GET /api/devices/:id`: account-owned device details.
- `PATCH /api/devices/:id`: `{name?, project_id?, agent_id?}`. Updating an
  online device reconnects it so the next handshake applies its defaults.
  Changing the project clears the agent unless an agent is explicitly supplied.
- `DELETE /api/devices/:id`: revoke its credential and disconnect it immediately.

Use `Authorization: Bearer <device token>` or `X-API-Key`. Device tokens are
accepted by normal authenticated API routes, including `/api/auth/me`; existing
project roles, capabilities, and administrator checks still apply. Query-string
credentials are rejected. Device tokens are not app-install credentials and
cannot impersonate an app callback. `POST /api/auth/logout` with a device token
revokes that device. Password resets and MFA session sweeps revoke all account
Nodes and close their sockets. Account deletion cascades device records.

## WebSocket bridge

Connect to `/api/devices/link` with the device credential and send the existing
Node protocol v1 `hello`. The server validates the version and tool declarations,
then sends `welcome`, `projects`, and `agents`. Projects and agents cover the
account's accessible directory. The Node UI provides project selection.

Frames retain the envelope `{type, id?, data?}`:

- `welcome`: adds `device_id` and `project_id` to the existing agent fields.
- `projects`: `{projects: [{id, name}]}`.
- `agents`: existing agent records plus `project_id` and `project_name`.
- `select_project`: `{project_id}`; confirmed with a correlated `welcome`.
- `select_agent`: `{agent_id}`; server authorization also resolves its project.
- `app_request` / `app_response`: JSON app calls run through the existing app
  proxy, its installation resolver, admission controls, and user/project role
  checks. Explicit `project_id` captures the context when a request is created;
  older clients may omit it to use the connection's selected project. The Node
  credential is never forwarded to a sidecar. Responses are bounded to 1 MiB;
  streams/upgrades do not use this JSON channel.
- `invoke` / `result`: correlated hardware calls with local confirmation.
- `cancel_invoke`: cancels one pending local confirmation after request
  cancellation or expiry. Disconnect/revocation cancels all pending confirmations.
- `event`: `{event_id, capability, kind, data?, at}` with the same stable ID in
  the frame envelope. The server commits its receipt and subscription fanout to
  SQLite before sending `ack {accepted: true, duplicate}`. Invalid identities,
  changed payloads for an existing ID and undeclared events are rejected.
- `state`: validates declared capabilities and acknowledges receipt; state changes
  do not independently trigger agents.
- `error`: explicit unsupported-frame or authorization failures.

`POST /api/devices/:id/invoke` accepts `{tool, args}` from the owning account.
Only tools announced by the connected Node are callable; at most sixteen calls
may be pending and each expires after two minutes. Results preserve denial/error
fields. Calls are never replayed automatically on a replacement connection.

Only one connection is active per device. A new connection replaces the old one.
Ping/pong maintains liveness; directory refresh rechecks project membership.
Revocation uses close code 4003, which immediately returns Node to standalone
mode. Other reconnects use 4001. Server shutdown closes its device sockets.

## Agent-facing MCP tools

Node defines tools and JSON argument schemas from its hardware configuration in
`hello`; the server exposes those declarations through MCP. It does not invent
hardware tools. Each device has a stable, account-owned registry identity and
appears in the agent capability picker. Hardware execution always travels over
the authenticated device WebSocket and still passes Node's local confirmation
and safety guard. An offline device fails a call; hardware calls are never queued
or replayed automatically.

Settings → Devices → **Agent tools & events** grants selected tools to particular
agents. Devices remain account-wide, and a grant may target any project the owner
can edit. Each grant captures the agent's project, rechecks current permissions on
every MCP request/invocation, and has a revocable revision. Moving an agent to a
different project invalidates its previous grant. New advertised tools do not
expand existing grants. Changed schemas refresh existing attachments through the
normal agent configuration flow. Revoking a grant also cancels pending calls and
local confirmations. Device removal revokes all its capabilities immediately.

- `GET /api/devices/:id/grants` returns grants.
- `PUT /api/devices/:id/grants/:agentID` with `{tools: [name, ...]}` saves a
  grant and attaches the device through the normal agent MCP configuration API.
- `DELETE /api/devices/:id/grants/:agentID` revokes the grant and detaches it.
- Explicitly attaching the device from the agent capability picker grants its
  currently advertised tools; use Devices for a restricted subset. Removing that
  attachment revokes its grant.

Core uses the loopback-only `/mcp/devices/:deviceID` endpoint with an
agent-specific signed capability. It supports MCP initialize, ping, tools/list,
and tools/call over stateless HTTP. Public device credentials cannot be used as
MCP credentials. Discovery filters tools by grant, includes the device's schemas
and confirmation information, and calls validate Node's bounded schema subset
before forwarding. Unsupported schema assertions fail closed. Device errors and
human denials are MCP tool errors; JPEG results include image content.

## Button and sensor subscriptions

The same Devices panel configures a declared capability/event, a target agent,
thread, instruction, cooldown and maximum event age. Events & webhooks links to
these controls. Choosing another agent on the device screen never retargets a
subscription or queued delivery. Threads other than `main` must already exist;
the bridge never redirects a missing thread to main.

- `GET/POST /api/devices/:id/event-bindings` lists/creates subscriptions. POST
  takes `{agent_id, capability, kind, thread_id?, instruction?, enabled,
  cooldown_seconds?, max_age_seconds?}`. Project identity comes from the verified
  agent, never request arguments. Default thread is `main`, expiry 300 seconds.
  Cooldown is 0–86400 seconds; expiry is 1–86400 seconds.
- `PATCH /api/devices/:id/event-bindings/:bindingID` with `{enabled}` pauses or
  enables a subscription. Pausing cancels pending deliveries; enabling does not
  replay them. Delete removes it and cancels pending work. Target/options changes
  use a new subscription, keeping old captured deliveries separate.
- `GET /api/devices/:id/event-deliveries` returns the most recent 50 deliveries,
  including attempts, errors and Core execution IDs. `delivered` means durable
  Core admission, not successful completion of the agent's task.

Node writes events atomically to a private, fsynced disk queue before attempting
transmission. Acknowledgements remove them durably. The queue holds at most 512
entries/4 MiB, expires entries after 24 hours and reports overflow, storage errors
and expiry through device actions/logs. Queue entries are bound to the original
server and pairing credential; re-pairing or standalone/reset cannot replay them
into a different account. A daemon restart retains unacknowledged events.

The server stores deduplication receipts and immutable per-subscription deliveries
in one transaction. Delivery retries use a stable Core event ID and require its
tracked execution receipt. Core's existing event deduplication protects the
accepted-but-response-lost case. Permission loss, project moves, revocation,
removed subscriptions and missing threads cancel delivery. Stopped agents and
transient failures retry with bounded backoff until the original event expires.
Cooldown-suppressed and expired deliveries remain visible in history. Receipts
are retained for seven days, exceeding Node's maximum replay age.

## Verification

Focused server tests cover pairing, credential/header isolation, account-wide
selection, grants and project moves, filtered MCP discovery, invocation/denial,
cancellation, event deduplication, captured targets, retries, cooldown and expiry.
Node's race-tested suite covers its hardware guard, connection handling and disk
outbox. Dashboard tests cover grant selection, subscription creation, pause/enable
and errors. A real-daemon transport test uses simulated GPIO and a fake Core
admission endpoint, without inference or live accounts:

```sh
cd ../node && ./build.sh
cd ../server
env GOWORK=off APTEVA_NODE_TEST_BINARY=../node/bin/apteva-node \
  go test -run TestDeviceRealNodeDaemon -count=1 -timeout=90s .
```

Widget placement and app-specific refresh topics are separate from this hardware
integration. There is no Conversations dependency.
