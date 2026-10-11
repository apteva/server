# Exclusive app activation

`runtime.activation_strategy` supports absent/`auto` and `exclusive`. The
local supervisor requires exclusive replacement if either the running or
candidate manifest declares it, or either owns declared fixed host ports. This
is generic and applies per installation, including services with listeners
chosen from app configuration. There is no app-name or SIP-port special case.

The committed manifest stays authoritative during preparation. The old process
serves while the candidate is downloaded or built. At exclusive cutover the
server persists `pending` and a temporarily unavailable status message, removes
the old route, and requires confirmed process termination/reaping before spawning
the candidate. A stop failure aborts replacement and retains the process handle.
Fixed-port reservations remain held by the installation through cutover.

The candidate must pass HTTP readiness and known fixed TCP listener probes
within its startup deadline. Required app-configured or UDP listeners must gate
app HTTP readiness. An app can bind them during SDK `OnMount`; all app routes
remain gated until mounting completes. Ordinary HTTP-only apps retain the
existing overlapping update path.

On failure, the candidate must be stopped before the previous binary can restart.
Rollback restores its captured environment, HTTP port, health path and listener
declarations, then uses the previous version's startup deadline (30 seconds if
undeclared). Only verified readiness permits reporting the previous version as
running. An unconfirmed candidate stop prevents rollback and retains its port
reservations. `database_upgrade: requires_restore` remains blocked; binary
rollback does not undo committed database changes.

No new table is needed. Committed and pending manifests preserve the policy.
The candidate PID is recorded during exclusive startup while the committed
binary/manifest/port remain unchanged. Boot recovery kills a surviving candidate
before reconstructing the committed process, then retries the staged target.
Pending manifests on otherwise running rows are also recovered, covering a
server exit during download/build. PID persistence and process spawning are
separate operations: a hard crash between them still leaves a small orphan
window; a process supervisor that kills its whole process group on exit avoids
surviving children in that window.

Declared fixed ports retain installation ownership/conflict checks. Undeclared
app-configured ports cannot be reserved by the server; separate installations
must use distinct ports. Exclusive updates interrupt active sessions.

`min_apteva_version` is checked before install admission and local activation,
including manifests restored from JSON. Development builds validate minimum
version syntax but have no release version for comparison. Remote/manual mounts
cannot guarantee this policy and remote install admission rejects `exclusive`.
Support starts in app SDK `v0.100.0` and Apteva `0.87.4`. Deploy the supporting
server before releasing an opting-in app and declare `min_apteva_version: 0.87.4`.
Older servers may ignore the new field and the minimum-version declaration.

Regression coverage includes configured listeners, old/new policy transitions,
HTTP-only updates, startup and rollback failures, previous startup budgets,
confirmed stop enforcement, fixed-port conflicts, preparation/cutover routing,
and pending recovery before and after old termination and candidate startup.
