# Server-owned agent behavior

The server persists `autonomous`, `cautious`, or `learn` in `agents.mode` and a separate integer `agents.proactivity` from 0 to 100 (default 25, Conservative). Core receives an ordinary directive with one versioned, server-managed behavior section. The server adds mode, proactivity, and `behavior_sync` to config/status responses; it does not depend on core returning a mode. Nested `execution_control.mode` remains independent.

The section is bounded by `<!-- apteva:behavior:v1:start -->` and `<!-- apteva:behavior:end -->`. Changes replace existing complete sections, remove duplicates, and preserve text outside those boundaries. Mode changes read the latest live/disk directive unless a previous server edit is still pending. Explicit directive replacements remain replacements. Creation, MCP tools, templates, presets, runtime drafts, environment clones, and platform helpers use the same policy.

- Autonomous instructs independent work within scope and permissions, respecting explicit approval requirements.
- Cautious instructs asking and waiting before state changes; read-only work can proceed within scope.
- Learn instructs asking for unfamiliar tool/scope combinations, including reads, and reusing approvals available in context. There is no dedicated learn-mode safety-profile store.

These are model instructions, not enforced approval gates. Existing server access-policy restrictions still apply.

## Persistence and migration

`agent_behavior_state` tracks desired, main-applied, and fully-applied revisions. SQLite triggers update the desired revision in the same transaction as a directive, mode, or proactivity change, including audited app directive writes. Per-agent configuration locks serialize edits, startup preparation, and stop-time config flushing.

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


## Agent-wide proactivity

Proactivity controls unrequested initiative toward standing goals across the agent and its ordinary worker hierarchy. It is independent of conversations, apps, integrations, user presence, tool access, approval mode, and execution-control pauses. It does not grant authority or budget.

| Value | Label | Initiative |
|---|---|---|
| 0 | Reactive | No unsolicited work or exploratory wakeups. Respond to events and complete assigned responsibilities. |
| 1–25 | Conservative | Only tiny adjacent actions whose need and exact correction are known; no unsolicited investigation. |
| 26–50 | Balanced | Investigate a specific observed problem with clear benefit and complete one bounded improvement; skip weak leads. |
| 51–75 | Proactive | Validate one existing concrete but uncertain lead and, when evidence warrants, run a small reversible experiment; no discovery without a lead, even if bounded. |
| 76–100 | Highly proactive | Review unexamined areas without needing an existing lead, compare opportunities, and coordinate useful initiatives. |

At every level, including zero, explicitly requested recurring work, monitoring, necessary retries, and task completion continue subject to approval policy. Startup or a timer event does not itself authorize invented work. At 100, the agent should still sleep when nothing useful remains. The value is an initiative scale, not a percentage of messages, tool calls, time, or spending. It supplies qualitative guidance within the bands, not an enforced wake interval or execution budget.

`POST /agents` (also `/instances`) accepts optional `proactivity`; omission defaults to 25. `PUT /agents/{id}/config` accepts an integer from 0 to 100; omission preserves the current value and explicit zero is retained. List/detail and config/status responses expose the stored value. Environment clones inherit it; other creation paths default to Conservative unless a value is supplied to the store. Dashboard creation and settings both expose the control independently of safety mode.

Schema migration 4 adds the checked integer column with default 25 for existing agents. Behavior policy version 3 sharpens the initiative bands using the existing v1 section delimiters and reconciliation machinery, preserving evolved mission text. Schema migration 5 updates the revision trigger; older policy revisions are reconciled on upgrade. Desired changes survive failures/restarts and synchronize to ordinary saved/live workers. System memory workers remain excluded. Model-created workers receive an instruction to inherit the setting; inheritance and resistance to policy edits are not hard-enforced by core. The server DB is authoritative and recomposes the managed section on configuration updates/reconciliation/startup. No core changes are needed for this version.

## Codex scenario evaluation

Run deterministic storage/API/reconciliation tests:

```sh
go test . -run 'TestAgentBehavior|TestAgentProactivity' -count=1
```

Run the opt-in evaluation with real Codex LLM calls (requires an existing Codex login in `$CODEX_HOME/auth.json`, default `~/.codex/auth.json`, or `OPENAI_CODEX_ACCESS_TOKEN` and optionally `OPENAI_CODEX_ACCOUNT_ID`):

```sh
RUN_CODEX_PROACTIVITY_EVAL=1 go test . -run '^TestAgentProactivityCodexScenarios$' -v -count=1 -timeout 20m
```

`APTEVA_PROACTIVITY_EVAL_MODEL` overrides the default `gpt-5.6-sol`. Use a subtest suffix such as `/reactive_recurring` to run a single case. Twelve scenarios cover reactive idle/startup, requested actions and due recurring work at zero, Conservative follow-ups versus speculation, Balanced/high initiative, cautious/learn approval boundaries, exhausted opportunities, and scope restrictions.

Each case creates an isolated test server/store, submits the setting through its config API, and sends the resulting saved directive to the real Codex provider. The model chooses a structured tool action; all tools are simulated, so no application or external state is changed. Logs record model, policy, selected action, and token counts without credentials. These are bounded policy evaluations, not end-to-end core scheduling tests or guarantees of model behavior. Ordinary tests skip real calls unless explicitly enabled, and `-short` always skips them.


For a controlled level comparison, add `RUN_CODEX_PROACTIVITY_MATRIX=1` and `-parallel 3`:

```sh
RUN_CODEX_PROACTIVITY_EVAL=1 RUN_CODEX_PROACTIVITY_MATRIX=1 go test . -run '^TestAgentProactivityCodexScenarios$' -json -count=1 -parallel 3 -timeout 20m > proactivity-evaluation.jsonl
```

This adds 75 decisions: the same five situations at 0, 25, 50, 75, and 100, repeated three times each. Together with the twelve boundary scenarios, that is 87 real model calls. Server setup is serial; only model requests run concurrently. The five situations are a small confirmed follow-up, a bounded opportunity, a weak lead, nothing worthwhile remaining, and an explicit assignment. Approval mode remains autonomous for the comparison. Exploration choices are recorded rather than forced to a preselected answer above zero; invariant checks still require no unsolicited initiative at zero, completion of explicit assignments, and waiting when nothing useful remains. Three repetitions are a small behavioral sample, not a statistical guarantee or a test of ongoing core wake scheduling.


The richer capabilities evaluation compares the server policy alone against main and leaf-worker prompt constants read directly from core source (read-only). It supplies operational tools for record inspection, discovery, hypothesis validation, experiments, fixes, approval, pacing, and delegation. Ninety choices cover three opportunity types, five levels, three prompt contexts, and two repetitions. Its assertions test the new band boundaries. It does not execute the actions or the core loop, and omits dynamic provider catalogs and full runtime history.

```sh
RUN_CODEX_PROACTIVITY_CAPABILITIES=1 go test . -run '^TestAgentProactivityCodexCapabilities$' -json -count=1 -parallel 3 -timeout 20m > proactivity-capabilities.jsonl
```

The source directory defaults to `../core`; override with `APTEVA_PROACTIVITY_CORE_DIR`. A source hash is recorded to identify the exact core prompt snapshot. The test reads core files and never modifies them.

Core read-only findings are recorded in [proactivity-core-directive-review.md](proactivity-core-directive-review.md).
