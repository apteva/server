# Managed app runtime backends

Apteva 0.90.0 provides the Server implementation of `apteva.runtime-backend/v1`.
SDK v0.102.0 supplies portable package and public ingress declarations.
Instances, Containers, Compute, or another app can implement the HTTP contract
using ordinary protected SDK routes. There are no backend app-name switches.
Existing installations remain on the legacy local supervisor until explicitly
placed under the deployment controller.

## Responsibilities

Server owns placement, installation identity, permissions/bindings, deployment
records, encrypted credentials, operation ownership, port reservations, readiness,
activation, rollback, routing, reconciliation, and scaling decisions. Backend apps
own their provider API/SSH/container/process implementation and resource cleanup.
Server does not sample or record host CPU usage. Autoscaling uses backend-reported
application work load, such as queue depth or active work.

Backend apps do not import Server internals. SDK helper interfaces and concrete
backend implementations are separate work. SDK v0.100.0 supplies the activation
strategy declaration; it does not automatically implement this runtime protocol.
Real backend/app compatibility must be verified before opting an app into remote
migration or replication.

Automatic binary package preparation/cache/delivery is an optional Server
extension. See [managed packages](app-runtime-packages.md) for target-platform
builds, exact-release capability metadata, backend opt-in, streaming delivery and
recovery. Backends without this extension retain delegated preparation.

## Register and discover a backend

An administrator registers a running app installation:

```http
POST /api/apps/execution-backends
Content-Type: application/json

{
  "app_install_id": 52,
  "id": "native",
  "endpoint": "/_apteva/runtime/backends/native"
}
```

Server calls the provider's authenticated `GET <endpoint>/describe` and persists
its capabilities. The descriptor must declare `contract`, matching `id`,
`runtime_kinds`, `formats`, and `confirmed_stop`. Optional capabilities include
`platforms`, `protocols`, `snapshots`, `replicas`, `resource_controls`, `fencing`,
`target_schema`, and `options_schema`. Fencing in v1 is deployment-scoped;
host-wide power-off is not an implicit cleanup operation.

`GET /api/apps/execution-backends` returns the catalog and local adapter.
`POST /api/apps/execution-backends/discover` scans installed manifests for
explicit HTTP routes under `/_apteva/runtime/backends/<id>` and negotiates their
descriptors. This works with existing SDK route declarations. Future manifest
`provides.runtime_backends` declarations can advertise the same endpoint.
Discovery is bounded to 64 candidates and a 30-second request budget.

Public app proxies and static app mounts reject `/_apteva/runtime/` paths, even
if an app accidentally declares a broad public route. Direct backend ingress
must keep these endpoints private and authenticated too. Server app tokens
cannot access backend/pool management endpoints.

## Placement and targets

```json
{
  "type": "backend",
  "backend": {"app_install_id": 52, "backend_id": "native"},
  "target": {
    "app_install_id": 40,
    "resource_type": "instances.instance",
    "resource_id": "6"
  },
  "gateway_url": "https://apteva.example.com",
  "options": {}
}
```

The backend reference selects an execution implementation. The namespaced target
selects a resource; Server does not interpret resource-type strings. Backend and
target owners must be accessible in the installation's project. A backend cannot
manage itself or introduce a deployment dependency cycle. `type: local` uses the
existing supervisor through an adapter.

Remote execution requires an authenticated reachable gateway, not the management
machine's loopback gateway. Control URLs require HTTPS or loopback tunnels.
Backend `validate-target` checks target/options schemas, actual capacity, platform,
networking and connectivity. Its stable `host_key` is the reservation domain; the
prepared execution must return the same key. Backends targeting the same physical
listener namespace must return the same key, including through resource aliases.

## Operator API

The existing install API accepts optional `placement` alongside its manifest,
configuration and bindings. Existing installation placement uses:

- `GET /api/apps/installs/:id/placement`
- `POST /api/apps/installs/:id/deployment/prepare`
- `POST /api/apps/installs/:id/deployment/activate`
- `GET /api/apps/installs/:id/deployment/operations/:operation_id`
- `POST /api/apps/installs/:id/deployment/operations/:operation_id/cancel`
- `POST /api/apps/installs/:id/deployment/operations/:operation_id/retry`
- `POST /api/apps/installs/:id/deployment/discard`
- `POST /api/apps/installs/:id/deployment/restart`
- `POST /api/apps/installs/:id/deployment/stop`
- `POST /api/apps/installs/:id/deployment/delete`
- `GET /api/apps/installs/:id/deployment/logs?cursor=...`
- `GET /api/apps/installs/:id/deployment/endpoints`

Mutating lifecycle calls require an administrator and an `Idempotency-Key`
(identifier up to 128 characters). Same-key/same-request retries return the same
operation; changing the payload returns a conflict. One queued/running operation
per installation is enforced in SQLite. Prepare stores a pending candidate but
keeps the current route serving. Activate commits only after readiness. Failed
or cancelled preparation retains discoverable candidate ownership until discard
cleans its resources; it never silently deletes an unknown provider resource.

