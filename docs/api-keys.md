# Private API key permissions

In dashboard **Settings → API Keys**, choose **Private** and select **Read only**
or **Read & write**. New dashboard keys default to Read only. Existing keys keep
their previous read/write access. Permissions always remain bounded by the
owning user's existing project and platform permissions.

Create a key with `POST /api/auth/keys` using a session or read/write private key:

```json
{"name":"Monitoring","kind":"private","access":"read_only"}
```

The creation response includes `access` and the raw `key` once. The key list
(`GET /api/auth/keys`) includes `access` but never the raw key. The API accepts
`read_only` and `read_write`; omitted access defaults to `read_write` for older
clients. This setting applies to private keys. Scoped client/delegated app keys
retain their separate app permissions.

Use `Authorization: Bearer <key>` or `X-API-Key: <key>`. Explicit keys take
precedence over ambient session cookies on authenticated management routes, so
an accompanying admin session cannot upgrade a read-only request. The existing
query-string carrier remains supported for `/api/telemetry/stream`.

Read-only keys permit these GET inspection routes:

- `/api/auth/me`, `/api/auth/keys`, and `/api/auth/onboarding/status`.
- `/api/projects` and `/api/projects/:id`.
- `/api/agents` and `/api/agents/:id` (also the `/api/instances` aliases).
- Agent `status`, `threads`, `events`, `chat-history`, and `config` subroutes.
- `/api/telemetry`, timeline, stats, project activity/stats/timeline/tools, and stream.
- `/api/settings/server` and `/api/settings/new-agent-provider`.

Agent configuration is a restricted view of saved settings, with
`config_source: "saved"`. Provider/MCP credentials, commands, environment
variables, and arbitrary nested configuration are excluded. Agent detail
responses use the same configuration field filter. This does not redact user
content from directives, chat history, or telemetry.

Mutations, key issuance/revocation, credential and backup exports, arbitrary
app/core proxy routes, tool execution and protocol upgrades are denied with
HTTP 403. GET alone does not make an operation safe: the backend explicitly
allows reviewed routes, so future endpoints are denied until reviewed. HEAD
and OPTIONS do not authorize resource operations with these keys. Public
unauthenticated endpoints and CORS preflight handling retain their own behavior.

Expired, revoked, and deleted keys stop authenticating. To change a key's
permission, create a replacement and revoke the old key from a session or a
read/write key. No core changes are required.
