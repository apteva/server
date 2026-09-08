# Parallel managed calls (unreleased)

Server app HTTP/MCP forwarding, sibling-app calls, connection execution and
provider delegation execute concurrently. Installation, endpoint, caller and
connection identity do not impose execution slots. The active server path uses
`internal/admission.Observer`; the older adaptive Controller remains unused by
server transports. Core is unchanged.

GET /api/admission remains platform-admin-only and reports mode `parallel`,
active/completed/failed/canceled work and bounded operation diagnostics. Queued
is always zero. Diagnostic cardinality overflow drops detail, never requests.
Paths are hashed in full to distinguish nested endpoints without exposing paths
or query values. No per-request CPU polling, destination file leases, automatic
CPU-based growth, or installation dependency-graph rejection runs in this path.
Existing lease files are left untouched; an older running server still has its
old behavior until replaced.

HTTP responses include X-Apteva-Admission-Wait-Ms: 0 and append
Server-Timing: apteva_gateway_queue;dur=0. Response-body EOF/Close completes
accounting, preserving streaming. Failures and cancellation do not replay work.
Authentication and public/private routing are unchanged. Ordinary HTTP bodies
stream through the gateway. Buffered MCP/callback JSON retains its byte-budget
protection; transport request count is not a memory budget.

Functions owns live worker capacity. It has no fixed per-function invocation
ceiling and no default global worker-count ceiling. A byte reservation covers
cold-starting, active and cached idle workers until process exit. Unused caches
are reclaimed before refusal. Actual capacity exhaustion fails promptly instead
of holding nested execution queues. An explicit operator MAX_WORKERS remains
supported. See apps/mcp/functions/README.md for memory budget semantics and the
protected /capacity endpoint. The budget is not a live OS memory-pressure
monitor, and macOS does not gain Linux kernel sandboxing from this change.

The API app's route lookups and Functions version lookup use SDK read pools.
No frontend embed refresh is needed. This change neither alters core execution
budgets nor adds distributed execution or automatic retries.
