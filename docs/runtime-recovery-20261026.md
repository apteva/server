# Managed-runtime recovery hardening (26 October 2026)

These local Server changes accompany Telephony 0.12.52 and Instances 0.14.2. Publishing the apps does not deploy this controller or change production.

- Keep admission distinct from startup health. Authenticated status must match the deployment ID/generation. Only a fenced, freshly verified current owner explicitly requesting `recovery.action: restart` is eligible. Keep atomic owner checks, a single active execution and persisted cooldowns.
- Probe a newly inspected control endpoint even if the previous endpoint failed and startup readiness is false. Carry backend's separate diagnostic health fields without treating them as ownership authority.
- For same-target exclusive replacement, validate the package first, drain the old execution, confirm stop, then call backend preparation. This avoids backends' legitimate exclusive state/service conflicts. Failed preparation after stop uses the existing rollback/replacement machinery.
- A freshly verified fenced predecessor does not wait for stale active-call database rows to drain. Confirmed backend shutdown remains mandatory before starting its replacement.
- Drain checks operation cancellation/ownership throughout its retry loop and cancels both app HTTP and remote backend inspection. Refresh control addresses from identity-checked inspection; a backend-confirmed stopped process needs no app drain. Unreachable control and unfinished work are separate progress phases.
- Persist `running` immediately and expose start/phase timestamps, elapsed milliseconds, retry count and retained last failure. Existing commit/retire/rollback cancellation restrictions remain.

## Local validation

Run with `GOWORK=off` because the developer workspace currently has conflicting unrelated app-sdk workspace replacements:

```
go test -run 'TestManagedRuntime|TestRuntime' -count=1 .
go test -race -run 'TestManagedRuntime(AutomaticFencedRecovery|FencedRecovery|Recovery|Drain|ExclusiveRestart|Monitor)' .
go build -o /private/tmp/apteva-server-runtime-reliability .
```

New tests cover a backend returning 409 when preparation overlaps an existing writer, prompt cancellation of blocked drain HTTP, fresh control endpoints, confirmed stopped execution, and fenced recovery with stale call rows. Existing tests cover current-owner authentication, cooldowns, stale generation rejection, confirmed shutdown and recovery rollback. Telephony's companion test opens a real UDP SIP listener and verifies expiry tears it down and only the replacement reopens it. These are local component/contract tests, not a live cross-provider outage rehearsal.

The unrelated pre-existing Server workspace changes were preserved. A baseline and task-only patch were saved under `/private/tmp/runtime-reliability-baseline-20261026` and `/private/tmp/server-runtime-reliability-20261026.patch`.
