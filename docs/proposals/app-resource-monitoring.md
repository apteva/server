# Apteva app resource monitoring proposal

Status: proposed

Date: 10 October 2026

Scope: a separate host monitor, process attribution in `server`, packaging in
`apteva`, and resource views in `dashboard`.

## Recommended design

Add an optional `apteva-resource-monitor` host service that measures CPU,
memory, disk activity, and resource contention for each app installation,
agent core, and Apteva server process. Show a contributor table and historical
charts in the dashboard, separated by production, staging, project, and host.

The monitor owns sampling, aggregation, retention, and resource alerts. It
stores measurements in its own database. Apteva server supplies trusted
workload identities and serves authorized reads; it does not sample CPU or
write resource measurements into its telemetry database. Existing transport
events retain their error, timestamp, connection, and call correlation fields.

Use process accounting for an initial release and cgroup v2 accounting for
reliable production attribution. Enable measurement without imposing resource
limits or changing application scheduling policy.

## Questions this should answer

- Which app consumed the VPS CPU during a particular interval?
- How much did production, staging, agent cores, and the server each consume?
- Did a telephony failure coincide with scheduling pressure, memory pressure,
  disk stalls, CPU throttling, or a process restart?
- Is an app accumulating memory or spawning excessive helper processes?
- How much host capacity remains, including workloads outside Apteva?

An app's resource measurements describe its processes. They do not assign all
indirect platform work to that app. For example, CPU used by the shared server
to proxy telephony remains in the server row. Remote model execution does not
consume VPS CPU; local providers and subprocesses do.

## Existing integration points

The current source provides most identity information needed for attribution:

| Location | Existing behavior | Proposed addition |
| --- | --- | --- |
| `server/apps_local.go` | Tracks processes by installation; starts a separate process group; retains old processes during upgrades | Inventory all active and retiring runs, plus build processes |
| `server/environment_install.go` | Persists environment app installation PIDs | Include project and environment identity in inventory |
| `server/environment_process.go` | Has a separate sandbox app launch path | Apply the same identity and accounting contract |
| `server/instances.go` | Starts agent cores and can reattach to surviving processes | Inventory agent runs and preserve identity through reattachment |
| `apteva/service.go` | Generates the systemd service and defines restart behavior | Package the monitor and support delegated accounting when enabled |
| `dashboard/src/pages/Monitor.tsx` | Shows agent activity and project scopes | Add a Resources view using the same project context |
| `server/websocket_telemetry.go` and `server/realtime_proxy.go` | Record transport lifecycle and termination events | Link authorized incident views to resource history by timestamp and participating workload |

The existing `LocalSupervisor.PID(installID)` reports the current process only.
It is insufficient for accounting during an upgrade, when an old process may
still serve traffic. Resource inventory must include both runs.

## Architecture and ownership

```text
Apteva server instances
  publish trusted process inventory and lifecycle metadata
                  |
                  v
apteva-resource-monitor
  reads OS counters independently of request handling
  maintains a separate bounded resource database
  exposes a private local query socket
                  |
                  v
Apteva server authorization and query adapter
                  |
                  v
Dashboard Resources view and transport incident links
```

| Component | Responsibility |
| --- | --- |
| Server process supervisor | Workload identity, process lifecycle, optional placement into delegated cgroups |
| Host monitor | OS sampling, derived metrics, rollups, retention, monitor health, resource alert state |
| Server query adapter | Project and administrator access checks, bounded queries, presentation metadata |
| Dashboard | Contributor table, history, incident overlays, collection status |
| App code | No required instrumentation for basic OS accounting |

Package the monitor as an independent Go binary. A suitable first location is
`server/cmd/apteva-resource-monitor`, with implementation in a dedicated
internal package that does not depend on the server's `main` package. It must
remain runnable when the server restarts. This avoids a new SDK requirement
for every app and keeps deployment dependencies small.

## Workload identity and attribution

Assign each logical workload a stable identity containing `host_id`,
`server_instance_id`, `kind`, and the applicable `install_id`, `agent_id`,
`project_id`, and `environment_id`. Allocate a separate `run_id` for every
process generation. Record host boot ID, PID, process start identity, launch
time, exit time, version, and accounting source.

Treat a PID as a locator, never as durable identity. Linux exposes a process
start-time field in `/proc/<pid>/stat`; combine it with boot identity to reject
PID reuse. Keep process-derived rates unknown until two valid counter samples
exist for a run.
[Linux process accounting reference](https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html).

