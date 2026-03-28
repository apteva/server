# Server

Multi-tenant management layer for [Core](https://github.com/apteva/core). Handles auth, spawns Core instances on demand, proxies API calls.

## Quick Start

```bash
# Set API key for Core instances
export FIREWORKS_API_KEY=your-key

# Make sure core binary is in PATH
export CORE_CMD=/path/to/core

# Build and run
go build -o server . && ./server
```

## API

### Auth (public)

```bash
# Register
curl -X POST localhost:8080/auth/register \
  -d '{"email":"you@example.com","password":"yourpassword"}'

# Login → get session token
curl -X POST localhost:8080/auth/login \
  -d '{"email":"you@example.com","password":"yourpassword"}'

# Create API key (authenticated)
curl -X POST localhost:8080/auth/keys \
  -H "Authorization: Bearer <token>" \
  -d '{"name":"my-key"}'
```

### Instances (authenticated)

```bash
# Create a Core instance
curl -X POST localhost:8080/instances \
  -H "Authorization: Bearer <token>" \
  -d '{"name":"my-agent","directive":"You manage my calendar"}'

# List instances
curl localhost:8080/instances \
  -H "Authorization: Bearer <token>"

# Stop and delete
curl -X DELETE localhost:8080/instances/1 \
  -H "Authorization: Bearer <token>"

# Update directive
curl -X PUT localhost:8080/instances/1/config \
  -H "Authorization: Bearer <token>" \
  -d '{"directive":"New mission"}'
```

### Proxy to Core (authenticated)

```bash
# These forward directly to the Core instance's API
curl localhost:8080/instances/1/status -H "Authorization: Bearer <token>"
curl localhost:8080/instances/1/threads -H "Authorization: Bearer <token>"
curl localhost:8080/instances/1/events -H "Authorization: Bearer <token>"  # SSE
curl -X POST localhost:8080/instances/1/event \
  -H "Authorization: Bearer <token>" \
  -d '{"message":"do something"}'
```

## Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| `PORT` | 8080 | Server HTTP port |
| `DB_PATH` | server.db | SQLite database path |
| `CORE_CMD` | core | Path to Core binary |
| `DATA_DIR` | data | Instance data directory |
| `FIREWORKS_API_KEY` | — | Passed to Core instances |

## License

MIT
