# Proposal: Claude Code as an Apteva runtime provider

Status: proposed

Scope: `server`, `core`, and the provider settings in `dashboard`

Provider key: `claude-code` (display name: **Claude Code**). The existing
`anthropic` provider continues to use an Anthropic API key.

## Decision

Offer Claude Code as an explicit, local provider choice. Apteva launches the
unmodified `claude` executable. Claude Code owns its login, token refresh,
conversation session, and internal model turns. Apteva owns agent admission,
inbox and channel delivery, tool policy, project scope, telemetry, and
cancellation.

Server owns a small host-level pool of warm Claude Code processes. A process
is assigned to one compatible Apteva thread at a time, and a saved Claude
session ID lets that thread resume after its process is evicted. Recently
active threads may retain a process for a short idle TTL; long-idle agents
consume none. The pool has a configurable process and memory budget; it does
not spawn one process per agent or per turn.

The first release targets a single-user installation where Server and Core run
under the same OS account as the person who signs in to Claude Code. It is
limited to text turns and Apteva tools. A hosted, multi-user release requires
per-user process and home-directory isolation plus an explicit review of the
Anthropic terms for that deployment.

This is a runtime provider, even though the UI presents it alongside model
providers. Core's current `LLMProvider.Chat` contract returns tool calls for
Apteva's Thinker to execute. Claude Code already owns an agent loop and
executes MCP calls within it. Treating it as a plain HTTP model adapter would
duplicate history and tool execution.

### Comparison with Hermes Agent

Hermes has an official **experimental** Claude subscription plugin that takes
a different approach. It starts a fresh unmodified `claude` process for each
model call, replays canonical history into that process, advertises Hermes
tools through an inert MCP server, extracts Claude's proposed tool calls from
the structured output, then lets Hermes execute them in its existing loop.
There is no parked Claude session. Hermes reports qualification against
Claude Code 2.1.263 and describes version-sensitive replay behavior. Its
request-scoped local relay forwards native authorization headers in memory
to admit exactly one upstream request per model call.

That design fits Core's existing `LLMProvider.Chat` contract, but it pays
process startup and history replay costs on every model call and depends on
native protocol details that need validation for each supported CLI version.
Its relay also needs a separate review against our no-token-intermediation
boundary. The protocol spike should measure this one-shot adapter alongside
the warm runtime before finalizing the implementation path. The warm runtime
remains the preferred design if it can enforce turn-scoped tool authority.

## User experience

1. Settings shows **Anthropic API** and **Claude Code** as separate choices.
   Claude Code shows the detected binary version and `ready`, `not signed in`,
   `missing binary`, or `unsupported version` status.
2. **Connect** starts Claude Code's own `claude auth login` flow on the same
   host and shows its progress. Server polls `claude auth status --json` to
   confirm completion. If the service cannot present the native flow, the UI
   shows the exact command to run and continues polling. Apteva does not run
   an independent Anthropic OAuth exchange.
3. A user selects a Claude model alias for an agent. The provider is explicit;
   signing in does not silently change existing agents or their defaults.
4. **Disconnect from Apteva** removes the provider selection. It does not run
   `claude auth logout`, which would also sign the user out of their own CLI.
   The UI may offer a separately labelled **Sign out of Claude Code** action
   when the CLI is dedicated to Apteva.
5. An expired login stops the turn with a clear `needs login` state. Apteva
   does not silently switch to API-key billing or retry uncertain tool work.

## Server work

### Provider registration and host checks

- Add `claude-code` to the provider catalog and provider selection path in
  `server/providers.go` and `server/new_agent_provider.go`. Represent it as a
  runtime connection without encrypted Anthropic credentials. Keep the
  `anthropic` API-key provider independent.
- Probe the configured executable path, version, and `claude auth status
  --json` from the **same OS identity and Claude config directory** that
  agent Core processes will use. Pin the executable path on the server; do not
  accept a command or path supplied by the browser.
- Mark the provider available only on hosts where Server can launch that
  executable. Do not add it to automatic API-key discovery or automatic
  fallback ordering.
