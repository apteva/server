# Agent process CPU priority

Agent CPU weight uses the same process controls and SDK policy types as app
resource settings. It is Server-owned configuration, stored separately from Core
model/config JSON, and never an agent tool permission. No Core or SDK release is
required. Administrator-only `GET/PUT /api/agents/:id/resources` (also available
through the existing instances alias) uses `{revision, policy}` compare-and-swap:

```json
{"revision":0,"policy":{"process":{"cpu_weight":200}}}
```

Only CPU weight is accepted for agents in this increment. Reset with an empty
policy. Saving does not restart or migrate a process; it applies on the next
controlled Core restart. Changes to ordinary agent configuration cannot alter
this policy. Environment agent copies start with their own default policy.

The dashboard exposes CPU priority in agent settings for administrators, reuses
the process resource field editor, and separates requested settings from observed
OS values. Diagnostics include available `cpu.stat`, CPU pressure and ancestor
quotas. These are snapshots, not a new sampling service or stored CPU history.

## Linux setup

Set `APTEVA_CGROUP_ROOT` to an administrator-delegated cgroup v2 subtree with the
CPU controller enabled in `cgroup.subtree_control`. Keep Server outside this
workload subtree, normally in its own service/leaf group. This opt-in creates one
sibling cgroup per app or Core execution, even when its policy is empty (baseline
CPU weight 100). An explicit agent weight such as 200 changes its relative share
under contention. The same parent is necessary for meaningful sibling weights;
ancestor quotas still apply. Separately hosted workloads do not share CPU.

The existing `APTEVA_APP_CGROUP_ROOT` remains a compatibility fallback. It keeps
its old explicit-policy behavior: it does not retroactively group ordinary
workloads. Prefer the generic root for comparable app/Core weights. Existing
processes are not moved when either setting changes; restart them deliberately.
No service configuration is changed automatically by Server.

Enforcement uses the existing pre-exec cgroup file descriptor placement. A bad
root, unavailable controller, rejected policy or failed readback fails launch;
Server never claims requested priority was applied. On unsupported hosts such
as macOS, the UI reports unsupported and explicit policies are rejected. Empty
policy on an unconfigured host retains existing launch behavior.

Each group names the workload kind, numeric identity and a unique execution.
Core subprocesses inherit that group. Reattachment reads the surviving process's
actual group without changing it. Kernel process identity protects observations
against PID reuse. Cleanup runs after process exit, including adopted cores;
Server shutdown/detachment does not remove a surviving Core's group.

## Verification

Portable tests cover API permissions, revisions/persistence/reset, ceilings,
unsupported controls, launch failure, agent/app isolation, default compatibility,
process-exit cleanup and adoption. Dashboard tests cover save-only behavior,
unsupported display, stale revisions and changing agents.

On an isolated Linux host with a writable delegated cgroup, run:

```sh
APTEVA_RESOURCE_TEST_CGROUP_ROOT=/sys/fs/cgroup/apteva-test \
  GOWORK=off go test -run 'Test(LocalResourceCgroupBeforeExec|AgentResourceCgroupBeforeExec|ProcessResourceOwnerPrefix)' .
```

A performance qualification must additionally compare weights 100 and 200 under
idle and CPU contention with identical voice traffic. Reuse Core realtime timing
(provider audio to bridge write, queue delays) and measure audio gaps/interruption
latency plus ordinary-worker throughput. Do not infer a voice improvement from a
successful cgroup write. Real Linux enforcement and contention cannot be exercised
by macOS portable tests alone.
