# Realtime voice configuration

Voice uses the existing Core thread API, audio WebSocket proxy and telemetry
stream. An integration can supply text and realtime models with one credential.
The agent's default text provider and text model maps remain independent of its
voice selection.

## Provider configuration

```json
{
  "name": "openai",
  "default": true,
  "models": {"large": "gpt-6.1-sol"},
  "realtime_models": {"large": "gpt-live-1"},
  "realtime_voice": "brise",
  "realtime_execution": {"mode": "thread"}
}
```

Server preserves the realtime fields during narrow configuration edits, stopped
edits, and restart hydration. Old installations with options only in Core's
config.json retain those options. Explicit Server-owned provider options take
precedence over disk fallbacks. The existing `realtime_provider` and
`realtime_model` agent selections are translated to provider model maps without
changing the text default. Legacy realtime companions remain supported.

Core's `realtime_providers` metadata identifies the transport provider, shared
integration, configured models and each model's capabilities. Dashboard settings
and voice availability share one resolver. Only old Core responses without that
metadata use the legacy suffix adapter. An explicit empty metadata list is
respected. Model-specific catalog voices/defaults override provider-wide voices.
The OpenAI catalog adds GPT-Live 1 with Brise and Sillage; existing realtime
models remain selectable. Select and save a new model to load its execution
capabilities from Core. No second integration or API key is required.

## Per-thread options

Both SDK spawn request types accept `Model` and optional `RealtimeExecution`.
`Model` is a configured **tier** (`large`, `medium`, `small`), not a model ID.
The chosen provider's voice map resolves the actual voice model. Omitted
execution configuration inherits the provider setting.

```go
sdk.RealtimeSpawnRequest{
    AgentID: agentID, ThreadID: "voice-unique-id", Provider: "openai",
    Model: "large", Voice: "sillage",
    RealtimeExecution: &sdk.RealtimeExecutionConfig{
        Mode: "provider", Provider: "openai", Model: "large",
    },
    Directive: "Answer briefly and use the attached tools when required.",
    InitialMessage: "Greet the caller.",
    CapabilityMode: sdk.RealtimeCapabilitiesNone,
}
```

`thread` uses the agent's ordinary backend inference. `provider` delegates
inference through the voice integration, requiring its configured text provider;
Core still owns the execution child, tool grants, history and lifecycle. The agent configuration UI exposes delegation only when Core advertises
support. Apps such as Conversations own starting calls, greetings and the voice
UI. They use the existing SDK/Server thread and audio proxy path. This change
does not add a dashboard voice entry point or a separate transport. Core retains protocol and
thread permission validation. Existing inheritance and single-use audio tokens
are unchanged. An initial message is delivered when the audio bridge connects.

## Transcript and activity events

The dashboard uses the existing `realtime.user`/`realtime.assistant` events and
upserts by agent, conversation, role and `data.transcript.id`. Server sequence
numbers reject replayed older revisions; legacy events retain event identity.
Transcript status and finalization metadata are retained. Application grouping
does not mean provider finalization or confirmed audio playback.

`realtime.execution_started` associates the execution child's tools with the
voice conversation. Original child thread identities remain available in event
details. Voice conversation filters include those child activities. Parallel
tools are tracked separately. The expanded voice dock shows session duration /
voice cost separately from backend inference tokens / cost. Replayed usage is
deduplicated and Core's delta accounting is honored. Apps receive final usage
through the existing telemetry interface, including usage emitted at shutdown.

No additional SSE stream, provider credential store or audio transport is added.
