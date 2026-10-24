# Stable-host deployments

The optional `apteva-host` keeps the HTTP/TLS listeners running while it replaces
`apteva-server`. The CLI commands remain `apteva`, `apteva update`,
`apteva rollback`, and `apteva service ...`. Existing installations without the
host keep their existing restart behavior.

This is a single-writer standby handoff. Blue serves while green is probed and
prepared; new requests briefly queue while ownership moves. It reduces update
interruptions on one machine. It does not guarantee 100% uptime: machine, host,
network, disk, client timeout, and application failures still need independent
high-availability infrastructure.

## Data and process ownership

The host and both server versions use the same explicit `APTEVA_HOME`, `DB_PATH`,
`DATA_DIR`, and public port. Neither version gets a second writable copy of the
installation database. An OS file lock fences controller ownership. Host control
uses a private mode-0600 Unix socket; backend loopback requests require a private
per-slot token. Client address, scheme and Host routing survive the proxy.

Candidates first advertise protocol support using disposable paths. Preflight
makes a consistent SQLite backup, applies the candidate migrations to a disposable
clone, and compares schema and stored rows. The trigger-maintained
`app_metadata_generation` cache epoch is excluded from the row comparison; the
underlying app and MCP records are still checked. Insufficient disk headroom,
changed schema, data transformations, pending restore markers, or missing keys
abort the update while blue stays active.

Green prepares the existing encryption key and schema before the switch. After
request drain, a fast identity check catches schema or key changes during warming.
Activation opens the existing database without migrations or database creation.
It cannot generate a replacement encryption key. App SQLite databases and other
per-install data remain in their original paths.

Before retirement the server freezes admission of app installations, deployment
operations, live binding workers and core rollouts. Outstanding operations cause
a safe refusal. Live core processes, including platform helpers, are adopted even
when boot resume is manual. Failed handoff adoption never kills the original
process or clears its durable PID/key. Legacy sidecars are adopted; managed
runtime deployments keep their durable process identities and existing controller
contracts. The host owns new sidecar processes and their rotating log readers,
so retiring a management slot cannot close their stdout pipes.

## Connections and recovery

Finite requests drain before retirement. Requests queue once; mutations are never
replayed automatically. Loopback callbacks needed to finish in-flight work remain
admitted until drain completes. WebSockets and CONNECT tunnels keep their selected
backend; an unfinished call blocks the switch and eventually aborts deployment
rather than being forcibly disconnected. Persisted dashboard GET subscriptions
reconnect after retirement. The dashboard never forces an automatic page reload.

Startup callbacks use that same narrow loopback exception before activation
and while waiting for an attested recovered active or pending slot. The backend
still authenticates each callback; forwarding headers cannot grant the exception.
Ordinary requests remain queued until readiness and publication. Failed activation
or slot cleanup removes its startup registration without clearing a newer one.

The journal is `$APTEVA_HOME/host/state.json`. It records the committed version and
attested slot PID, unique socket and proxy key. A restarted host may adopt that
slot only after verifying its private identity. A controller crash restarts the
committed binary against the current database; it never restores a preflight
snapshot. Failed activation rolls back to the previous binary against that same
current database. Old releases without the standby protocol and incompatible
schemas require an explicit maintenance migration; there is no silent restart
fallback. If ownership cannot be verified, the host stops activation and reports
`recovery_required`.

A clean service stop stops the installation. Its next start uses normal boot
resume policy. Host process failure can interrupt ingress and log pipes;
sidecar survival through a host/machine failure is outside this guarantee.
Managed stdio MCP runners restart with the controller; their definitions and data
are retained. Core update policies still apply after the management switch:
`preserve` keeps running versions; `rolling`/`restart` request the existing core
rollout coordinator. A core replacement can interrupt that core's own work.

## Installation and rollout

Release archives include `apteva-host` beside the existing four binaries. It is
optional when reading older archives. Foreground `apteva` selects a sibling host
unless an explicit server binary override was supplied. New service units select
the host when available. Existing units continue to use their original command
until deliberately converted. Container images include the host but retain their
existing entrypoint; container rollouts need separate orchestration.

Converting an existing live service is a one-time maintenance restart. Before
conversion, capture a consistent platform backup and app data, record the actual
unit/plist, environment, overrides, ports and current version, and test the exact
restore procedure. Preserve all operator overrides. `apteva service install`
regenerates the standard unit/plist; do not run it blindly over a customized
production installation. Review the concrete unit change and restart separately.
No production conversion is performed by these source changes.

The host executable itself remains running during updates and needs an explicit
service restart to upgrade. Hosted updates keep older version directories so its
executable and rollback targets cannot be pruned while in use. Inspect retained
preflight backups under `$APTEVA_HOME/host/preflight/` when managing disk capacity.
Use `apteva update --dry-run` for the same host preflight without activation.

## Verification

Normal checks:

```sh
go test ./... -short
go test -race ./internal/host
go test -race . -run '^TestHosted'
```

An opt-in real-server test uses only temporary local data. Build the current
server twice with test version stamps, then the host and CLI:

```sh
# From server/, with sibling apteva/ available:
GOWORK=off go build -ldflags '-X main.Version=0.90.6 -X main.CLIVersion=0.90.6' -o /tmp/apteva-bluegreen-server .
GOWORK=off go build -ldflags '-X main.Version=0.90.7 -X main.CLIVersion=0.90.7' -o /tmp/apteva-bluegreen-server-next .
GOWORK=off go build -o /tmp/apteva-bluegreen-host ./cmd/apteva-host
GOWORK=off go -C ../apteva build -o /tmp/apteva-bluegreen-cli .
GOWORK=off APTEVA_HOST_E2E_BIN_DIR=/tmp go test . -run '^TestHostedRealServerHandoff$' -count=1 -v
```

It verifies concurrent writes through update and public CLI rollback, a real
sidecar's unchanged PID/start count and database, the existing key, and an adopted
helper using a local core API fixture and a real OS process. It uses no live LLM
or production credentials. Failed runs retain their disposable home for diagnosis.
