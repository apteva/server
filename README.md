# Apteva Server

Management layer for [Apteva Core](https://github.com/apteva/core). Handles auth, spawns core instances, manages integrations, proxies API calls, serves the dashboard.

Usually started automatically by the [Apteva CLI](https://github.com/apteva/apteva). Can also run standalone for production deployments.

## Quick Start

```bash
# Build
go build -o apteva-server .

# Run (spawns core instances as child processes)
CORE_CMD=/path/to/apteva-core ./apteva-server
```

Or let the CLI manage it:

```bash
cd ../apteva && ./apteva   # spawns server + core automatically
```

## What It Does

- **Auth** — user registration, sessions, API keys
- **Instances** — spawn/stop core processes, per-instance config and data
- **Integrations** — 263+ app catalog, encrypted credentials, per-connection MCP servers
- **Providers** — LLM provider management, encrypted API keys, injected into core env
- **Projects** — multi-tenant isolation
- **Subscriptions** — webhook auto-registration with external services
- **Telemetry** — ingest, store, query, stream (SSE)
- **MCP Gateway** — stdio MCP server injected into each core instance for management tools
- **Dashboard** — embedded static web UI
- **Proxy** — forwards CLI/dashboard requests to the correct core instance

## API

### Auth (public)

```bash
# Register
curl -X POST localhost:5280/auth/register \
  -d '{"email":"you@example.com","password":"yourpassword"}'

# Login
curl -X POST localhost:5280/auth/login \
  -d '{"email":"you@example.com","password":"yourpassword"}'

# Create API key
curl -X POST localhost:5280/auth/keys \
  -H "Authorization: Bearer sk-..." \
  -d '{"name":"my-key"}'
```

### Instances

```bash
# Create and start
curl -X POST localhost:5280/instances \
  -H "Authorization: Bearer sk-..." \
  -d '{"name":"my-agent","directive":"You manage support tickets","mode":"autonomous"}'

# List
curl localhost:5280/instances -H "Authorization: Bearer sk-..."

# Stop
curl -X POST localhost:5280/instances/1/stop -H "Authorization: Bearer sk-..."

# Start
curl -X POST localhost:5280/instances/1/start -H "Authorization: Bearer sk-..."

# Update config (full body forwarded to core)
curl -X PUT localhost:5280/instances/1/config \
  -H "Authorization: Bearer sk-..." \
  -d '{"directive":"New mission","mode":"cautious"}'
```

### Proxy to Core

```bash
# These forward to the core instance's API
curl localhost:5280/instances/1/status -H "Authorization: Bearer sk-..."
curl localhost:5280/instances/1/threads -H "Authorization: Bearer sk-..."
curl localhost:5280/instances/1/events -H "Authorization: Bearer sk-..."  # SSE (flushed)
curl -X POST localhost:5280/instances/1/event \
  -H "Authorization: Bearer sk-..." \
  -d '{"message":"do something"}'
```

### Integrations

```bash
# Browse catalog
curl localhost:5280/integrations/catalog -H "Authorization: Bearer sk-..."

# Create connection
curl -X POST localhost:5280/connections \
  -H "Authorization: Bearer sk-..." \
  -d '{"app_slug":"stripe","name":"stripe","auth_type":"api_key","credentials":{"token":"sk_live_..."}}'

# List connections
curl localhost:5280/connections -H "Authorization: Bearer sk-..."
```

### Providers

```bash
# Create provider (credentials encrypted at rest)
curl -X POST localhost:5280/providers \
  -H "Authorization: Bearer sk-..." \
  -d '{"type":"fireworks","name":"fireworks","data":{"FIREWORKS_API_KEY":"..."}}'

# List providers
curl localhost:5280/providers -H "Authorization: Bearer sk-..."
```

### Delegated app access

Identity apps mint browser credentials without choosing their own privileges.
A project owner configures the server-owned policy for each issuer install and
OAuth client; the issuer then supplies only trusted subject data and validated
browser origins.

```bash
curl -X PUT localhost:5280/api/apps/installs/158/delegated-access-policies \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{"policies":[{
    "oauth_client_id":"web-client",
    "scopes":[
      {"type":"app_user","app":"catalog","actions":["items.list"]},
      {"type":"app_user","app":"channel-chat","actions":["chat.list","message.send"],"agent_ids":[566]}
    ],
    "token_ttl_seconds":3600,
    "rate_limit_per_minute":120
  }]}'
```

`PUT` atomically replaces the install's complete policy set; `{"policies":[]}`
disables delegated minting. Apps and actions must be explicit—wildcards are
rejected. Project-scoped installs derive `project_id` automatically; policies
for a global issuer install must include it. Replacing a policy immediately
revokes existing delegated credentials from that issuer install.

The gateway enforces the configured app boundary, browser origin, rate limit,
and MCP tool actions. REST apps receive the trusted identity and policy in
`X-Apteva-*` headers and must map their own routes to the configured actions;
Channel Chat is the reference implementation.

## MCP Gateway

Each core instance gets an `apteva-server` MCP gateway injected automatically. This gives the agent access to management tools:

| Tool | Description |
|------|-------------|
| `list_integrations` | Browse 263+ available apps |
| `create_connection` | Connect an integration |
| `list_connections` | List active connections |
| `create_subscription` | Subscribe to webhooks |
| `list_mcp_servers` | List registered MCP servers |
| `activate_provider` | Switch LLM provider |

The agent can manage its own integrations and connections.

## Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| `PORT` | `8080` | Server HTTP port |
| `DB_PATH` | `apteva-server.db` | SQLite database path |
| `CORE_CMD` | `apteva-core` | Path to core binary |
| `DATA_DIR` | `data` | Instance data directory |
| `APPS_DIR` | auto-detect | Integration catalog JSON directory |
| `PUBLIC_URL` | — | Public URL for webhook callbacks |
| `APTEVA_GEOIP_COUNTRY_DB` | — | Optional path to a Country `.mmdb`. Public app ingress receives a trusted `X-Apteva-Country` ISO code; the file is reloaded after atomic updates. |
| `APTEVA_TRUSTED_PROXY_CIDRS` | — | Comma-separated proxy networks allowed to supply `X-Forwarded-For`, for example `10.0.0.0/8,2001:db8:1234::/48`. Loopback proxies are trusted automatically. |
| `QUIET` | — | Set to `1` to suppress console output |

GeoIP is enabled by default with the free DB-IP Country Lite database. Its first
download happens asynchronously, requires no account or API key, and is refreshed
monthly. GeoIP remains entirely fail-open: a missing, invalid, or unmatched
database never blocks a request. Operators can instead use
`MAXMIND_LICENSE_KEY=... apteva geoip setup --account-id ...` for GeoLite2
Country or `apteva geoip setup --test` during development. Configuration and the
database live below `$APTEVA_HOME/geoip/`; Server keeps the last known-good copy
on refresh failure and notices atomic replacement without a restart.
`APTEVA_GEOIP_COUNTRY_DB` remains an operator-owned override that Server never
downloads over.

DB-IP Country Lite data is provided by [DB-IP](https://db-ip.com) under the
[Creative Commons Attribution 4.0 International License](https://creativecommons.org/licenses/by/4.0/).
`APTEVA_TRUST_PROXY_HEADERS=1` remains available for older, network-isolated
deployments, but new deployments should use the CIDR-scoped setting.

## Instance domain and HTTPS

Platform administrators can use **Settings → Server → Domain & HTTPS** without
installing an app. The same setup is available through `apteva https`:

```sh
apteva https setup agents.example.com --accept-terms
apteva https setup agents.example.com --cloudflare --connection 12 --accept-terms
apteva https setup agents.example.com --proxy --trusted-proxies 10.0.0.5/32
apteva https setup agents.example.com --cert-file chain.pem --key-file key.pem
apteva https status
apteva https doctor
apteva https retry
apteva https cancel
```

- **Direct:** native ACME issuance and renewal. Public ports 80/443 must reach the
  configured listeners. Create the domain's A record and only publish AAAA when
  IPv6 is reachable.
- **Cloudflare:** DNS-01 issuance and renewal using an existing administrator-owned
  Cloudflare connection or `--cloudflare-token-file`. Tokens need Zone Read and
  DNS Edit. Select Full (strict) in Cloudflare. The optional
  `--set-cloudflare-strict` explicitly changes the **whole zone** and needs Zone
  Settings Edit. Apteva creates and cleans up temporary validation TXT records;
  operators manage A/AAAA records. Delegated challenge CNAMEs are not supported.
- **Existing proxy/tunnel:** the proxy owns certificates. Preserve Host, forward
  `X-Forwarded-Proto: https`, and specify its source CIDRs. Loopback is trusted.
- **Imported certificate:** the key must match, cover the domain and be currently
  valid. Replacement is manual. Add `--cloudflare-origin` for a proxied Cloudflare
  Origin CA certificate; that certificate is not browser-trusted directly.

Setup keeps the current public URL until origin and public HTTPS checks succeed.
Diagnostics run from the server and check every published address; they cannot
prove reachability from all external networks. `doctor` checks without activating
an unfinished setup; `retry` completes setup, and `cancel` discards only the
unfinished configuration. A restart during setup retains it for retry. Automatic
Cloudflare renewal continues even if a separate domain setup remains unfinished.

Tokens and imported private keys are encrypted in the instance database. Submit
secrets through HTTPS or run the CLI locally over SSH. Administration requires a
platform administrator session or private API key; app tokens cannot configure
instance HTTPS. Use `--data-dir PATH` to target another local instance.

On Linux, a system service can be prepared for low ports with:

```sh
sudo apteva https prepare-service --system --data-dir /path/to/instance --restart
```

This installs a scoped systemd drop-in with `CAP_NET_BIND_SERVICE`, preserving
other capabilities. User services need a proxy or higher local ports with public
port forwarding (`--http-port 8080 --https-port 8443`). Container deployments must
publish the required ports. Existing environment-managed ingress listeners are
reused at their configured ports. The advanced public URL override remains
available, but changing a URL by itself does not configure TLS.

## License

MIT
