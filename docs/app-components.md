# Owned app components

Server supports an optional `components` extension to an app release manifest.
A component is a private, ordinary app installation owned by one parent
installation. The parent keeps its database, UI and catalog. Each component has
its own identity, configuration, data directory, placement and deployment history.
The existing runtime controller manages both local and remote placement; a backend
such as Instances or Compute does not require parent-app-specific code.

The optional declaration and validation now live in App SDK (`Manifest.Components`,
`ComponentDeclaration`, `ComponentDeclarations`). Server uses the same types and
strict parser; no separate manifest stripping/validation layer is required.
Dashboard controls and actual app worker extraction remain separate work. See
[SDK app usage](../../app-sdk/docs/components.md) for authoring and call helpers.

## Release declaration

Example extension to an otherwise valid parent manifest:

```yaml
requires:
  permissions: [platform.apps.call]
components:
  executor:
    protocol: example.executor/v1
    required: true
    capabilities:
      passive_start: true
      drain: true
      replica_safe: false
      worker_coordination: false
      routing: key
    manifest:
      schema: apteva-app/v1
      name: example-worker
      version: 1.0.0
      scopes: [project, global]
      runtime:
        kind: source
        port: 8080
        health_check: /health
        source:
          repo: https://github.com/example/worker
          ref: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          entry: .
      provides:
        mcp_tools:
          - name: submit
            description: Submit an idempotent job
```

The worker is selected by an immutable source commit or verified platform
artifacts. Its full manifest goes through normal validation. At most 16
components are allowed; names are lowercase identifiers. Protocols are versioned
identifiers such as `example.executor/v1`. Nested components, public routes,
agent-facing UI/channel/prompt/skill contributions, and static workers are
rejected. The initial contract requires passive start and draining.

Declarations are stored transactionally alongside installation and upgrade
staging, with versioned snapshots retained alongside the full SDK manifest JSON. The parent's committed release
selects its declarations; a failed parent upgrade does not select the pending
release's component declarations. Existing installs are not modified to add
components automatically. `required` is descriptive metadata for callers and
future setup UI: Server does not automatically deploy it or delay parent startup.
An app should report the missing required worker through its own readiness/setup
and handle an unavailable call explicitly.

## Operator API

Read declarations, configured child IDs, status, permissions and placement:

```http
GET /api/apps/installs/:parent/components
```

Create/deploy an explicitly selected component (administrator only):

```http
POST /api/apps/installs/:parent/components/executor
Idempotency-Key: deploy-executor-1
Content-Type: application/json

{
  "placement": {"type": "local"},
  "config": {},
  "approved_permissions": []
}
```

Use the existing [backend placement](app-runtime-backends.md) object for remote
execution, including the target, reachable gateway and package platform/recipe
when required. The response is HTTP 202 with `install_id`, `owner_install_id`,
`component_name` and the existing durable runtime `operation`. Retrying the same
request/key reuses the child and operation. Preparation failure retains the child
and candidate for inspection and explicit cleanup. One execution is supported;
replica pools, replicas and public ingress are rejected for this first contract.

The same existing lifecycle routes are available beneath
`/api/apps/installs/:parent/components/:name/deployment/...`, including operations,
logs, stop, restart and discard. `.../:name/placement` uses existing placement
handling. Operators may also use the ordinary child-ID deployment/config APIs.
Upgrade a component through the component POST endpoint after approving the
parent's new declaration; an independent marketplace upgrade is rejected.

`DELETE /api/apps/installs/:parent/components/:name` uses normal uninstall:
drain, confirmed stop and managed resource cleanup. Installation data is retained
by the existing uninstall policy. Parent uninstall (even forced) is blocked while
child installations exist. Stop children before explicitly stopping/disabling the
parent. Ownership and project/user scope cannot be changed independently. A
removed declaration's child stays visible in component details for cleanup.

Children are omitted from the ordinary top-level installed-app list. The parent's
list row has optional `components` metadata. Internally a private catalog identity
`component-<parent-id>-<name>` avoids the existing one-app-per-project constraint;
the worker manifest/release keeps its real app name for package building/reuse.

## Calls and permissions

The parent uses the SDK component helper on the existing app-call transport:

```go
err := app.Components().Call(ctx,
    "executor", "submit", map[string]any{"job_id": jobID}, &result)
```

This uses the existing authenticated app-call endpoint and its result, batch,
deadline, cancellation and streaming-proxy behavior. Server resolves the alias
from the authenticated caller's installation, checks project/ownership, and
requires a compatible active deployment. There is no silent local fallback.
An unrelated app cannot call a child by guessing its alias, ID or private app
name; ordinary browser/agent proxy access and MCP registration are disabled.

The parent needs `platform.apps.call`. Calling does not grant deployment admin
rights. A worker receives its own token and explicitly approved manifest
permissions, encrypted config and data; it does not inherit the parent's secrets,
bindings or permissions. Supply scoped job inputs, including signed Storage URLs
where appropriate. Implicit `requires.apps`/integration dependencies are rejected
for workers in this initial contract. The operator must approve the exact worker
permission set, including newly added permissions on a later release.

## Worker contract and lifecycle

The worker implements normal managed-app lifecycle routes. Implementing SDK's
optional `ComponentService` interface (`ComponentProtocol() string`) makes
`sdk.Run` mount an authenticated `GET /_apteva/runtime/component/describe`
automatically. Existing custom implementations can retain that route instead.
The response is:

```json
{"protocol":"example.executor/v1"}
```

Before activation Server checks HTTP readiness, this exact protocol and the
existing `/_apteva/runtime/app/describe` lifecycle capabilities against the
approved declaration. The same declaration supplies package capability metadata
and is included in the existing package cache identity. Backend/package checksums,
ownership generations and confirmed shutdown rules remain enforced.

The existing protected `/_apteva/runtime/app/status` must report the exact
deployment ID, numeric generation, state and `admitting` boolean. Server confirms
admission after activation before publishing the worker. A healthy passive
process or HTTP 200 activation acknowledgement alone is insufficient.

Updates prepare while the old worker runs, then drain and confirm shutdown before
starting/activating its replacement. The worker's drain endpoint must wait for
existing jobs or confirmed cancellation; a deadline failure retains the previous
execution. Compatible old releases may be restored using retained approved
deployment records after failure. Calls already in progress retain their selected
connection; new calls use the committed active deployment. The parent owns durable
job IDs/outcomes and application-level retry/idempotence. Server owns process
health, routing and deployment recovery; it does not infer that a ready process
completed a job. Deploying Media's actual executor and proving a real backend
end-to-end remain app/backend integration work.

## Local verification

`apps_components_test.go` covers validation, persisted declarations and migrations,
permission approval, distinct owners, scope guards, operator idempotency, hidden
top-level rows, owner calls, cross-owner/project rejection, protocol mismatch,
registry reload, no replicas/fallback, drain-first upgrades and failed-upgrade
rollback. It uses disposable databases and an arbitrary named backend fixture;
no real installation or remote host is changed by the tests.
