# Kimi Code membership connection

Status: integration and Server support prepared locally. Core adapter support
is deliberately deferred. No deployment or existing agent configuration changes
are required by this increment.

## Connection and discovery

The `kimi-code` integration is separate from `moonshot-ai`, `opencode-go` and
`fireworks`. It accepts a Kimi Code membership key created in the Code console,
uses `https://api.kimi.ai/coding/v1`, and identifies itself as `Apteva`. It does
not use the Kimi CLI, borrow another client's identity, or fall back to paid
Moonshot Platform billing. Membership access is for programming workflows.

The existing integration runner provides `list_models` and non-streaming
`chat_completion`; health checks reuse the read-only `list_models` tool.
Callers provide the model explicitly and preserve assistant `reasoning_content`,
tool calls and tool results in subsequent history. Models with fixed thinking
behavior should omit `reasoning_effort`.

The existing connection API supports:

- `GET /api/connections/:id/models?refresh=1` for account model discovery.
- `GET/PATCH /api/connections/:id/runtime-config` for staged model preferences.
- The usual connection create, test, disconnect and encrypted credential storage.

Server discovery reads account-specific context limits and image/video flags.
The optional model policy only admits documented coding model IDs actually
returned by that account. It never fabricates availability during an outage.
Unknown future model IDs require a policy update before agent use. Cache keys
use the existing full-credential hash; explicit refresh bypasses the cache.

HTTP 401 reports a rejected membership key; 403 points to permissions, plan or
quota; 429 reports rate/quota exhaustion. Valid Retry-After values are included
in discovery errors. Failed discovery is not cached and never falls back to a
different billing endpoint. Discovery is bounded to 15 seconds and 1 MiB, does
not follow redirects, and does not echo upstream error bodies or credentials.

No subscription quota API has been verified for this contract, so it does not
advertise `subscription_usage` or invented quota percentages. Per-token dollar
rates are left unset for this subscription connection.

## Core handoff

The intended contract is provider key `kimi-code` and `KIMI_CODE_API_KEY` from
the encrypted connection. The current Server's existing `isLLMKey` allow-list
does **not** admit it. That same support check now gates runtime catalog choices,
runtime connection summaries and credential export, so connecting the ordinary
integration cannot make it an agent default or feed an unsupported provider
to Core. Model discovery and staged preferences remain available independently.
There is no new manifest availability flag or parallel provider API.

To enable later, implement and qualify the Core compatible provider, then add
`kimi-code` to Server's runtime allow-list and runtime-enabled catalog tests.
The adapter must use the coding endpoint, preserve reasoning/tool-call history,
honor per-model effort metadata (no thinking-off for K3.1 Pro), and test streaming,
cancellation, cache accounting, context limits and quota errors with a real
membership account. Verify native membership access with an honest Apteva
User-Agent. Only enable agent selection once the bundled Core supports it.

## Verification

Local mocked tests exercise actual integration request construction, model
discovery, credentials, cache isolation/refresh, malformed catalogs, bounded
responses, redirects, errors, policy validation, saved connection settings and
the unsupported-Core gate. They perform no membership inference or paid calls.

## References

- Official endpoint and membership scope: https://www.kimi.com/code/docs/en/
- Models and effort behavior: https://www.kimi.com/code/docs/en/kimi-code/models
- Official discovery schema: https://github.com/MoonshotAI/kimi-cli/blob/main/src/kimi_cli/auth/platforms.py