Use authoritative project and environment records for production and staging
labels. A missing environment label should display Default or Unclassified,
rather than being inferred from a port or database directory. Version the
label metadata so moving or renaming an installation does not rewrite history.

Use these attribution rules:

| Workload | Accounting rule |
| --- | --- |
| Local sidecar app | Installation and all owned helper processes |
| Blue-green upgrade | Sum old and new app runs for the installation; allow expansion by run |
| Agent core | Agent and its owned subprocesses, excluding separately registered shared services |
| Server | Server process and otherwise unattributed server work, excluding registered children |
| App build | Separate Build row, associated with the target installation where known |
| Monitor | Separate Monitor row, including its database and aggregation work |
| Static UI | Shared server workload; no invented per-UI CPU percentage |
| Remote app | Unavailable on this host unless its host supplies its own monitor |
| Shared worker or database | Shared service row unless the workload has an independent accounting boundary |

An OS process belongs to one contributor in an additive view. Parent totals
and child totals must not both enter that sum. Host totals include all work;
the Apteva subtotal includes only its attributed workloads. Show the residual
as Other host activity, with reconciliation quality, rather than assigning it
to the server. Sampling alignment and kernel accounting differences can cause
small residual discrepancies; do not fabricate a balancing app measurement.

## Accounting backends

### Initial process backend

On Linux, sample process CPU counters and resident memory through `/proc`.
Discover helper processes using supervisor inventory, parent relationships,
and process groups. Inventory must include builds, retiring runs, and
reattached cores. Inspect process identity on every discovery pass and discard
rates when the generation changes.

Label this backend Process estimate. A helper can detach from its process
group, reparent, or run entirely between samples. Summed process RSS can count
shared pages more than once. These limitations are acceptable for a first
contributor view, but must remain visible in details and exports. A known
stopped process displays Stopped; a failed measurement displays Unknown.

For macOS development, provide a process backend using supported process
interfaces with matching metric semantics where possible. Unsupported metrics
remain unavailable. The first production qualification targets Linux.

### Production cgroup backend

Use cgroup v2 groups for workload runs. Relevant kernel interfaces include
`cpu.stat`, `memory.current`, `memory.events`, `io.stat`, and `pids.current`.
Counters include the relevant descendants; read leaf workload groups for the
additive contributor table. [Kernel cgroup interfaces](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html).

For a native systemd deployment, prefer one delegated subtree per server
instance. The server supervisor owns placement; the monitor only reads it.
Keep server, app runs, cores, and builds in separate leaves. For example:

```text
apteva production service delegated subtree
  server
  apps
    installation 22 run A
    installation 22 run B while upgrading
  cores
    agent 1173 run A
  builds
    installation 91 build A

apteva staging service delegated subtree
  server
  apps
    installation 22 run A

resource monitor service
```