Operations are persisted with phases, timestamps, an owner and an expiring
heartbeat lease. Progress uses `runtime.operation.updated` on the existing app
event bus. Cancellation is cooperative between bounded steps. After commit,
retirement and rollback must finish rather than be abandoned by cancellation.
Retries preserve operation/deployment IDs for backend idempotency.

## Backend wire protocol

Backend control requests carry:

- `contract`, `operation_id`, and `idempotency_key`;
- `install_id`, `deployment_id`, `generation`, and `project_id`;
- namespaced `target`, backend `options`, and the exact manifest;
- previous `execution` and reusable `state_ref` where applicable.

Only `start` receives the process environment and credentials. Other operations
must not log or echo them. Server persists these fields encrypted and excludes
them from operator deployment responses. Backend logs are bounded and known
process/control tokens are redacted.

Server invokes `POST <endpoint>/<action>` for `validate-target`, `prepare`,
`start`, `inspect`, `stop`, optional `fence`, `delete`, and `logs`. Responses are
bounded JSON. HTTP 200/202 are accepted. Preparation can return `state: preparing`
and is then polled through `inspect` for up to 30 minutes. Provider-side operation
progress must survive provider restarts.

The execution descriptor includes `handle`, `state`, `host_key`, `release_digest`,
`platform`, `control_endpoint`, optional `browser_endpoint`/`endpoints`,
`state_ref`, allocated `ports`, `ready`, and `stopped_confirmed`. A prepared
package needs an exact SHA-256 digest. Matching checksummed artifacts are verified
against the manifest. An implementation must preserve the stable execution
handle, reservation domain and release digest across start/inspection. State
reference reuse means reuse of the existing persistent state, not an empty volume.

Backend readiness must include all required listeners. Server independently
checks authenticated app HTTP readiness within the app's startup budget. TCP/UDP
port/range reservations are keyed by host reservation domain and prevent overlap
between installations. Native local fixed ports also use existing supervisor
reservations, so managed and legacy local apps cannot claim the same listener.

`stop` must report confirmed termination/listener release. Failed or uncertain
stops block replacement unless the backend can provide deployment-scoped fencing
and confirmed loss of the previous execution's ownership. A timeout is never
interpreted as successful termination. `delete` removes only resources belonging
to the addressed deployment and must preserve state shared with retained versions.

## App lifecycle and state

Apps can expose protected endpoints:

- `GET /_apteva/runtime/app/describe`: `passive_start`, `drain`, `replica_safe`,
  `worker_coordination`, and `routing` (`stateless` or `key`).
- `POST /_apteva/runtime/app/drain`: stop new admission; return `drained: true`
  only after active work/writers have drained.
- `POST /_apteva/runtime/app/activate`: resume admission for the verified owner.

The prepared package descriptor carries the app capabilities for that exact
release. The app must actually honor `APTEVA_RUNTIME_MODE=standby`. A passive
process must not admit work or produce external effects before its ownership
becomes active. Existing app permission checks and call-specific media leases
remain necessary.

Process environment includes installation/project identity, configuration,
gateway/public URLs, deployment ID/generation, endpoint metadata, and a unique
deployment credential. Incoming proxy authentication follows the selected
replica's credential. Outgoing credentials are valid only for eligible deployment
states. Pending executions have initialization read access; after activation they
receive their normal approved permissions. Retiring executions can finish work
while draining, then lose callback access on confirmed stop.

`GET /api/apps/callback/runtime/ownership` returns the calling execution's active
ownership and a short renewal horizon. App lifecycle implementations must enforce
that ownership where required; a generation field alone does not fence a process.

SQLite or declared filesystem state moving between different state locations
requires passive startup and snapshot capabilities on both sides. Writers stop
before transfer. Archives and digests are journaled for restart recovery, and
restoration requires checksum acknowledgement. Server never restores underneath
a started candidate without a recorded verified restore outcome.

Native snapshots cover the installation's SDK data directory. Unmapped extra
local storage mounts are rejected rather than silently omitted. Snapshots are
bounded to 512 MiB; archives reject traversal, links, special files and oversized
expansion. Larger state or specialized volumes require an extended provider
transfer contract. Rollback uses the previous startup deadline and verifies
readiness. `database_upgrade: requires_restore` stays blocked; binary rollback
cannot undo arbitrary committed schema changes.

## Browser endpoints and credentials

Endpoint discovery retains the existing installation proxy base and optionally
returns direct browser/signaling/media addresses. Existing HTTP/MCP callers keep
using the same installation ID.

`POST .../deployment/browser-credentials` issues a five-minute credential scoped
to installation, project, user identity, operations, deployment generation,
issuer and browser origin. It never returns a process token. Source apps authorize
it through authenticated `POST /api/apps/callback/runtime/browser-authorize` and
must retain business-level authorization. Renewal rechecks access; explicit
revocation uses `POST .../deployment/browser-credentials/revoke`. Generation
changes invalidate old credentials. Long connections must revalidate ownership,
expiry and revocation and close when no longer authorized; CORS is not sufficient.

## Pools and scaling

