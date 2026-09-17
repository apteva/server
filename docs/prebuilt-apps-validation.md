# Prebuilt app validation — 2026-09-13

Implemented an additive runtime.artifacts contract in the SDK and server. The
installer prefers a verified package for the local platform across marketplace,
onboarding dependencies, updates and exact runtime recovery. Unsupported
platforms retain source delivery; selected but invalid artifacts fail explicitly.

Conversations 0.23.3 was packaged from the existing isolated onboarding server's
installed source. The local untracked 0.9.0 app folder was not used or changed.
A comparison against the pre-task backup found all 35 local files unchanged.
Source and resource inventories match between initial and final release builds:
131 source files, 37 packaged resource files. No app business logic, SDK pin,
existing database, messages, attachments, or configuration was edited.

Validation completed:

- SDK: go test ./... -short passed.
- Server: go test ./... -short passed (104 seconds).
- Additional final artifact tests: cache corruption, checksums, HTTP errors,
  traversal, symlinks, duplicate entries, manifest mismatch, missing resources,
  source fallback, data/config retention, process preservation after a failed
  download, restart, and clone quarantine all passed.
- Conversations: its existing go test -mod=readonly ./... suite passed with
  GOWORK=off against the app's pinned SDK.
- Real Conversations package: health, panel, MCP tools/list, 24 database tables,
  and preserved database content after restart passed with no build tools on PATH.
- Real HTTP package installation through the server passed. Onboarding reused it,
  the MCP bridge registered eight agent-visible tools, and an offline upgrade
  kept its configuration/binding metadata and completed process handoff.
- Dashboard: 336 tests passed; production build passed and was synced into the
  server's embedded dashboard. Preparation progress and double-submit handling
  were exercised in the existing onboarding flow tests.
- Linux and macOS, amd64 and arm64 packages built successfully. Only the host's
  macOS arm64 binary was executed locally. The release workflow also includes a
  Linux amd64 health/UI/MCP/database smoke test before publication.

Measured local cold package preparation and startup was approximately 1.9–2.2
seconds; verified cache reuse was approximately 19–27 ms. The earlier source
install took 29 seconds. These are local tests; internet download latency is
additional. Compressed packages are approximately 5–6 MB per platform.

Local outputs:

- Server candidate: /private/tmp/apteva-server-prebuilt
- Four packages and manifest: /private/tmp/apteva-conversations-0.23.3-final-packages
- Detailed server smoke log: /private/tmp/apteva-artifact-server-smoke.log

Running servers were not restarted. Public artifacts have not been uploaded and
the public registry has not been switched. The release workflow must run from
the reviewed current Conversations source, then its generated apteva.yaml can
replace Conversations' registry manifest URL. Do not publish the older local
0.9.0 checkout over the current app. Other apps remain on their existing delivery
paths until they explicitly declare artifacts.