Delegate through systemd rather than writing arbitrary groups in its managed
hierarchy. Leave controller parents without processes, and give each subtree
one writer. The supervisor must preserve existing shutdown, core detach,
upgrade, and reattachment behavior. [Systemd delegation rules](https://systemd.io/CGROUP_DELEGATION/).

Place a newly launched workload in its group before application code executes;
moving just the root PID after launch can miss helpers it already spawned.
On supported Linux hosts, assess Go's `SysProcAttr.UseCgroupFD` and `CgroupFD`
for this launch boundary. Keep existing process-group shutdown behavior.
[Go process launch interfaces](https://pkg.go.dev/syscall#SysProcAttr).

A deployment spike must validate kernel support, delegation, controller
availability, and server re-execution before this backend is enabled. Existing
running processes can retain Process estimate until their next planned launch;
do not restart production telephony merely to improve attribution.

Retain exited-run inventory and empty run cgroups until the collector confirms
a final read or a bounded grace period expires, proposed as 60 seconds. This
lets the monitor recover total CPU for helpers or builds that finish between
polls. A freshly created empty group can provide a known zero counter origin;
an adopted group cannot. Mark first and final partial intervals explicitly,
and never manufacture a one-second peak from lifetime CPU. If cleanup wins the
race or the collector is down beyond the grace period, report incomplete
accounting. Group cleanup must remain bounded when the monitor is unavailable.

For containers, use the runtime's existing workload cgroups where a container
matches an app installation. A combined server container needs a supported
delegated subtree or remains at container/process granularity. Do not require
a privileged container or a public container-engine socket. Namespace and PID
translation must be explicit in the inventory contract.

## Metrics and definitions

Every sample carries UTC observation time, actual interval length, workload
identity, source, and quality. Measure elapsed intervals with a monotonic
clock. Store counter deltas before converting them into rates; never infer a
five-second interval merely because five seconds was scheduled.

| Metric | Display and purpose |
| --- | --- |
| CPU across VPS | Percentage of total online logical CPU capacity consumed during the interval |
| CPU cores used | CPU seconds divided by elapsed seconds, useful for local diagnosis |
| CPU peak | Largest observed one-second average within the selected window |
| Memory current and peak | Bytes attributed by the backend, with the memory definition displayed |
| CPU throttling | Throttled time and throttled period count deltas when supported |
| CPU pressure | Time tasks waited for CPU; host and workload views when supported |
| Memory and I/O pressure | Time work stalled on those resources |
| Disk activity | Attributed read/write bytes per second and operation counters |
| Process count | Run subtree population and growth |
| Restarts and exits | Supervisor lifecycle counts, with exit reason when available |
| Host context | Available memory, swap, CPU steal, CPU I/O wait, and host disk usage |

CPU across VPS is calculated as:

```text
CPU percent = 100 × CPU seconds consumed / host CPU capacity seconds
host CPU capacity seconds = integral of online logical CPU count over interval
```

With four logical CPUs online for ten seconds, consuming 1.2 CPU seconds means
3.0% of VPS capacity. One core busy continuously represents 25%. Host capacity
is distinct from an app's quota: display quota utilization separately, when a
quota exists. Record CPU-count changes so historical percentages keep their
original denominator. Window averages use summed CPU and capacity seconds,
rather than averages of already rounded percentages.

Host CPU steal and I/O wait are separate context; neither is app CPU. Memory
from a cgroup includes charged memory and is not interchangeable with a sum of
RSS values. Disk bytes measure block activity, not all logical file operations.
Network bytes require separate instrumentation and are deferred.

Pressure Stall Information provides host and cgroup measurements of resource
waiting. Present CPU pressure separately from usage and quota throttling.
Ignore host CPU `full` as a diagnostic measure. Retain cumulative pressure
counter deltas as well as available rolling averages. [Linux PSI definitions](https://www.kernel.org/doc/html/latest/accounting/psi.html).

A low one-minute CPU average does not establish that scheduling was healthy.
Likewise, a one-second peak does not reveal the exact timing of a millisecond
stall or prove the cause of an audio failure.

## Sampling and incident correlation

Use a proposed default of one-second collection for host and tracked workload
counters. Persist five-second buckets containing CPU deltas, elapsed capacity,
latest and maximum memory, pressure deltas, and maximum observed one-second
CPU rate. Batch database writes; dashboard polling never triggers sampling.

Keep a two-minute in-memory buffer of one-second samples. A supported pressure
trigger or transport-incident notification can retain that buffer and the next
minute as an incident window. Bound capture frequency and disk usage so an
error burst does not multiply storage without limit. Add this incident capture
after routine collection is qualified; missing captures must be visible.

The server may send a small asynchronous incident hint to the monitor with
timestamp and permitted workload identifiers. This hint carries no resource
measurements, payloads, tokens, or audio. It must never block a proxy or agent
event path. Transport events remain durable through their existing mechanism.

An incident page joins existing transport events with resource samples using
host, instance, time, and known participating app/core runs. Call IDs stay in
incident metadata, not permanent metric series labels. A call can involve the
telephony app, server, and agent core; show those participants separately.
Display clock health and collection gaps. Correlation identifies candidates
for investigation; it does not label a resource spike as a proven cause.

## Inventory and query contracts

Add a private, read-only inventory endpoint per server instance. The monitor
uses a dedicated credential restricted to that endpoint, or an authenticated
Unix socket. Return complete snapshots plus a revision, and optional lifecycle
updates; poll every ten seconds for recovery even when updates are available.
Never expose application environment variables, credentials, or arbitrary
process command lines.

An inventory record should include these proposed fields:

```text
host_id, server_instance_id, workload_id, run_id, kind
install_id or agent_id, project_id, environment_id
display_name, version, lifecycle_state
pid, process_start_identity, host_boot_id, cgroup_path if available
started_at, stopped_at, metadata_revision
```

The collector caches identity independently of server availability, checks it
against the OS, and marks inventory stale when updates fail. It can continue
measuring known cgroups while the server is down. It must not accept an app's
self-reported installation ID as accounting authority.

Expose bounded read APIs through the server query adapter. Proposed routes:

| Route | Purpose |
| --- | --- |
| `GET /api/resources/status` | Collection mode, capabilities, freshness, monitor health |
| `GET /api/resources/contributors` | Authorized workload rows for host, project, environment, and time range |
| `GET /api/resources/history` | One authorized workload or grouped scope at a requested resolution |
| `GET /api/resources/incidents` | Authorized resource incidents and links to transport incidents |

Responses identify requested and actual resolution, sample coverage, source,
staleness, and denominator. Unsupported values are null with a reason. A
monitor outage returns an explicit unavailable status, not zero usage.
Only administrator responses include host-wide denominators and context.
Validate filters, cap returned rows and time ranges, and enforce timeouts.
Proposed initial limits are 1,000 contributor rows and 10,000 history points.

## Dashboard behavior

Add Resources under Monitor. Platform administrators can select a host and see
the whole VPS. Project viewers can see resources for workloads in their
authorized project, including explicitly permitted environments. Reuse server
authorization helpers; a client-provided project filter is not an access check.

The default administrator view shows the previous minute's weighted CPU
average, RAM now, peak CPU, restart count, and collection freshness. Provide
Last hour, Last 24 hours, and custom interval controls, plus a production/staging
filter and grouping by app installation or project. Expand an installation to
inspect its active and retiring runs. Refresh live views every five seconds.

The following CPU values illustrate the requested presentation; they are not
measurements of the current VPS:

| Contributor | CPU across VPS over previous minute | RAM now | CPU peak over previous minute |
| --- | ---: | --- | --- |
| Production telephony | 3.0% | measured value | measured one-second peak |
| Staging Tables | 1.1% | measured value | measured one-second peak |
| Apteva server | 0.7% | measured value | measured one-second peak |
| Production Tables | 0.5% | measured value | measured one-second peak |
| Production Functions | 0.2% | measured value | measured one-second peak |

Include agent cores, builds, the monitor, and Other host activity when present.
Do not present the example's five rows as an exhaustive VPS accounting total.
Project viewers do not receive other projects' names, raw host process data,
shared-server usage, or global contributor rankings. Host-wide context and
denominators are administrator-only in the first release; project views default
to cores used and memory for their permitted workloads.

Workload details show CPU, memory, and pressure history, version changes,
restarts, and transport-error markers. A marker opens the existing error's
timestamp, connection/call correlation, and nearby resource window. Static and
shared workloads show their attribution limitation instead of an invented row.
Display Stale, Unknown, Stopped, Unsupported, and Process estimate explicitly.

## Storage and retention

Use a separate SQLite database owned only by the monitor, with batched writes
and bounded query execution over its private socket. The server never opens
this database directly. Separate run metadata, raw buckets, rollups, lifecycle
records, and incident windows; index by workload and time. Keep host samples in
their own scope. Existing telemetry tables remain unchanged.

Proposed defaults are:

| Data | Retention |
| --- | --- |
| Five-second buckets | 24 hours |
| One-minute rollups | 7 days |
| Fifteen-minute rollups | 90 days |
| One-second incident captures | 7 days, within a separate capture budget |
| Lifecycle and historical labels | At least as long as associated retained measurements |

Roll up counters by summing deltas and elapsed capacity; retain maxima and
sample coverage, not just averages. Mark incomplete buckets. Never bridge a
missing interval with a fabricated rate or join two process generations.

Set a proposed total disk budget of 1 GiB, with 128 MiB reserved within it for
incident captures. Enforce the budget even if configured retention has not
expired; evict older data and report the actual available history. Bound the
database, WAL, temporary operations, and in-memory queues. Preserve aggregate
history before discarding detailed data where possible.

At 30 workload/host series, five-second persistence produces approximately
518,400 rows per day before rollups. Storage bytes per row depend on schema and
indexes; measure them in the deployment spike. Retention defaults and the disk
budget are design targets, not a promise that every host fits full retention.

## Access and operational behavior

Run the collector under a dedicated identity with the file visibility required
for collection. Probe capabilities at startup and expose missing access. Keep
cgroup writes in the supervisor's delegated subtree and keep the collector's
query socket private. A system-wide collector must isolate each server instance's
query principal; no instance may query another instance's workloads by guessing IDs.
Only an explicitly configured host-administrator principal gets global views.

Apply project access checks on every dashboard request, including historical
queries and incident links. Deleted projects and changes in membership must
not preserve access through cached resource labels. Avoid storing secrets,
message content, audio, full command lines, or environment variables.

Collection is optional and fails independently. A slow or unavailable monitor
does not delay app startup, HTTP/WebSocket proxying, or agent telemetry. Failed
optional cgroup placement falls back to a visibly degraded accounting mode
where safe; it must not silently change lifecycle semantics.

Resource alerts start as dashboard incidents only. Suggested defaults are
host CPU above 85% for five minutes, an OOM event, or repeated unexpected app
exits. Calibrate memory and pressure alerts against deployment baselines and
active telephony incidents. Deduplicate, record recovery, and distinguish
process exits from deliberate upgrades. Do not send external notifications or
automatically throttle, restart, or kill workloads in this release.

## Delivery plan

| Phase | Deliverables | Completion gate |
| --- | --- | --- |
| 1 Host and launcher qualification | Confirm deployed process/container layout, kernel counters, inventory identities, storage cost, and access model | Document supported accounting mode and lifecycle constraints on the target host |
| 2 Process accounting | Independent binary/service, full runtime inventory, process backend, separate database, bounded query adapter | Correct contributor attribution, restart handling, gaps, and project isolation |
| 3 Dashboard | Contributor table, filters, history, quality states, lifecycle markers | Requested production/staging view works with real measurements |
| 4 Cgroup accounting | Delegated placement before launch, descendant accounting, build and upgrade runs, reattachment | Native service and supported container modes pass lifecycle and attribution checks |
| 5 Incident correlation | PSI, transport overlays, bounded capture windows, dashboard alert state | Operators can inspect a failure's resource window without blocking transport |

A useful first release is phases 1 through 3 with its Process estimate label.
Reliable Linux cgroup accounting and pressure correlation complete the proposed
production design. Qualify production telephony first and staging workloads
alongside it, then extend to all apps and agent cores.

Add the independent binary to local/release builds and applicable container
packaging. Update the CLI service installer for optional monitor installation;
do not make collection a hidden dependency of `apteva-server`. Dashboard
changes require the existing embed build and asset sync before server rebuilds.

Roll out the process collector first without changing workload placement.
Observe coverage and overhead for 24 hours. Enable cgroup placement for staging
and newly launched workloads after qualification, then production during normal
maintenance. Rollback disables collection/query features and preserves app
runtime behavior. Removing delegated placement requires a validated transition
plan; do not delete live workload groups.

## Validation and acceptance criteria

- On a fixed four-CPU test host, a workload consuming one CPU second per second
  reports approximately 25% VPS CPU over a steady window. Test time-weighted
  denominators and CPU-count changes with deterministic counter fixtures.
- An app and a busy helper are one installation total; server CPU excludes both.
  Static UI requests and shared worker CPU remain shared, as specified.
- An upgrade reports both live generations without counting either twice.
  Short-lived helpers, builds, crashes, PID reuse, and host reboot are exercised.
- Production and staging identities remain separate even when numeric install
  IDs match in different server instances. Renames retain historical identity.
- Stopped, missing, denied, and unsupported samples are distinguishable. After
  collector downtime, chart gaps remain gaps and recovery establishes a fresh
  counter baseline where needed.
- Cgroup launch and server re-execution preserve existing signal handling,
  detach policy, blue-green upgrades, and surviving-core reattachment.
- Project viewers cannot enumerate or query other workloads through IDs,
  history, exports, incident links, or collector query parameters. Host views
  remain restricted to the configured administrator principal.
- Resource histories and transport events align within reported collection
  resolution. Injected pressure produces a retained window without blocking
  call handling; missing notification/capture is reported.
- Disk budgets, rollup arithmetic, incomplete coverage, and query limits hold
  under a high-workload-count and error-burst test. Metric label cardinality
  does not grow with call, connection, or thread IDs.
- Benchmark the collector at representative idle and busy load. Initial targets
  are under 1% of total VPS CPU and under 100 MiB RSS at 30 tracked workloads;
  these are acceptance targets to verify, not measured performance claims.
- Compare telephony latency and termination behavior with collection enabled
  and disabled. Do not accept a measurable regression solely to obtain metrics.

## Decisions to settle in qualification

The preferred choices are a Go host service, separate SQLite storage, existing
dashboard authorization, an initial process backend, and delegated cgroups for
Linux production. Qualification must settle the deployed native/container
layout, whether production and staging share the host, supported delegation
and kernel launch interfaces, read permissions, measured overhead, and actual
storage cost. These determine backend support and rollout configuration.

Resource limits, automated workload remediation, per-call CPU allocation,
network traffic accounting, and a fleet monitoring backend remain later work.
The proposed contributor table and incident history do not depend on them.