`POST /api/apps/execution-pools` stores a project-scoped backend and namespaced
target list. Placement can select `pool_id`; Server spreads allocations using
persisted deployment counts. The backend validates actual target capacity.

`POST .../deployment/scale` accepts `replicas` (1–32). It requires an explicit
replica-safe app, worker coordination, supported routing, and a replica-capable
backend. Local supervisor replication, writable SQLite copies, exclusive apps and
restore-required apps are rejected. Each replica has its own credential and
execution handle. Normal HTTP requests distribute across ready replicas; keyed
apps use `X-Apteva-Routing-Key` or `runtime_routing_key`. Existing upgraded
connections stay with their chosen replica.

`POST .../deployment/autoscaling` configures minimum/maximum replicas,
`target_load`, `cooldown_seconds` (at least 30), and `enabled`. Backend `inspect`
reports finite nonnegative application work load. Scaling has hysteresis and a
cooldown; absent/unreachable observations do not trigger scale-out or failover.
CPU sampling is not part of this controller.

## Recovery and operational boundaries

Managed installations are excluded from legacy local resume/reconstruction.
Remote unreachability retains ownership and never creates a local fallback.
Interrupted operations are reclaimed only after their owner lease expires.
Execution inspection recovers start responses lost during a restart. Native
processes journal their PID before exec and validate process identity before
stopping a surviving orphan. Unknown start/stop outcomes require attention rather
than overlapping another process.

Managed upgrade/restart/configuration changes use the controller. Disable and
cleanup confirm all replicas stopped. Providers and target-owner apps cannot be
disabled/uninstalled while retained deployments depend on them. Cleanup preserves
ownership if a backend is unavailable. Server-node high availability, live-call
migration and implementing concrete infrastructure providers are not included.

Tests use protocol providers and real temporary native subprocesses. They cover
idempotency, authorization, private endpoints, generation credentials, browser
scope/revocation, exclusive activation, lost responses, verified/failed rollback,
state snapshots, interrupted starts, port ranges, replica routing/stop, and native
orphan recovery. Production infrastructure/media paths have not been exercised.


## Dashboard placement and target discovery

The Apps install flow offers **This Server** or a discovered execution backend.
Execution placement is independent of app integration bindings. The installed
app's Deployment tab uses the persisted controller operations; navigating away
or restarting Server does not discard progress. App list badges identify backend
and target, with local/remote, backend and target filters at every project scope.

New backends advertise `target_listing: true` in `describe` and implement
`GET <backend endpoint>/targets?project_id=...&q=...&cursor=...` using the same
private installation authentication as their lifecycle routes. Return:

```json
{
  "targets": [{
    "ref": {"app_install_id": 42, "resource_type": "compute.machine", "resource_id": "host-1"},
    "name": "Trial worker", "status": "ready", "available": true,
    "platform": "linux-amd64", "region": "example-region"
  }],
  "next_cursor": ""
}
```

Target references must belong to the selected backend installation. A backend
must apply its own resource authorization; Server also checks installation and
project access. Return `available: false` with a safe `reason` for unavailable
resources. Responses are bounded, cached for display labels, and may use opaque
pagination cursors. Never include credentials or private control URLs in labels.

For older backends, an administrator can register `target_discovery` through
`POST /apps/execution-backends`, or the backend can advertise the same field in
its descriptor. This maps a private inventory **GET** route without executing
inventory tools or creating resources:

```json
{"path":"/api/resources","items":"resources","id_field":"id","name_field":"name","status_field":"status","ready_value":"ready","resource_type":"compute.machine"}
```

The mapping survives backend rediscovery. It is backend configuration, shared
by all deployed apps; Server contains no provider-name or app-name switches.
Native target listing is preferable for large inventories and project-specific
resource authorization. Legacy mapped inventories are capped at 5,000 records
and paginated by Server. Returned fields other than safe target metadata are
not forwarded to the browser.

The console API adds `GET /apps/deployment-targets` and
`POST /apps/deployment-preflight`. Preflight checks access, runtime/listener
compatibility, ingress capabilities, state transfer and optional read-only
`inspect-target`. Builds, uploads, listener ownership checks and activation
remain authoritative in the deployment controller. Preflight does not start
an app or change host networking. Backend `options_schema` drives advanced
fields; backend validation remains authoritative for backend-specific options.

`POST /apps/installs/:id/deployment/deploy` prepares and activates in one
persisted operation, with the existing `Idempotency-Key` contract. Remote
installation requests may also supply this header; receipts atomically link
that key to the installation, and retries resume the stored release instead
of creating another installation. A different payload with the same key is
rejected. Stop/remove-runtime retains persistent installation data and never
removes the backend host or unrelated services.

Deployment summaries distinguish current process readiness from app admission.
A stored active record alone is not a health check; observations expire after
90 seconds. An app may optionally expose authenticated
`/_apteva/runtime/app/status` returning `{"state":"active","admitting":true}`.
Without that optional status, the console reports process readiness and unknown
admission, rather than asserting that the app is accepting work. No host CPU
sampling is added. Domain setup uses the existing managed ingress capabilities;
only apps with declared public routes and capable backends can select HTTPS.
