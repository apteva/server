# Server-owned agent behavior

The server persists `autonomous`, `cautious`, or `learn` in `agents.mode`. Core receives an ordinary directive with one versioned, server-managed behavior section. The server adds mode and `behavior_sync` to config/status responses; it does not depend on core returning a mode. Nested `execution_control.mode` remains independent.

The section is bounded by `<!-- apteva:behavior:v1:start -->` and `<!-- apteva:behavior:end -->`. Changes replace existing complete sections, remove duplicates, and preserve text outside those boundaries. Mode changes read the latest live/disk directive unless a previous server edit is still pending. Explicit directive replacements remain replacements. Creation, MCP tools, templates, presets, runtime drafts, environment clones, and platform helpers use the same policy.

- Autonomous instructs independent work within scope and permissions, respecting explicit approval requirements.
- Cautious instructs asking and waiting before state changes; read-only work can proceed within scope.
- Learn instructs asking for unfamiliar tool/scope combinations, including reads, and reusing approvals available in context. There is no dedicated learn-mode safety-profile store.

These are model instructions, not enforced approval gates. Existing server access-policy restrictions still apply.

## Persistence and migration

`agent_behavior_state` tracks desired, main-applied, and fully-applied revisions. SQLite triggers update the desired revision in the same transaction as a directive or mode change, including audited app directive writes. Per-agent configuration locks serialize edits, startup preparation, and stop-time config flushing.

Existing rows start at policy version zero. Migration preserves their latest disk/live directive and uses their server mode. Startup handles stopped instances; reattach reads live configuration and updates only MCP URLs before behavior reconciliation. Re-running migration does not append another section. Pending server edits take precedence over stale disk content after a failed update/restart.

A cancelable reconciliation loop runs at startup and once per minute, selecting only agents with pending revisions or migration. Healthy cores receive no additional polling from this loop. It skips running database rows with no attached core, because their files may still belong to a surviving process. Stopped files use temporary-file/rename writes. Invalid saved JSON fails instead of discarding existing configuration.

Config PUT returns the core error if main cannot be updated; the desired state remains persisted. A successful main update with unfinished worker updates returns HTTP 202 and `behavior_sync.pending=true`. Stopped changes are fully saved for the next start. `behavior_sync` exposes revision numbers and the last reconciliation error.

## Workers and real time

Existing ordinary workers receive their own directive plus the behavior section through `PUT /threads/{id}`. Tools, MCP permissions, history, and unrelated instructions are preserved. System memory workers are excluded. Core may wake an ordinary worker after a directive update.

Server-created explicit worker/voice directives receive the selected behavior section; suffix-based app workers already inherit main's directive. Model-created workers require main to propagate the applicable instructions; this guidance is included in the managed section. The server cannot enforce instruction inheritance for model-internal spawns.

Active voice sessions that reject prompt changes with HTTP 409 remain running. The revision stays pending and is retried; the server does not force a voice restart. Multi-worker application is not atomic, and work already underway is not retracted. Server locks serialize server edits, but core has no compare-and-swap API for racing core-originated `evolve` changes.

The event/SSE path, real-time audio transport, snapshot cadence, and cache strategy are unchanged. Configuration changes rebuild the relevant prompt as required. A small companion core fix services queued runtime mutations at paused execution gates without releasing the gate; integration coverage exercises mode changes while paused and after stop/restart.

## Release dependency

Ship with the mode-free core, including the paused-mutation fix. Older core versions can still inject their own legacy mode instructions. No deployment, service restart, or production migration was performed while implementing this change locally.
