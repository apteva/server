# Local GPT-Live verification — 2026-10-21

Implementation is local to Server, dashboard, SDK and integration catalog. No
release or production deployment was performed.

## Runtime verification

Local Server `127.0.0.1:5280`, isolated project **GPT-Live local test**, agent
**1191**, project-scoped OpenAI connection **175**. Credentials are stored by
Server's encrypted connection store; no credential is included here or in code.
The backend text model is `gpt-6.1-sol`; the voice model is `gpt-live-1`.

A synthetic caller generated locally with macOS speech synthesis was resampled
to PCM16 mono 24 kHz and sent in 20 ms frames through the existing Server audio
WebSocket proxy. No microphone or real caller recording was used.

| Execution | Voice | Greeting / caller transcription | One history_search call and spoken result | Thread cleanup |
|---|---|---|---|---|
| Agent inference | Brise | Passed | Passed | Passed |
| Provider delegation | Sillage | Passed | Passed | Passed |
| Agent inference | Sillage | Passed | Passed | Passed |
| Provider delegation | Brise | Passed | Passed | Passed |

The read-only tool searched this isolated conversation's retained history for
“pineapple” and found the caller's own request. It did not access other agents.
The checks waited for execution completion and result audio after the tool
result, rather than counting an early acknowledgement as success.

An additional Sillage/provider test disconnected the browser-facing bridge,
renewed its single-use token, reconnected the same thread, and completed the
same caller/tool/spoken-result flow with one tool call. All temporary voice and
execution threads were removed; only `main` remained afterward. Provider usage
was recorded separately with `accounting=delta`.

An actual configuration check changed voice to Sillage and backend execution to
provider/medium, applied an unrelated narrow edit, stopped the agent and started
it again. Voice models, voice, execution overrides and the independent text
default survived. After verification the standalone test agent was stopped. Its data and
project-scoped encrypted connection were retained. No dashboard calling flow
is exposed; calls remain app-owned.

## UI and automated checks

The browser verified the model selector, Brise/Sillage voices and both advanced
execution choices. Agent configuration exposes these choices for apps to use through the
existing Server path. The experimental header voice button, automatic greeting
and standalone call summary were removed following scope clarification.

UI tests cover capability discovery, legacy discovery, typed
start requests, transcript revisions and stale replay, execution child tools,
separate usage and separate voice/backend usage. Server tests cover
hydration, stopped edits, restart reconstruction, model/voice validation,
shared/legacy selection transitions, SDK callback/runtime forwarding and the
existing authenticated audio proxy. Focused Server race checks, the SDK suite,
Core offline protocol/compatibility checks and dashboard build/type checking
passed. Browser microphone capture and audible playback still need a manual
check using the **Voice** button.

## Intended product flow

Configure the agent's realtime provider/model/voice in agent settings. An app,
such as Conversations, starts the voice thread and controls its greeting using
the existing SDK and Server audio proxy. No standalone dashboard calling entry
point is added. Browser microphone/playback verification belongs to that app's
voice flow.
