# Backplane

Multi-tenant management layer for [Cogito](https://github.com/apteva/cogito). Handles auth, spawns Cogito instances on demand, proxies API calls.

## Quick Start

```bash
# Set API key for Cogito instances
export FIREWORKS_API_KEY=your-key

# Make sure cogito binary is in PATH
export COGITO_CMD=/path/to/cogito

# Build and run
go build -o backplane . && ./backplane
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
# Create a Cogito instance
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

### Proxy to Cogito (authenticated)

```bash
# These forward directly to the Cogito instance's API
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
| `PORT` | 8080 | Backplane HTTP port |
| `DB_PATH` | backplane.db | SQLite database path |
| `COGITO_CMD` | cogito | Path to Cogito binary |
| `DATA_DIR` | data | Instance data directory |
| `FIREWORKS_API_KEY` | — | Passed to Cogito instances |

## License

MIT
