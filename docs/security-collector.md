# SSH access collector

`apteva-host` records SSH logins, failures and `authorized_keys` changes on the
machine it runs on, evaluates fixed alert rules, and sends alerts to a webhook.
It does not depend on `apteva-server`: monitoring and alert delivery continue
while the server is down, restarting or being updated.

## One collector per machine

The collector works in a directory chosen in this order:

1. `APTEVA_SECURITY_DIR`, when set;
2. `/var/lib/apteva-security`, when it exists and is writable;
3. `$APTEVA_HOME/host/security`.

The process that holds an exclusive `flock` on `collector.lock` in that directory
is the collector. Every other `apteva-host` that uses the same directory serves
events, health and policy from the shared store, and retries the lock every
10 seconds. When the collector exits, the kernel releases the lock and another
process takes over.

Several tenant installations on one machine share the collector only if they
use the same directory. Create `/var/lib/apteva-security`, or set
`APTEVA_SECURITY_DIR` for every host. Otherwise each installation falls back to
its own `$APTEVA_HOME/host/security` and runs a separate collector.

Files in the directory:

| File | Contents |
|---|---|
| `collector.lock` | flock target |
| `events.db` | SQLite (WAL): events, source cursors, alert outbox, `authorized_keys` snapshots, health metadata |
| `policy.json` | rules and webhook settings (mode 0600) |
| `machine-id` | generated only when `/etc/machine-id` is unreadable |

## Sources

- **journald (primary):** `journalctl --output=json --follow` filtered to
  `SYSLOG_IDENTIFIER=sshd`, `sshd-session` (OpenSSH 9.8 and later) and
  `sshd-auth` (OpenSSH 10). The first run reads back 7 days
  (`APTEVA_SECURITY_BACKFILL`). Later runs resume with `--after-cursor`.
- **Fallback:** when `journalctl` is missing or fails before producing a record,
  the collector tails `/var/log/auth.log` or `/var/log/secure`. The cursor is
  `<device>:<inode>:<offset>`. After a rotation, the collector finishes the
  rotated file (found by inode as `.1`, `.0` or `-YYYYMMDD`) before it reads the
  new file.
- **`authorized_keys`:** the collector checks `/root/.ssh/authorized_keys`,
  `/home/*/.ssh/authorized_keys` and `/etc/ssh/authorized_keys/*` every 30
  seconds (override the list with `APTEVA_SECURITY_AUTHORIZED_KEYS`). It stores
  only key fingerprints, types, comments and a digest of each key's options. It
  never stores key material or option values.

Each event is written in the same SQLite transaction as the source cursor
advance. An event ID is `sha256(machine-id + source position)`, and the ID is
`UNIQUE`. Replays and restarts therefore never duplicate events or alerts.

If the stored position cannot be resumed, the collector records a
`collector_gap` event with the time range and the reason. Causes:

- the journal was vacuumed or rotated, or the cursor was not found;
- the cursor is malformed;
- the log file was truncated or rotated away.

Unexpected gaps can mean someone tampered with the logs.

## Events

`GET /security/events?after=<seq>&limit=<n>` on the host control socket returns
`{"events": [...], "next_after": <seq>}`. The default limit is 500 and the
maximum is 2000. `seq` increases monotonically.

| Kind | Results |
|---|---|
| `ssh_auth` | `accepted`, `failed`, `invalid_user` |
| `ssh_session` | `opened`, `closed`, `disconnected` |
| `authorized_keys_change` | `baseline` (first inventory), `changed` |
| `collector_gap` | none |
| `collector_started` | none |
| `collector_stopped` | none |
| `policy_changed` | `changed` |

Fields include `user`, `source_ip`, `source_port`, `method`, `key_type`,
`fingerprint`, `pid` and a sanitized `message` of at most 512 bytes, plus
`alerts[]`.

Retention: 365 days (`APTEVA_SECURITY_RETENTION_DAYS`) and at most 500,000
events (`APTEVA_SECURITY_MAX_EVENTS`). The oldest events are pruned first.

## Rules

Rules run inside the collector. They make no model calls.

| Rule | Severity | Trigger |
|---|---|---|
| `unapproved_source` | high | Successful login from an address outside `approved_sources`, even with an approved key |
| `root_login` | critical | Root login when `root_login_allowed` is false, or from outside `root_approved_sources` (an empty list means any source) |
| `unknown_key` | high | Public-key login with a fingerprint not in `approved_fingerprints` |
| `password_login` | high | `password` or `keyboard-interactive` login |
| `authorized_keys_change` | critical / high / medium | Critical: an unapproved key was added. High: other additions. Medium: removals only. A first inventory alerts (medium) only when it finds unapproved keys |
| `failure_burst` | medium | At least `failure_burst_threshold` failed or invalid-user records from one IP within `failure_burst_window_seconds` (defaults: 10 in 600 s). At most one alert per IP per window |
| `collector_gap` | medium | Log coverage gap |
| `policy_changed` | medium | Policy file changed |

An empty policy is the safe default after a credential exposure: every
successful login raises an alert.

## Policy

Manage the policy with `GET /security/policy` and `PUT /security/policy` on the
control socket. It is persisted as `policy.json`. When that file is missing, the
collector seeds it from the JSON file named by `APTEVA_SECURITY_POLICY_FILE`.

```json
{
  "approved_sources": ["203.0.113.10/32", "10.0.0.0/8"],
  "approved_fingerprints": ["SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"],
  "root_login_allowed": true,
  "root_approved_sources": ["203.0.113.10/32"],
  "failure_burst_threshold": 10,
  "failure_burst_window_seconds": 600,
  "alert_webhook_url": "https://alerts.example.com/apteva/ssh",
  "alert_webhook_secret": "<random>"
}
```

