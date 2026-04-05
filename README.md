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
| `QUIET` | — | Set to `1` to suppress console output |

## License

MIT
