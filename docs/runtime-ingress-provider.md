# Server-owned ingress, Instances Caddy adapter (0.15.0)

There is one deployment ingress workflow. Server reads the approved app manifest,
selects the current deployment and hostname, manages DNS and durable operations,
checks readiness/authenticated admission, invokes a provider, and verifies public
HTTPS before recording publication. Fresh proxy setup and explicit adoption of an
existing proxy are resource preparation operations in that same workflow.

Instances implements `apteva.ingress-provider/v1` at the protected
`/_apteva/ingress/providers/caddy` prefix. The native compute backend supplies
`ingress_upstream`, `ingress_host_key`, and an optional provider ID hint. It does
not configure or publish ingress during prepare/start/stop/delete. The old
native-backend `publish-ingress` action is removed; `managed_ingress` is false.
Server supports independent compute and ingress app installations/target
namespaces. No provider ID, Caddy command, SSH implementation, CORS policy or
Telephony route belongs in Server's orchestration.

## Upgrade order and existing installations

Use the matching Server ingress-provider controller changes before deploying
public apps with Instances 0.15.0. This is a controller contract change, not a
migration performed by an app update. Older controllers cannot create/publish
public native ingress with this release. Inventory, monitoring/history, SSH,
firewalls, storage and private native deployment APIs remain available.

Updating Instances does not stop, reload, adopt or rewrite an existing proxy.
Existing process environments and native lifecycle fingerprints remain valid.
A previously configured proxy must be explicitly adopted through Server before
its public deployment is upgraded. Resolve outstanding deployment operations
first. Adoption requires the installation's active, ready, identity-verified,
admitting managed deployment; it is not an app installation/import operation.
Server retains the pre-adoption placement/endpoints for rollback and preserves
legacy backend request options for that existing deployment only. New generations
use the one ingress-provider workflow.

## Provider contract

`GET /describe` reports the contract, Caddy adapter, formats and host constraints.
The other operations are bounded authenticated POST requests:

- `inspect-target`: read-only physical host identity/public IPv4 for Server DNS.
- `prepare`: provision a new, isolated owned Caddy service/configuration. Occupied
  ports and active connections are refused. Existing foreign proxies require adoption.
- `inspect`: read-only proxy/service/config/certificate/route evidence and a SHA-256
  plan. It creates no proxy files, deployment records or listener reservations.
- `adopt`: apply the reviewed plan in an explicitly quiesced maintenance window.
- `publish`: update an already owned proxy using the same validated publication
  primitive as adoption. Server must have confirmed deployment admission and,
  for a replacement, drained and confirmed the previous upstream stopped.
- `status`: verify the running/source config and report the actual owner/generation.
- `unpublish`: replace only that deployment's owned routes with 404 responses;
  an old deployment cannot remove a newer deployment's routes.
- `rollback`: restore the exact original source bytes and running configuration.
- `release`: acknowledge release of an already-unpublished owned hostname. It
  retains a harmless 404 vhost and a bounded protected receipt; it never stops a
  shared proxy service. Lost release responses are idempotent.

Server supplies a normalized intent containing `contract`, `controller_id`,
`operation_id`, `install_id`, `deployment_id`, `generation`, `project_id`, a
namespaced `target`, `hostname`, explicit `upstream`, approved `routes`, provider
`options`, and, for mutations, observed `host_key` and `plan_sha256`.
Application tokens, manifests, source artifacts and environments are not passed
to the proxy adapter. The Caddy adapter accepts only an HTTP loopback upstream on
its registered host; Server checks the backend's `ingress_host_key` against that
host. A different provider can implement other upstream/target capabilities.

For fresh proxies, options are resolved by the adapter into controller-owned
paths/service names. Existing-proxy options are explicit:

```json
{
  "adapter": "caddy",
  "hostname": "app.example.com",
  "service": "caddy.service",
  "binary": "/usr/bin/caddy",
  "config": "/etc/caddy/Caddyfile",
  "site_file": "/etc/caddy/sites/app.caddy",
  "format": "caddyfile",
  "admin": "unix//run/caddy/admin.sock"
}
```

Caddyfile adoption requires an isolated single-host source fragment. The complete
main configuration is adapted and compared before reload; unrelated routes, TLS
policies and global settings must remain identical. Mixed fragments, wildcard or
ambiguous ownership, nonmatching upstreams, source/live disagreement, symlinks,
hard links and unsafe permissions are rejected. JSON configurations permit an
isolated exact-host route replacement while retaining the rest of the document.
Configuration/subprocess/journal sizes are bounded. Backups and receipts remain
0600; errors/results exclude configuration contents and credentials.

The adapter revalidates the registered SSH identity, live proxy executable,
systemd definition, source hashes and local admin configuration. Target/service
changes and manual edits require a new explicit reviewed plan. Durable pending
receipts recover interrupted responses without replaying app HTTP requests.

## Server operator API

Use authenticated platform-admin POST requests to
`/api/apps/installs/<installation>/deployment/ingress/{inspect,adopt,status,rollback}`.
The inspect body selects a hostname and independent provider reference:

```json
{
  "hostname": "app.example.com",
  "provider": {
    "app_install_id": 8,
    "id": "caddy",
    "target": {"app_install_id":8,"resource_type":"instances.instance","resource_id":"1"},
    "options": {"adapter":"caddy","hostname":"app.example.com","service":"caddy.service","binary":"/usr/bin/caddy","config":"/etc/caddy/Caddyfile","site_file":"/etc/caddy/sites/app.caddy","format":"caddyfile","admin":"unix//run/caddy/admin.sock"}
  }
}
```

For adopt, add the returned `plan_sha256`, `provider_plan_sha256`, `host_key`, a
fresh `idempotency_key` and `maintenance_window:true`. Server returns the normal
durable deployment operation (202); existing operation status/retry endpoints
apply. A changed request cannot reuse an idempotency key. Recovery reads provider
status first; an expired one-minute maintenance attestation cannot authorize a
new cutover. Submit a fresh reviewed request when a new window is required.

Application authentication and CORS remain application-owned. Generated routes
preserve every declared surface, including browser/WebSocket and callback paths,
while excluding private runtime controls. A marker validates public TLS and the
selected deployment without sending real carrier callbacks.

## Connection safety and rollback

Initial adoption requires externally quiesced arrivals and zero established
80/443 connections. A quiet instant is not treated as an operator maintenance
attestation. Active WebSockets cause refusal before changing configuration; the
proxy is never restarted for adoption. Managed reverse proxies use a 24-hour
stream-close delay as an additional safeguard, not as a promise of indefinite
connection preservation. Unknown draining/ownership outcomes stay unconfirmed.

Original-proxy rollback is limited to the same still-active deployment. After an
application upgrade, use Server's application deployment rollback, which restores
and admits the appropriate execution before publishing its routes. A failed
public verification during initial adoption restores the original proxy when
ownership and the maintenance window are still valid; otherwise retained intent
requires explicit inspection. No rollback overwrites externally changed config.

Validation uses temporary databases/loopback services only. The adapter suite
includes a real local Caddy HTTPS/WebSocket process with a fixture-only CA (no
system trust changes), browser and POST callback fixtures, unchanged certificate
fingerprints, unrelated-site preservation, original-byte rollback, preparation,
ownership, drift, retries and interrupted reloads. Production is not a test target.