- **Sources:** CIDRs or bare IP addresses.
- **Fingerprints:** OpenSSH SHA256 form (`ssh-keygen -lf key.pub`).
- **Webhook URL:** must be `https`. Plain `http` is accepted only for loopback
  receivers.
- **Webhook secret:** never returned. Reads report `alert_webhook_secret_set`. A
  PUT without a secret keeps the stored one; clearing the URL also clears the
  secret.

## Alert webhook

Each alert is queued in an outbox in the same transaction as its event. Only the
collector (the lock holder) delivers alerts. Each one is a `POST` with this JSON
body:

```json
{"idempotency_key": "<event id>:<rule>", "created_at": "...", "machine_id": "...",
 "hostname": "...", "alert": {"rule": "...", "severity": "...", "message": "..."},
 "event": {...}}
```

Headers:

- `X-Apteva-Signature: sha256=<hex HMAC-SHA256(body, secret)>`
- `X-Apteva-Idempotency-Key`
- `X-Apteva-Event-Id`

Any non-2xx response is retried with exponential backoff (5 s doubling, capped at
1 h), and retries survive restarts. Redirects are not followed. A delivered alert
is never sent again. A crash between the receiver's 2xx and the collector
recording it can resend one alert, so receivers must deduplicate on the
idempotency key. Without a webhook URL, alerts stay queued. The server can read
them through the events API (`alerts[]`); `alert_backlog` in health counts them.

## Health

`GET /security/health` reports:

- **Collector state:** `role` (`collector`, `reader` or `stopped`), `lock_held`,
  `collector_pid`, `started_at`, `stopped_at`, `heartbeat` (written every 15 s)
  and `stale` (heartbeat older than 45 s, or the collector stopped).
- **Source:** `source`, `source_error` (including limited journal access) and
  `cursor`.
- **Events:** `last_event_at`, `last_seq`, `event_count`, `gap_count`,
  `last_gap_at` and `authorized_keys_error`.
- **Disk:** `free_bytes` and `low_space` (under 256 MiB).
- **Alerts:** `alert_backlog`, `alert_last_error`, `alert_last_delivered_at` and
  `webhook_configured`.

## In-process mode (default)

Every `apteva-host` starts the collector in its own process. To change this, set
`APTEVA_SECURITY_COLLECTOR` in the `apteva-host` environment:

- `reader`: never take the lock; only serve the shared store.
- `off`: no collector; the `/security/*` routes return 503.

In-process collection inherits the host's privileges, which is root on a
typical production machine.

## Standalone mode (recommended for production)

`apteva-host security-collector` runs only the collector. It needs no
installation home, so it can run as its own user with only the access it needs:

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin apteva-security
install -d -o apteva-security -g apteva-security -m 0700 /var/lib/apteva-security
# Use a stable copy, not a per-version path that updates can prune.
install -m 0755 ~/.apteva/bin/<version>/apteva-host /usr/local/lib/apteva/apteva-host
```

`/etc/systemd/system/apteva-security-collector.service`:

```ini
[Unit]
Description=Apteva SSH access collector
After=systemd-journald.service network-online.target
Wants=network-online.target

[Service]
Type=simple
User=apteva-security
Group=apteva-security
SupplementaryGroups=systemd-journal adm
ExecStartPre=+/bin/chown -R apteva-security:apteva-security /var/lib/apteva-security
ExecStart=/usr/local/lib/apteva/apteva-host security-collector
Environment=APTEVA_SECURITY_DIR=/var/lib/apteva-security
# Optional seed policy (only used while policy.json does not exist):
# Environment=APTEVA_SECURITY_POLICY_FILE=/etc/apteva/ssh-policy.json
StateDirectory=apteva-security
StateDirectoryMode=0700
# Read /root/.ssh and /home/*/.ssh/authorized_keys; no other privilege.
AmbientCapabilities=CAP_DAC_READ_SEARCH
CapabilityBoundingSet=CAP_DAC_READ_SEARCH
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/var/lib/apteva-security
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload
systemctl enable --now apteva-security-collector
journalctl -u apteva-security-collector -n 20
```

Then set `APTEVA_SECURITY_COLLECTOR=reader` in every `apteva-host` unit on the
machine (for example `systemctl edit apteva`), so a root host never becomes the
writer while the standalone unit is restarting. This keeps all files in the
directory owned by `apteva-security`. Root hosts read the shared store directly.

Membership of `systemd-journal` (or `CAP_DAC_READ_SEARCH`) lets `journalctl`
read the system journal. `adm` covers `/var/log/auth.log` on Debian and Ubuntu.
If journal access is limited, `source_error` in health says so.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `APTEVA_SECURITY_DIR` | see above | Collector directory |
| `APTEVA_SECURITY_COLLECTOR` | `on` | `on`, `reader` or `off` (in `apteva-host`) |
| `APTEVA_SECURITY_POLICY_FILE` | unset | Seed policy JSON |
| `APTEVA_SECURITY_BACKFILL` | `168h` | First-run journal/log backfill (Go duration) |
| `APTEVA_SECURITY_RETENTION_DAYS` | `365` | Maximum event age |
| `APTEVA_SECURITY_MAX_EVENTS` | `500000` | Maximum stored events |
| `APTEVA_SECURITY_AUTHORIZED_KEYS` | defaults above | Colon-separated globs, or `none` |
