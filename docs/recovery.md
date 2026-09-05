# Platform recovery archives (format 2)

`GET /api/platform/snapshot` requires platform administration. It captures the
server SQLite database, persistent data for locally supervised app installations
(including stopped installations), agent directories, and managed MCP source.
The manifest lists archive roots and every file's SHA-256 digest and size.
SQLite files are captured with online VACUUM INTO. Each database is internally
consistent; separate databases and ordinary files are captured at different
instants while the platform continues running. This is not a cross-process
transaction or a filesystem snapshot.

By default the archive records the encryption key fingerprint and requires the
original key for recovery. Keep that key separately and securely. To make an
archive recoverable with a passphrase, supply `X-Backup-Passphrase` (at least 12
characters) when downloading it. The archive then contains a random-salt scrypt
key derivation and an AES-GCM-wrapped encryption key. It never contains the raw
key. Keep the passphrase separately from the archive. A download without this
header still requires the original encryption key on the destination.

Restore with `POST /api/platform/restore`, an authenticated administrator,
`X-Confirm-Restore: yes`, and the archive as the request body. Supply the same
`X-Backup-Passphrase` if the destination uses a different encryption key.
A configured `SERVER_SECRET` must match the restored key. Restore validates
file inventory, digests, SQLite integrity, identities in the archived database,
and key compatibility before publishing a durable activation plan.

Format 2 restore does not replace live app files. It stages sibling files and
directories on their destination filesystems and returns `restart_required`.
Stop the server and its locally managed runtimes normally, then restart it.
Activation runs before opening the database or starting runtimes. Existing
files/directories are preserved beside their replacements as `.prerecovery-*`
backups. If activation is interrupted, startup resumes the staged plan; an
activation failure stops startup rather than exposing partially restored state.
After successful activation and verification, remove old backups manually when
no longer needed. They may contain sensitive historical data.

The archive is portable across server/app cache directory changes. It restores
archived installation IDs rather than looking up those IDs in the destination's
old database. Agent process IDs, ports, and runtime keys are cleared and rebuilt
through normal startup. App binaries/build caches are not data backups and must
remain available or be rebuilt. External sidecar storage, files outside managed
roots, remote services, system service configuration, and TLS configuration
outside these roots need their own recovery process. Symlinks and special files
inside captured roots cause the snapshot to fail explicitly; their targets are
not silently followed or omitted.

Format 1 archives remain accepted for legacy database/source restoration. They
lack the complete inventory and key envelope of format 2 and must not be treated
as portable disaster-recovery archives. Make a format 2 archive before relying
on fresh-machine recovery.

Archive limits are 10,000 files (including the manifest), a 4 MiB manifest,
128 GiB per file, 256 GiB expanded, and 64 GiB compressed upload. Snapshot
generation rejects inventories exceeding the file/expanded limits before
streaming an archive. Restored files are private to the server's OS user and
preserve executable bits.
