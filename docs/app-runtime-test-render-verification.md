# Generic remote deployment: test-render verification

Verified on 2026-10-13 from the local dev Server. Server changes remain local;
SDK v0.101.0, Instances 0.7.3 and Telephony 0.12.15 are published releases.

The install request selected the Telephony release and namespaced target
`{app_install_id: 8, resource_type: instances.instance, resource_id: "1"}`.
Server selected the backend, Linux amd64/GNU platform and configured gateway,
built/delivered/sealed the package, and verified readiness and lifecycle metadata.
Ordinary carrier bindings remain separate from execution placement. No Instances
or Domains binding was added to Telephony. No public SIP/WebRTC configuration or
carrier calls were performed.

- Test project: `Telephony remote deployment test-render` (`1791897705704-da944eef99d429dc`).
- Installation: `52824`; final generation: `4`;
  final state: `active`.
- Package: `pkg-d9efdebcde41ac3d33088f996f969c556ac6184f974b2301bd91f546d0c76268`; 125 files.
- Executable SHA-256: `a4c9cdc67ea758d21b41f6a07a1b459fef2f698478555c5ab5219b621f748c76`.
- Duplicate backend starts left one process.
- Actual interrupted handoff recovered with the same package identity/checksum.
- Another generation reused the same package.
- Local Server and Instances restart recovery preserved the remote execution.
- An injected failure affected only a newly prepared test executable. The
  controller confirmed candidate shutdown and restored the previous verified
  release in a fresh execution/credential generation, retaining data.
- Stop confirmed no owned process remained; delete removed execution units and
  retained the exact database/marker hashes. Restart reused the package/data and
  left Telephony active on test-render.
- All 23 pre-existing running services survived;
  both pre-existing remote Apteva Server PIDs remained unchanged.
- Existing local Telephony installations 692 (0.8.1) and 1015 (0.10.3) stayed
  running; all five local user agents recovered. Production was not accessed.

Interrupted HTTP upload recovery is additionally covered by Server/Instances
fixtures; the real host test interrupted a persisted deployment handoff rather
than deliberately dropping packets during a file upload. SDK full tests,
Instances full short race tests plus later patch regressions, Telephony full
short tests/managed runtime race tests, and Server managed deployment/package/
rollback/retry regressions pass. Unsupported cross-CGO remains an explicit
builder requirement; successful Go cross-compilation does not provision host
libraries. Unusual app requirements belong in release metadata.

## HTTPS test on 2026-10-13

Domains installation 3 created a new `phone-test-render.apteva.dev` A record through the existing Porkbun connection 14, pointing to `152.42.133.147`. Existing DNS records were preserved. A separate Caddy 2.11.7 service (`apteva-test-ingress-52824.service`) owns only this test ingress and its files under `/var/lib/apteva-test-ingress-52824`. Its downloaded archive was verified against the official release SHA-512 checksum. HTTPS is trusted, issued by Let's Encrypt, and only `/runtime-browser/*` is proxied to the app. Private runtime controls and MCP return 404.

Instances 0.7.4 supplies optional generic `browser_endpoint` and named `endpoints` options. Server needed no further changes. Generation 5, deployment `R6JAD35PN3VEHRU53JPCEJD7WW`, advertises `https://phone-test-render.apteva.dev/runtime-browser`; its current private HTTP port is 55103. The retained verified package is reused. Authenticated browser reads succeed; missing credentials and foreign origins fail. One app process remains, the data marker is unchanged, and existing remote Server processes remain running.

The local static phone page is installation 52825 at `https://apteva.shares.zrok.io/test-phone/`, sourced from `/Users/marcoschwartz/Documents/code/telephony-test-phone`. It uses Telephony's existing headless client in direct mode with PCM audio and normal dashboard sign-in, without embedding credentials. Browser rendering/sign-in handling is verified. Actual carrier dialing, microphone capture and end-to-end audio have not been tested.

This is separately provisioned test ingress, not automatic DNS/TLS lifecycle support. If a replacement deployment changes the private HTTP port, update this owned Caddy upstream. The direct browser origin policy currently requires the configured Apteva public origin; localhost alone is insufficient, so the local page is served through the existing local Server HTTPS gateway.
