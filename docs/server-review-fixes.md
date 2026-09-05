# September 2026 server fixes

The implementation covers active-server findings from the September 5 audit
and performance review. Deprecated chat code was excluded. Runtime behavior
continues to use direct event delivery and SSE; there is no telemetry result TTL.

- Integration HTTP clients share bounded transport pools by proxy/TLS identity.
  Core proxy requests retain query parameters, propagate cancellation, impose a
  response-header deadline, and stream unmodified bodies. Boot retries occur only
  for refused connections, avoiding replay after an ambiguous mutation failure.
- Agent lists use in-memory runtime observations and one authorized database
  query. A bounded background monitor refreshes observations each second; start
  and stop paths update process state immediately. Lifecycle relays use bounded
  parallel requests. Metadata writes update changed fields, and database deletion
  is transactional. Exited children cannot remove replacement processes.
- Compact telemetry facts and minute summaries update in the ingestion
  transaction. Reads combine complete-minute summaries with the partial first
  minute, preserving fractional time boundaries. Replays, corrections, deletes,
  and reopened databases are covered by regression tests. This adds ingestion
  and storage work in exchange for much cheaper dashboard queries. Retention has
  a time index and bounded delete batches.
- Dashboard history uses one project request. Refreshes skip hidden tabs and
  overlapping calls, and reject stale project responses. Live telemetry remains
  subscribed. Public asset caching shares a body-buffer budget across stored and
  pending data, bounds metadata, coalesces misses, and versions cache keys with
  routes. Streams, failed writes, and truncated responses are not cached.
- Internal integration/custom MCP routes require route capabilities. Capability
  issuance validates agent scope. Predictable development tokens were removed;
  externally supplied identity headers are scrubbed. Cookie mutations check
  origin, app scope changes check both scopes, and ingress ownership cannot be
  silently claimed by another install. Slow MCP starts do not hold manager locks.
- Webhook secrets require signatures; email ingress verifies freshness/signatures
  and deduplicates provider IDs before delivery. Verified email retains reply
  context. OAuth refresh uses per-connection locks across local processes,
  re-reads current credentials, and writes with compare-and-swap protection.
- App subscription deliveries are queued durably before emit acknowledgement.
  Retries retain stable core event IDs; failed entries retry with backoff and
  remain in the outbox after 12 failures for diagnosis. Successful/canceled
  receipts are retained seven days. Delivery is at least once; idempotent core
  event IDs prevent normal retry duplication. Polling excludes active jobs before
  limiting and caps concurrent work. Interrupted provisioning is recoverable.
- Shutdown cancels/drains external work, retains authenticated internal callbacks
  during process teardown, stops workers, and shuts down retained listeners.
- Recovery format 2 includes persistent data and optional passphrase-wrapped
  key recovery; see [recovery.md](recovery.md). Go 1.26.6 and release validation
  gates replace the vulnerable build toolchain. Dashboard assets were rebuilt
  and synchronized into the server's embedded assets.

The broader suggestions to extract the complete router, replace every trusted
header with a typed principal, and sandbox all native apps remain architectural
projects, not prerequisites for these targeted fixes. Native apps still run with
the server's OS identity and are suitable only for trusted installed code.
