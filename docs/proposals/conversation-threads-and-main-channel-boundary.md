# Proposal: Reliable Conversation Threads and a Private Main Coordinator

Status: proposed

Scope: `core`, `server`, and `dashboard`

## Summary

Every human conversation should have a dedicated core conversation thread. The
agent's `main` thread should be a private coordinator: it owns durable policy,
autonomous work, status, reports, and alerts, but it should not directly write
ordinary messages into Apteva's internal chat channel.

This proposal also closes the lifecycle failures observed in
`conv-96cf247b4ee8078c79f0404d`: API-created chat threads must survive agent
restarts, the server must not trust a stale spawn cache, and asynchronous
handoffs must always return a final result to the originating conversation.

## Goals

- Make chat delivery reliable across refreshes, navigation, disconnects, and
  agent restarts.
- Give each conversation isolated history and tool activity, including the
  primary/default agent chat and the platform-helper chat.
- Keep `main` free of full user conversations while preserving its role as the
  durable coordinator.
- Prevent `main` from calling `channels_send` into `current` or `apteva`.
- Preserve autonomous status, reports, approvals, alerts, and external-channel
  behavior.
- Keep exactly-once visible-message behavior and avoid acknowledgement loops.

## Proposed Runtime Model

```text
Apteva chat message
        |
        v
dedicated conversation thread
        |  direct/read-only work: tools -> channels_send
        |
        |  durable or coordinator-owned work: send(main, request)
        v
private main coordinator
        |  evolve / schedule / act / delegate
        v
send(origin conversation thread, result)
        |
        v
conversation thread -> channels_send -> originating chat
```

The conversation thread is the only owner of the conversational response. Main
never guesses which chat is active and never writes directly into internal
Apteva chat.

Main may still:

- call `channels_set_status` for global agent state;
- publish reports, approvals, and alerts through their dedicated tools;
- perform autonomous work and emit genuinely important notifications;
- answer an originating conversation by calling core `send` with that thread's
  ID.

## User-Visible Handoff Contract

Ordinary chat work produces one final visible message.

Durable handoffs produce at most two visible messages:

1. After `send(main)` returns a successful delivery receipt, the conversation
   sends one short, truthful progress message such as, "I've handed that off and
   am waiting for confirmation." A failed handoff instead produces an immediate
   visible failure or retry result.
2. After main replies, the conversation sends one final outcome.

The progress message should follow the successful handoff receipt, not precede
it. This avoids claiming that work was handed off when delivery failed and uses
core's new immediate send-result continuation. The current preliminary
acknowledgement-before-handoff rule should be removed so the user never receives
three messages.

The send receipt is not completion. Its tool result must retain explicit wording
that the sender must wait for the recipient's response and must not resend.

## Required Reliability Changes

### 1. Persist API-created conversation threads

`core/api.go::spawnThread` currently calls `SpawnWithOpts` without saving the
new thread in `Config.Threads`. Consequently an agent restart loses the chat
thread even though the server and database still reference it.

After a successful non-ephemeral API spawn, core must save the complete
`PersistentThread` state, including:

- ID, parent, depth, directive, and tools;
- MCP names, provider, model, and reasoning;
- `Conversation`, realtime, and voice fields.

If persistence fails, core must kill the just-created thread, unregister any
audio bridge, and return an error. `ephemeral=true` remains intentionally
non-persistent. An idempotent POST for an existing thread must ensure its
persistent record also exists and is current.

On restart, `NewThinker` restores the thread before main can process inbox work,
so a pending main-to-conversation `send` has a valid target.

### 2. Replace blind server spawn caching with ensure-and-retry

`apps/channelchat/handlers.go::spawnedChatThreads` is process-local and currently
outlives a core child restart. Its value is therefore only an optimization, not
proof that the target exists.

Introduce an `EnsureThread` operation with idempotent POST semantics. Before the
first delivery for a process generation, ensure the thread exists. More
importantly, when `/event` returns a missing-thread response:

1. delete the cache entry;
2. ensure/spawn the persisted conversation thread;
3. retry the event exactly once;
4. mark delivery failed and show the existing saved-message notice if retry
   fails.

Use a typed core HTTP error carrying status and response text rather than string
matching. Do not retry authentication failures, invalid requests, or arbitrary
5xx errors as if they were missing threads.

Optionally key the optimization cache by an explicit agent process generation,
but correctness must come from idempotent ensure plus one bounded retry.

### 3. Keep immediate tool-result continuation bounded

The completed core change makes `send` receipts and correctable failures request
an immediate next LLM turn. Preserve these invariants:

- one successful receipt-processing turn;
- at most one correction attempt for invalid arguments or a missing target;
- no hot loop from consecutive `send` calls;
- recipient work remains asynchronous;
- an incoming recipient reply independently wakes the sender.

The same completion rule applies to successful and already-current `evolve`
results so main cannot sleep before replying to a waiting conversation.

## Remove Internal `channels_send` From Main

### Phase 1: hard server-side authorization

Core already injects `_apteva_caller_context` after model-visible telemetry. The
Channels MCP server must use that trusted value as a capability boundary.

For `channels_send` with `channel=current` or `channel=apteva`:

