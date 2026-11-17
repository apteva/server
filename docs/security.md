# Security controls

What apteva-server records, enforces and exposes for incident response, and how to turn on the opt-in parts.
Everything here is admin-only under `/api/security/…`.

## Audit trail

- **Where it lives.** A separate SQLite file at `<DATA_DIR>/security/security-audit.db`, mode 0600. It is not the platform database, so a platform restore or bug can't rewrite it.
- **Tamper evidence.** Each event is linked to the previous one by a sha256 hash chain. `GET /api/security/chain` re-verifies it and reports the first broken row.
- **What gets recorded:**
  - logins, MFA, logout, registration and password changes, each with the source IP and session reference;
  - API key creation and revocation;
  - credential reveals;
  - platform role changes;
  - account disable and enable;
  - containment actions;
  - SSH policy changes;
  - refused privileged attempts;
  - 401/403 responses, rate-limited to 30 a minute per IP, and dropped beyond that;
  - mutating requests to auth, credential, permission, export, deployment and config routes;
  - privileged MCP tool calls through the app proxy, which by default are the names that look like exec/run/delete/reveal/deploy/rotate.
- **What is never stored.** Secrets. Details are redacted by key name (`password`, `token`, `secret`…) and by value pattern (`sk-`/`uk_` keys, bearer tokens, JWTs, PEM blocks, `KEY=value`, long hex). Credentials appear only as references such as `session:<16 hex>` or `api_key:<id>`.
- **Writes never block requests.** If the queue is full, the gap is counted and later written into the trail as `audit.events_dropped`, which raises a critical alert.

| Setting | Default | Meaning |
|---|---|---|
| `APTEVA_SECURITY_RETENTION_DAYS` | 400 | Events older than this are pruned; the chain re-anchors on the oldest kept row. When set, it also applies to the host SSH collector's own store (default 365) |
| `APTEVA_SECURITY_FORWARD_URL` | — | Independent, ideally append-only destination. Events are POSTed in order and the cursor survives restarts, so each event is sent once |
| `APTEVA_SECURITY_FORWARD_TOKEN` | — | Bearer token for that destination |
| `APTEVA_SECURITY_AUDIT_MCP` | heuristic | `all` audits every MCP tool call, `off` none |
| `APTEVA_SECURITY_AUDIT_MCP_TOOLS` | — | Extra tool names to always audit |

## Alerts

Alerts come from deterministic rules evaluated as events are written:
- a login from a new source (the first login is exempt);
- 10 or more failed logins from one IP in 10 minutes;
- a credential reveal;
- a platform role change;
- an API key created by a non-user principal;
- the first admin being registered;
- a refused privileged operation;
- a containment action;
- dropped audit events;
- every rule match reported by the host SSH collector, which appears as `ssh_<rule>`.

Alerts are deduplicated per event and rule, so a replayed event never alerts twice. Acknowledge them with `POST /api/security/alerts/<id>/ack`.

| Setting | Meaning |
|---|---|
| `APTEVA_SECURITY_ALERT_WEBHOOK` | POST target for each alert, retried with backoff. Each alert is delivered once across restarts, and every delivery carries an `Idempotency-Key` |
| `APTEVA_SECURITY_ALERT_SECRET` | Signs the body: `X-Apteva-Signature: sha256=<hmac>` |

SSH alerts that apteva-host already sent to its own webhook are listed here but not sent a second time.

## Sessions, keys and accounts

- **Sessions.**
  - Tokens are stored as `sh_` + sha256. Existing raw rows are converted on upgrade, so nobody is logged out.
  - Each session keeps the IP and user agent it was created from, and when and from where it was last seen.
  - A revoked session is kept, with who revoked it and why, and purged 90 days after it ends.
- **API keys.** Private keys record the IP of each use. Revoking a key keeps the row, with the actor and reason.
- **Disabling an account** (`POST /api/security/users/<id>/disable`):
  - existing sessions, private keys, delegated `uk_` keys and app callbacks that carry the user's session are all rejected immediately;
  - the user's agents are stopped;
  - you can't disable your own account or the last active admin.
- **Your own sessions.** Users can list them with `GET /api/auth/sessions` and end one with `DELETE /api/auth/sessions/<ref>`.

### Step-up for privileged operations

These operations need a dashboard session that confirmed its password, and its MFA code when enrolled, within the last 15 minutes:
- revealing stored credentials;
- changing a platform role;
- disabling an account;
- containment;
- changing the SSH policy.

Sign-in counts as confirmation; after that, use `POST /api/auth/reauth`. A refused call returns 403 with `error: reauthentication_required` and `reauth_url`. API keys, app tokens and agents get `interactive_session_required`.

| Setting | Default | Meaning |
|---|---|---|
| `APTEVA_SECURITY_REAUTH_MINUTES` | 15 | Step-up window |
| `APTEVA_SECURITY_ALLOW_KEY_PRIVILEGED` | off | `1` lets private API keys perform these operations. This is for break-glass automation only |

### Containment

`GET /api/security/containment?user_id=N` lists everything the account can still use:
- sessions;
- keys;
- app installs;
- Fleet tenants.

`POST /api/security/containment/revoke` takes the following and requires a `reason`:

```json
{"user_id": N, "sessions": true, "api_keys": true, "keep_key_ids": [], "include_fleet": true, "reason": "…"}
```