- Initially gate it to single-user local installations. A server with users
  sharing one OS account must not expose the host's Claude login as a provider
  for every user.

### Native login lifecycle

- Extend the existing provider-auth API in `server/provider_auth.go` with a
  Claude Code driver. `start` supervises `claude auth login` as a bounded native
  process, `status` asks the CLI for auth state, and `cancel` stops the login
  process. Persist only provider configuration and non-secret status metadata.
  Do not add `runtime-token` or token-refresh operations for this provider.
- Use a terminal/PTY only if the installed CLI requires one. Keep login output
  bounded and redacted before returning it to the dashboard. Recheck status
  after a Server restart rather than assuming an earlier login session remains
  active.
- When `server/instances.go` starts Core, pass only the runtime selection and
  authenticated supervisor endpoint. The Server Claude supervisor launches
  the pinned executable under the appropriate OS identity and
  `CLAUDE_CONFIG_DIR`. Do not pass Claude OAuth credentials through
  environment variables or core config.
- Add a connection smoke check that starts a text-only Claude turn in an
  isolated temporary directory and reports a clear failure if login, model
  selection, or CLI protocol is unavailable. It must not invoke agent tools.

### Bounded Claude process supervisor

- Keep a host-level pool of warm Claude Code subprocesses rather than one
  subprocess per turn or one permanently running subprocess per agent. Start
  with a configurable cap of two processes, an idle TTL, and one active turn
  per process. Measure cold-start latency, resident memory, and resume time
  before choosing production defaults.
- Reuse a warm process only when account, model, system prompt, tool policy,
  and Claude session are compatible. Otherwise retire it, persist its session
  ID, and resume that session on a new process when needed. Evict the least
  recently used idle process when the cap is reached; queue or reject when
  every process is busy rather than exceeding the host budget.
- Expose an authenticated, streaming RunTurn/Cancel boundary to Core. Bind
  each request to the server-known agent and thread, not to client-supplied
  identifiers. Kill the process group on cancellation, deadline, or failed
  cleanup, and never return a dirty process to the pool.

### Dashboard

- Reuse the provider connection and agent model-selection flows, with a native
  login screen instead of an API-key field. State clearly which OS host will
  run Claude Code and whether that host is ready.
- Show Claude Code session usage separately from Anthropic API spending.
  CLI-reported usage can be shown as an estimate; do not present it as an
  Anthropic billing record.

## Core work

### Runtime boundary

- Add a `TurnRuntime` path beside `LLMProvider`. It takes the admitted agent,
  thread, directive, latest input, model, and effective tool policy and emits
  structured text, tool, usage, and terminal events. The Thinker remains
  responsible for scheduling, inbox state, channel delivery, and the final
  visible response.
- Select this path only for `claude-code`; existing providers keep their
  `LLMProvider.Chat` path. The first release applies to ordinary text threads;
  realtime voice and provider built-in tools are out of scope.
- Ask Server's Claude supervisor to run the turn. Parse its structured stream
  into terminal status, session ID, text chunks, tool events, usage, and
  bounded stderr diagnostics. Propagate turn cancellation to the supervisor.
  Server launches `claude -p` with streaming JSON input/output, an explicit
  model, and strict MCP configuration; compatible turns reuse the process.

### Session and history ownership

- Persist a Claude session ID and last admitted input cursor per Apteva
  thread. Serialize runs for one thread. Resume the Claude session after a
  Core restart; do not resend the complete Apteva transcript on every turn.
- Seed a new Claude session from the current agent directive and a bounded
  transcript when switching into this runtime. Start a new session when the
  effective directive or tool policy changes. Keep Apteva's user-visible
  transcript, but let Claude Code manage its own internal history and
  compaction. Do not run Apteva's context compaction on the Claude session.
- If the CLI exits after a tool may have run but before a terminal result is
  recorded, mark the turn outcome uncertain. Inspect the persisted Claude
  session and tool audit before retrying; never blindly replay a mutation.

### Apteva tool bridge