- require a non-empty caller context identifying a live conversation thread;
- resolve the target exclusively from that conversation context;
- reject `caller_context=main` with a tool error explaining that main must
  `send` the result to the originating conversation thread;
- reject missing, deleted, or mismatched conversation contexts;
- never fall back to the primary/default chat.

This must be enforced in `server/channel_mcp.go`, not only in prompts. Prompt
guidance remains useful, but it is not an authorization boundary.

During this phase, main may retain `channels_send` for explicitly addressed
external channels if required for compatibility. Calls to internal Apteva chat
are impossible.

### Phase 2: remove the conversational tool from main's projection

Split conversational and broadcast capabilities so main no longer sees an
internal reply tool at all:

- conversation threads receive `channels_send` scoped to their conversation;
- main receives status/report/approval/alert/publish capabilities;
- external direct conversations also move to per-peer/per-room conversation
  threads, which own their corresponding reply capability;
- once external routing is migrated, remove `channels_send` from main entirely.

This is preferable to a prompt-only rule because it makes cross-conversation
delivery structurally impossible.

## Server Routing Changes

- Stop routing `defaultChatID(agentID)` to `main`; lazily assign it a persisted
  `chat-<conversation-id>` thread like every other conversation.
- Stop keeping platform-helper chats on `main`. Each helper session gets a
  project-scoped conversation thread; the helper's main remains its private
  coordinator.
- Preserve project context when spawning helper conversation threads and when
  listing or invoking project apps.
- Continue storing chat messages in the channel-chat database. Core session
  history is execution context, not the user-visible source of truth.
- Continue enforcing one active SSE connection per browser tab; transport
  connection state must not choose the core thread or response destination.

Existing chats require no destructive migration. `EnsureChatThread` can assign
their thread IDs lazily on first use. The rollback flag may temporarily restore
legacy routing during rollout, but new tests must exercise the per-thread path.

## Main-Thread History Policy

Main will still receive concise durable requests and return results. That is
coordinator work, not conversational pollution. The full transcript, UI events,
typing state, and repeated acknowledgements must remain in the conversation
thread.

Handoffs should contain:

- the durable instruction or requested coordinator action;
- the originating conversation thread ID supplied by the event envelope;
- a request for one authoritative result;
- no copied conversation history unless essential.

Main's reply goes only to the originating thread. It must not acknowledge the
same result through internal Channels tools.

## Test Plan

### Core deterministic tests

- API-spawned non-ephemeral conversation persists and restores after restart.
- Ephemeral API thread does not persist.
- Persistence failure rolls back the live spawn.
- Successful `send` receipt triggers exactly one immediate continuation even
  with a six-hour configured pace.
- Failed `send` gets one bounded correction turn.
- Successful and already-current `evolve` trigger one completion turn.

### Server integration tests

- Default chat, custom chat, and platform-helper chat all target dedicated
  conversation threads, never `main`.
- Stop/start an agent between child-to-main handoff and main's reply; the final
  reply still reaches the original conversation.
- Simulate a stale spawn cache and a core missing-thread response; server
  ensures the thread and retries the event exactly once.
- `channels_send(current|apteva)` from `main` is rejected.
- The same call from a valid conversation thread reaches only its conversation.
- Deleted or forged conversation contexts are rejected.
- Main retains status, report, approval, alert, and publish capabilities.
- External-channel compatibility remains unchanged during Phase 1.

### Real-LLM release smoke

Using Codex and a deliberately long thread pace, require this ordered trace:

1. user sends an unhinted durable instruction;
2. conversation calls `send(main)` exactly once;
3. successful receipt immediately triggers another conversation LLM turn;
4. conversation sends one visible waiting acknowledgement after that receipt;
5. main evolves or recognizes the directive as already current;
6. main sends one authoritative result to the originating thread;
7. conversation sends one final visible confirmation;
8. no duplicate channel message, resend, parent acknowledgement, or premature
   completion occurs;
9. repeat with an agent restart inserted after step 2.

### Dashboard tests

- Chat activity displays only tool calls/results for the selected conversation
  thread.
- The handoff receipt may render as "Waiting for agent confirmation" without
  creating an additional database message.
- Refresh/navigation does not flicker to main or another conversation.
- Delete closes the selected conversation and its modal without selecting the
  next conversation implicitly.

## Rollout

1. Land core persistence and server ensure/retry behavior.
2. Add the Phase 1 internal-channel authorization gate.
3. Route primary/default and helper chats to dedicated threads behind the
   existing rollback flag.
4. Run deterministic suites and the restart-injected real-Codex smoke.
5. Enable per-thread routing by default in local/dev, then production.
6. Migrate external direct-message sessions and remove `channels_send` from
   main's tool projection completely.

## Acceptance Criteria

- No human chat event is delivered to `main` directly.
- Main cannot send an ordinary message into Apteva internal chat.
- Every visible internal chat reply is attributable to exactly one live
  conversation context.
- Restarting an agent at any handoff boundary does not strand the conversation.
- A successful send/evolve receipt cannot be followed by a pace sleep while a
  requester is waiting.
- Ordinary work yields one visible final; durable handoff yields one truthful
  progress message and one final, with no duplicates.