- It returns per-target results, counts, and `complete`.
- Your own current session is kept unless you ask otherwise.
- Fleet tenants are reported as `unconfirmed`, so `complete` is `false`, because Fleet has no tool yet to revoke a tenant's keys. Rotate those tenants by hand until Fleet gains one.

## What apps inherit

By default, app sidecars, source builds and stdio MCP tools get an allowlist of the server's environment instead of all of it. That allowlist is:
- base system variables (`PATH`, `HOME`, `TZ`, locale, proxy, TLS);
- runtime toolchain variables, for builds;
- variables prefixed with the app's own name, so the `fleet` app gets `FLEET_*`.

The following are never forwarded, whatever the mode: `SERVER_SECRET`, `APTEVA_HOST_*`, `APTEVA_DELEGATED_DNS_*`, `INSTANCE_SECRET`, `AGENT_SECRET`, `APTEVA_SECURITY_*` and `APTEVA_CHILD_ENV_ALLOW`.

Each spawn logs the names it withheld, never the values.

| Setting | Meaning |
|---|---|
| `APTEVA_CHILD_ENV_ALLOW` | Comma list of extra names. Accepts `NAME` and `PREFIX_*` for every app, or `<app>:NAME` / `<app>:PREFIX_*` for one app only (e.g. `fleet:HCLOUD_TOKEN`) |
| `APTEVA_CHILD_ENV_MODE=inherit` | Temporary escape hatch: the old behaviour, minus the never-forwarded names |

When rolling out, start the server and read the `not forwarding … server environment variable(s)` log lines. Allow only the names an app really needs.

## Per-app OS users (opt-in)

`APTEVA_APP_ISOLATION=uid` runs each app install as its own user and group, numbered `APTEVA_APP_UID_BASE` (default 200000) plus the install id. One app then cannot read another app's database, files or process environment, nor the platform database or `.secret`.

| Path | Mode | Owner |
|---|---|---|
| apps root, `<app>/`, `<app>/<version>/`, `<app>/data/` | 0711 | server (traverse only) |
| `<app>/data/<install>/` (cwd, `HOME`, `APTEVA_DATA_DIR`, DB) | 0700 | app user |
| `<version>/stderr.log` | 0600 | server |

When it's on, the server also:
- switches to umask 077;
- removes group and other access from the top level of its data directory, because app users can now traverse into the apps tree inside it;
- hands an install's existing data to its user on the first isolated start.

Requirements and behaviour:
- **Platform.** It needs Linux, with a root server or a root apteva-host.
- **If it can't apply,** for example on macOS, a non-root server, or an apps directory under a private home such as `/root`, apps refuse to start and the reason is logged. They never silently run unisolated.
- **Ancestor directories are never changed.** If the apps root's parents aren't traversable, the error says which one; move the apps directory with `APTEVA_HOME`.
- **Exemptions.** `APTEVA_APP_ISOLATION_EXEMPT=containers,deploy` keeps named apps on the server user. Use it for apps that need the server's privileges, such as the Docker socket.
- **Rollout.** Enable it on a staging host first and check that every installed app starts.

## SSH access to the machine

The collector runs inside apteva-host, one per machine. It works while apteva-server is down: it keeps collecting, storing and alerting to its own webhook. apteva-server copies its events into the audit trail every 15 seconds (`APTEVA_SECURITY_HOST_POLL`). Each event keeps the host's ID, so restarts and replays never duplicate anything.

| Endpoint | Purpose |
|---|---|
| `GET /api/security/host` | Collector health and problems, e.g. stale heartbeat, log source errors, low disk, no webhook |
| `GET /api/security/host/policy` | Approved sources and key fingerprints, root-login rule, failure-burst threshold, webhook. The secret is never returned |
| `PUT /api/security/host/policy` | Change the policy. Needs step-up, and is audited with the before/after shape but no secret |
| `POST /api/security/host/sync` | Copy pending events now |

Setup (systemd unit, shared `/var/lib/apteva-security` directory) and the collector's settings, such as approved sources, backfill and retention, are in [security-collector.md](security-collector.md).

### The host control socket

Calls to `control.sock` now need the per-host token (`X-Apteva-Host-Token`), which is minted at host start. Server slots get it as `APTEVA_HOST_CONTROL_TOKEN`; other local callers read `$APTEVA_HOME/host/control.token`, mode 0600. The `APTEVA_HOST_` prefix keeps it out of app environments.

An app running as the same OS user as the host can still read that file. The token therefore stops other users and remote callers, while separating apps from the host needs `APTEVA_APP_ISOLATION=uid`.

The `apteva` CLI's `update` through the host must send this token. Older CLIs get 401 from an upgraded host.

**What it does and doesn't prove.** The collector records:
- logins: user, source, method and key fingerprint;
- session open and close;
- `authorized_keys` changes;
- gaps in its own coverage.

It does not see commands typed inside a session. That needs auditd or session recording. Servers that don't run under apteva-host have no collector, and `/api/security/host` says so.

## Exposure inventory

`GET /api/security/inventory` lists:
- the listeners, flagging any non-loopback management bind;
- MFA coverage of admin accounts;
- the child-env mode and app-isolation mode;
- trusted proxy settings;
- Fleet tenants.

It also lists findings in plain language. `GET /api/security/overview` combines this with audit health, open alerts, the last 24 hours of denials and logins, and the host collector status.