- Core exposes a loopback MCP endpoint for admitted Claude turns. A random
  process capability is active for only the currently admitted run and bound
  to its agent, thread, project, and exact effective tool allowlist. Reuse of
  a warm process must not carry authority between turns; the protocol spike
  must prove that stale or background calls are rejected before enabling
  warm reuse. `tools/list` exposes only the active allowlist;
  `tools/call` rechecks it and invokes Core's existing tool dispatcher so
  approvals, app scopes, telemetry, and result handling remain authoritative.
- Generate a private, mode-0600 MCP config for the CLI and remove it when
  its process exits. The capability is inactive between turns and expires
  on cancellation, timeout, or process retirement. Never trust
  caller-supplied agent or project headers from the CLI process.
- In the first release, disable Claude Code's native shell, file-editing,
  browser, and other built-in tools. Use `--strict-mcp-config` and an isolated
  settings policy so user/project MCP servers, hooks, plugins, skills, and
  local customizations cannot widen the Apteva tool set. Verify the exact
  behavior with the supported CLI version; `--bare` is unsuitable for a
  subscription login because it disables native OAuth/keychain access.
- Emit the same `tool.call` and `tool.result` telemetry shape used by ordinary
  Core tools. The UI must show a pending tool, its result, and failure reason
  even though Claude Code performs the internal agent loop.

## Delivery order

1. **Protocol spike:** On the current local host, verify native login from
   the Server service identity, multiple turns in one warm process, structured
   streaming, session resume after process exit, exact MCP tool restriction,
   and cancellation. Measure startup, resident memory, and follow-up latency
   against a Hermes-style one-shot adapter. Stop if subscription login cannot
   be combined with the required tool isolation and turn-scoped authority.
2. **Server:** Add native login/status, provider discovery, provider selection,
   and the bounded process supervisor.
3. **Core:** Add turn runtime, persisted Claude session mapping, scoped MCP
   bridge, telemetry, and uncertain-result handling.
4. **Dashboard:** Add Connect/status and model-selection UI; enable only after
   the server and core protocol versions agree.
5. **Local rollout:** Opt in one local user and one agent before making the
   option generally visible. Keep hosted multi-user mode disabled pending
   isolation and Anthropic review.

## Acceptance criteria

- A user signs in through Claude Code and Apteva reports readiness without
  receiving or persisting an Anthropic OAuth or refresh token.
- A selected agent answers a text message, calls an allowed Processes MCP
  tool, and shows the tool activity in Apteva. A disallowed tool and a
  cross-project call are rejected at the bridge.
- A follow-up uses the same Claude session after a Core restart without
  duplicating the previous user message or tool action.
- Repeated turns on one active thread reuse a warm process. After the idle
  TTL, its process exits while its Claude session remains resumable. At the
  configured pool cap, additional agents queue or receive a clear capacity
  error; memory stays within the chosen budget.
- Cancellation and deadline stop the Claude process and invalidate its MCP
  grant. Login expiry produces a `needs login` error. No fallback changes the
  billing route without explicit configuration.
- A second Apteva user on a shared host cannot see or select the first
  user's native Claude login.

## Open questions and external constraints

- Confirm the minimum Claude Code version whose structured stream, strict MCP
  config, settings isolation, and login behavior satisfy the protocol spike.
- Decide whether a later coding mode should allow native filesystem/shell
  tools under a workspace sandbox and Apteva approvals. It is not needed for
  the initial Apteva-tools runtime.
- Confirm with Anthropic that the intended hosted deployment fits its terms
  before enabling multi-user subscription use. Anthropic's published guidance
  permits end users to sign in to an unmodified Claude Code binary, including
  on a hosted platform, and prohibits third-party handling or routing of
  subscription credentials on users' behalf.

## References

- Anthropic Claude Code legal and compliance:
  https://code.claude.com/docs/en/legal-and-compliance
- Anthropic Claude Code CLI reference:
  https://code.claude.com/docs/en/cli-reference
- OpenClaw Claude CLI backend and MCP bridge:
  https://docs.openclaw.ai/gateway/cli-backends
- OpenClaw Anthropic provider route:
  https://docs.openclaw.ai/providers/anthropic
- Hermes experimental Claude subscription provider:
  https://github.com/NousResearch/hermes-plugin-claude-subscription-directsdk
