# WebSocket proxy transport telemetry

Apteva's app, hostname, runtime and environment WebSocket proxies share this
instrumentation. Realtime audio uses the same trace, collector and history
format. It measures proxy transport activity; it does not sample CPU or record
frame payloads, media tokens, full request paths or query strings.

## Correlation

Each attempted upgrade receives a fresh server-owned
`X-Apteva-Proxy-Connection-ID`, replacing every client-supplied value. The ID is
forwarded to the backend and returned to the client on successful and rejected
handshakes. Events contain both `proxy_connection_id` and the compatibility
field `connection_id`.

App call IDs are adopted only from the backend's bounded `X-Apteva-Call-ID`
response header, including HTTP 409 and other rejected statuses. The app must
set that header after authenticating the call and before sending its response.
A public `call_id` query/header is not proof of call identity. Realtime's
existing request correlation hint remains supported; its backend can override
it with the response header.

The fixed `route_category` values are `carrier_media`, `browser_media` and
`diagnostic`. Known media route families are classified without retaining the
path. An app can return `X-Apteva-Proxy-Route-Category` with one of those values
for custom routes. Unknown categories are ignored. `client` denotes the
external carrier/browser leg, and `backend` denotes the app/core leg.

## Events and ordering

Every event has a UTC `observed_at`, per-process `monotonic_ns`,
`event_sequence`, `process_id` and `event_id`. Sequence order establishes the
order Apteva observed events within that process; it is not proof of which
remote system caused a network failure.

- `opened`: copying started after an accepted upgrade.
- `rejected` / `failed`: backend status or upgrade error, including attempts
  where copying never started.
- `first_termination`: the immutable first directly observed read/write error
  or EOF. It includes operation, source/destination leg, error type and bounded
  unwrap chain, byte counts, last successful activity, and whether a local
  close had already been requested.
- `close_trigger`: a shutdown/cancellation or other local decision with no
  preceding transport error, used as the causal event for a local close.
- `local_close_requested`: emitted before socket close, half-close or an
  explicit realtime close frame, with a structured reason and triggering
  termination event ID where applicable. Reasons include `leg_terminated`,
  `other_leg_terminated`, `request_canceled`, `server_shutdown`,
  `backend_unavailable` and `peer_close_ack`.
- `cleanup`: subsequent directly observed transport errors, separately linked
  to the first termination. Duplicate errors for the same operation/leg are
  suppressed.
- `direction.closed`: final copy totals, separate from first-error evidence.
- `closed`: the first observation, both copy summaries, activity totals and
  completeness. Pre-copy failures explicitly report `copy_not_started`.
- `forwarding_history`: numbered chunks of the incident's sample window.
- `history.unavailable`: the history registry reached capacity.
- `proxy.telemetry.loss`: dropped events, history capacity drops and persistence
  failures, retained across retries until successfully persisted.

Raw proxy reads/writes are instrumented beneath `io.Copy`; inner WriterTo and
ReaderFrom optimizations are suppressed. Hijacking drains the original HTTP
reader before reading the underlying connection. Both TLS connections and
already-buffered upgrade data pass through the observers. Realtime counts
message payload bytes instead of raw wire bytes; `byte_unit` makes this
explicit.

## History and limits

An independent one-second sampler keeps a ring covering the preceding 60
seconds. The first transport termination freezes the pre-incident window and
collects up to 30 seconds afterward, including explicitly closed/idle samples.
A connection younger than 60 seconds naturally has less pre-incident evidence.
Delayed sampling is visible in `interval_ms`; samples are not fabricated to
fill gaps. Samples include cumulative and interval read/write bytes, activity
timestamps and monotonic offsets, total write duration and maximum write delay.
History uses at most 91 boundary/window samples per tracked connection.

The collector tracks at most 256 histories and queues at most 256 records.
History chunks contain at most eight samples. Serialization, logging,
context-bounded database writes and broadcaster fanout run in the worker,
never in a forwarding read/write. Persistence failures retain a bounded batch
of 64 events for retry. Overflow is counted explicitly, and missing history
chunks are detectable from their part counts. Forwarding proceeds even when
telemetry storage is unavailable. Terminal evidence can also be lost during
sustained overload; the collector reports the loss rather than blocking media.

Server shutdown requests closure of hijacked connections before stopping apps.
Collection then flushes with a bounded deadline. Shutdown may truncate the
30-second post-incident window; chunks mark `shutdown_truncated` and
`window_complete`. Abrupt process death cannot flush an in-memory window.

## The previous missing termination record

The previous implementation read response call headers only for HTTP 101,
never forwarded a server-owned proxy ID, and observed termination at
`io.Copy` completion through `WriteTo`. It wrote logs and database events
synchronously in that path. Failures after hijacking but before a copy started
could return without a normal closure record, and it did not observe individual
read/write failures or record local close decisions. A copy that had not yet
returned could have no termination record even if an app had replaced its own
logical media bridge. Persistence failure could also leave a log entry without
its database event.

Those are established coverage gaps, not a confirmed explanation of any
particular Flexylead incident. Confirm the deployed server build and compare
the staging call's authenticated call ID, proxy opened/rejected/failed/direction
records, application bridge generation and service restart timestamps before
attributing the old connection's missing record. New instrumentation cannot
reconstruct transport events that were never collected.

## Validation

Tests cover response correlation on HTTP 409 with both Director and Rewrite
proxies; spoofed IDs; each leg's read/write failures and immutable first
observation; copy optimization bypass; TLS; a frame buffered with its upgrade;
shutdown and cancellation decisions; bounded pre/post history; and nonblocking
overflow with persistence failure/recovery. Existing realtime and proxy routing
checks must continue to pass.
