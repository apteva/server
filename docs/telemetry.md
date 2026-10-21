# Unified telemetry API

Apps and authenticated API clients share one stream engine, retained-event query,
projection policy and replay buffer. The existing endpoints are upgraded:

| Read | Authenticated API client | App installation callback |
| --- | --- | --- |
| Live/resumable SSE | `/api/telemetry/stream` | `/api/apps/callback/telemetry` |
| Paged retained history | `/api/telemetry?format=page` | `/api/apps/callback/telemetry/history` |
| Agent inventory | `/api/telemetry/agents` | `/api/apps/callback/telemetry/agents` |
| Runtime inventory | `/api/telemetry/runtimes` | `/api/apps/callback/telemetry/runtimes` |
| Runtime detail | `/api/telemetry/runtimes/:id` | `/api/apps/callback/telemetry/runtimes/:id` |
| Agent topology | `/api/telemetry/agents/:id/topology` | `/api/apps/callback/telemetry/agents/:id/topology` |

All routes above are GET-only. There are no `/observations` routes.

The original callback event shape and live-only default remain compatible.
Dashboard `all=1`, `instance_id`/`agent_id`, numeric `since`/`Last-Event-ID`, helper
visibility, and default array history responses remain compatible too. These are
small compatibility adapters around the shared telemetry implementation.

SDK `TelemetryAPI()` / `NewTelemetryClient` use that same API. `SubscribeTelemetry`
is a convenience wrapper for live-only content. `WatchTelemetry` opts into typed
delivery frames with `format=delivery`; replay is enabled unless `replay=false`.
Both use one SDK connection/reconnect implementation; subscriptions do not probe
and discard an initial SSE connection.

Internal readers, retained-history pages and legacy runtime history share the
same SQL filters and row decoder. Public array responses share one sanitizer;
runtime owners retain their existing access rules. Legacy replay and resumable
delivery share one ring traversal and the same event identities. Compatibility
methods adapt request/response shapes without maintaining another query or replay
algorithm.

API clients use ordinary user/API credentials, including read-only API keys.
Apps use installation credentials and approved permissions. No numeric request
header or runtime ID can bypass authorization. The observer's permissions are
checked independently of the runtime owner's control permissions.

Telemetry query parameters: `project_id`, `agent_id`, `runtime_id`, `thread_id`, `thread_prefix`,
`events` (comma-separated exact event names or a family such as `tool`),
`since`, `until` (RFC3339), `limit` (1–500), `cursor`, and `projection`
(`metadata` by default, or explicitly permissioned `content`). History returns
`{server_id, events, next_cursor, order}`. Cursor pagination is newest ingestion
first, bounded by the initial page's row boundary. A query's filters must remain
unchanged when continuing. Output is bounded in size and query duration.

The upgraded stream uses SSE `data` frames containing the SDK's typed
`TelemetryDelivery`. Set `backfill=true` to receive retained stored history on
connection and on detected replay gaps. It subscribes before history is loaded
and also replays the retained in-memory suffix, covering overlapping persistence
and live ingestion. Clients deduplicate by Server identity and producer event
ID. The SDK handles overlap, reconnection, cursors, backpressure and cancellation.
A `history` frame's `next_cursor` supports older-history pagination.

Cursor epochs distinguish Server restarts even if sequence numbers coincide.
`gap` reasons distinguish process restart, expired retention and cursor reset.
The ring remains the existing bounded 8,192-event buffer. Durable history does
not contain live-only token fragments, and ingest paths are not an exactly-once
transactional event log. A gap must remain visible when complete recovery is
impossible. Existing live-only subscriptions keep their original semantics through the same engine.

Open streams recheck permissions, installation scope and installation status
at least once per second, with immediate agent-eligibility revision checks.
Writes have deadlines so an indefinitely stalled peer cannot keep a stream
alive. Revocation closes the stream with a terminal error frame.

Metadata uses an explicit allowlist of activity/tool names, known statuses,
numeric counters/durations, and correlation IDs. Arbitrary payloads, model
output, tool arguments/results and error text are omitted. Content access
preserves the existing directive sanitization and explicit raw permission.
Oversized content returns an explicit omission marker with event identity.

Runtime output contains only identity, project, status, agents, installed apps
and lifetime timestamps. Runtime control routes retain owner-only checks.
Topology uses saved configuration and recorded parents; unavailable
information is marked unknown. Tool names describe explicit configured filters
when known. This API does not infer handoffs, own a scene graph, start services,
collect CPU metrics, or add a central remote-server registry.

See `../app-sdk/TELEMETRY.md` for typed client usage and compatibility details.
A consuming app must be built with the supporting SDK and run against a Server
supporting these routes. Existing apps do not acquire these permissions during
an upgrade without the normal permission approval flow.
