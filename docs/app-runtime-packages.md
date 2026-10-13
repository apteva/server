# Automatic managed app packages

Apteva 0.90.0 can prepare and deliver binary packages through an optional extension to
`apteva.runtime-backend/v1`. Backend app names and resource-type strings have no
special meaning to the package pipeline. The SDK release metadata and backend target inspection are generic contracts;
no backend or app name selects the package pipeline.

## Automatic operator flow

Select the exact app release and namespaced target. Backend selection, target
platform, ABI, gateway configuration, build, delivery and activation are automatic:

```json
{
  "manifest_url": "https://raw.githubusercontent.com/apteva/apps/telephony/v0.12.15/mcp/telephony/apteva.yaml",
  "project_id": "test-project",
  "bindings": {"carrier": 123},
  "placement": {
    "type": "backend",
    "target": {"app_install_id": 8, "resource_type": "instances.instance", "resource_id": "1"}
  }
}
```

Ordinary app integration bindings still apply; they do not select the execution
backend. Existing installations are never migrated implicitly. Server discovers
providers from their declared protected routes at startup, periodically, on
catalog reads and when resolving a selected target. A unique compatible backend
is selected automatically; ambiguous backends require an explicit `backend_id`.
The gateway comes from this Server's configured public address, never an
untrusted request Host header. Inspection verifies reachability from the host.

A backend advertising `target_inspection: true` implements protected POST
`inspect-target`, using the ordinary request without an artifact and returning
`{platform,host_key,abi,system_libraries,formats}`. Inspection never creates an
execution or changes host configuration. Server persists host identity/platform
before build and checks them again during prepare/start. Instances 0.7.1
implements this generic contract. It also accepts verified archive-to-package
handoffs while preserving the approved archive checksum separately from the
executable checksum.

SDK v0.101.0 adds optional validated `runtime.package`. Standard Go apps use the
source entry, declared UI/migrations/skills, optional `native/` directory and
existing `build:ui`/`build:frontend` script. Lifecycle capabilities are declared
once per release and verified against the authenticated running app. Unusual
files/build requirements are release-owned exceptions. Legacy
`apteva.runtime-package.json` remains supported; dual metadata sources fail.
ELF-derived ABI/library requirements are sealed and checked against the host
before upload, prepare and start. Explicit release constraints remain enforced.
Unsupported CGO requires a compatible builder or verified prebuilt archive;
host discovery does not create a cross compiler.

The lower-level overrides below remain for legacy backends and administrator
use; they are not required by the automatic flow.

## Backend registration

A backend can advertise:

```json
{
  "package_delivery": {
    "protocol": "apteva.runtime-package/files-v1",
    "formats": ["binary"]
  }
}
```

Server currently prepares this extension's `binary` format. Other execution
formats and delegated backend preparation retain their existing behavior.
Registration rejects a delivery format that the backend does not advertise.

An administrator may explicitly attest the same extension when registering a
legacy backend whose protected endpoints already implement it. This enables
Instances 0.7.0 without changing that app:

```http
POST /api/apps/execution-backends
Authorization: Bearer <server-admin-credential>
Content-Type: application/json

{
  "app_install_id": 52,
  "id": "native",
  "endpoint": "/_apteva/runtime/backends/native",
  "package_delivery": {
    "protocol": "apteva.runtime-package/files-v1",
    "formats": ["binary"]
  }
}
```

IDs and gateway URLs below are examples, not discovered host identities. Server
checks the live descriptor and confirmed-stop guarantee. The optional extension
is explicitly declared, never inferred from an app name or a `binary` format
alone. Automatic discovery works without an override once a backend advertises
the extension itself. Public app proxies still block private control paths.

## Placement and release metadata

The existing install API's `placement` and existing
`POST /api/apps/installs/:id/deployment/prepare` accept:

```json
{
  "placement": {
    "type": "backend",
    "backend": {"app_install_id": 52, "backend_id": "native"},
    "target": {
      "app_install_id": 52,
      "resource_type": "instances.instance",
      "resource_id": "6"
    },
    "gateway_url": "https://owning-server.example.com",
    "package": {
      "platform": "linux-amd64",
      "app_capabilities": {
        "passive_start": true,
        "drain": true,
        "replica_safe": false,
        "worker_coordination": false,
        "routing": "key"
      },
      "files": ["native"],
      "abi": "gnu",
      "system_libraries": ["libc.so.6", "libdl.so.2", "libpthread.so.0"]
    }
  }
}
```

For the first Telephony trial, select its approved **0.12.14 source manifest**
(`telephony/v0.12.14`, entry `mcp/telephony`) and a fresh installation. The
capabilities above are that exact release's declaration. Telephony does not add
Instances or Domains integration bindings. Server performs the build/upload;
there is no manual package-staging step.

For legacy backends without target inspection, target platform is explicit. A backend's supported-platform list is not proof of
the selected host's architecture; its `validate-target` must independently check
the package against the physical target. Instances 0.7.0 requires a sealed package
before that validation, so artifact-independent automatic probing is separate
work. The gateway must reach the Server owning this deployment. Server injects
the same URL into both the backend request/options and `APTEVA_GATEWAY_URL`;
conflicting manually supplied options are rejected.

Release authors can commit `apteva.runtime-package.json` beside `apteva.yaml`:

```json
{
  "app_capabilities": {
    "passive_start": true,
    "drain": true,
    "replica_safe": false,
    "worker_coordination": false,
    "routing": "key"
  },
  "files": ["native"],
  "build": [
    {"args": ["bun", "install", "--frozen-lockfile", "--ignore-scripts"]},
    {"args": ["bun", "run", "build:frontend"]}
  ]
}
```

The descriptor is read from the exact pinned revision. An administrator may
supply equivalent fields under `placement.package`; those fields are persisted
as an explicit release declaration, not guessed from an existing running app.
Optional `source_revision` must equal the resolved full commit. A managed app
advertising lifecycle routes must supply capabilities. Ordinary apps default to
all capabilities disabled. A declaration does not prove app safety: Server
checks the authenticated running descriptor before readiness/activation.

Declared preparation steps run in order in the app directory; optional `dir` is
relative to it. They use argv arrays, not shell strings. `bun` and `go` are the
supported preparation tools. Without explicit steps, Server recognizes the
released root package's `build:ui` or `build:frontend` script, installs its frozen
dependencies without install scripts, and runs that script. Other build systems
need a supported builder contract; Server does not invent a command.

## Build and cache guarantees

Source resolution, target platform and build inputs are journaled before build.
Source compilation never starts the app. It uses `GOWORK=off`, an isolated child
environment, a resolved Go toolchain, target GOOS/GOARCH and `-trimpath`.
Installation configuration, execution credentials and Server environment are not
passed to build scripts or included in packages. Script output is not persisted;
failed steps report their phase. Existing local source builds keep their original
environment and startup behavior.

The native builder supports Linux amd64/arm64 **CGO-disabled Go builds**. It
validates ELF architecture, interpreter and linked library requirements. Dynamic
executables require an explicit `abi` (`gnu` or `musl`) and approved
`system_libraries`; actual requirements are stored in a sealed runtime descriptor
and passed to the backend in the package reference. The chosen target must supply
that interpreter/ABI/library stack; Server does not provision it. Instances 0.7.0
does not independently negotiate these new ABI fields, so its target must already
satisfy them and startup/readiness plus staging runtime tests remain essential.
Telephony's CGO-disabled Linux build is dynamically linked, so the first GNU/Linux
trial needs the explicit declaration shown above.
`cgo: true` fails explicitly: a cross compiler and an appropriate target ABI/library
contract or another target-native builder are required. Purego/dlopen libraries
must be supplied by the exact release (embedded or declared runtime files);
successful cross-compilation alone is not a runtime library conformance test.

Packages include the executable, release manifest, UI directory, declared database
migrations, declared skill files and recipe runtime paths. Paths must be relative,
regular files; hidden components, symlinks, traversal and special files are
rejected. Every file has a size and SHA-256. Package limits match the initial
delivery contract: 1,024 files, 256 MiB per file, 512 MiB total.

Build lookup keys include approved manifest, exact source commit/archive digest,
platform, recipe, builder version and toolchain versions. Package IDs hash the
canonical full file/capability descriptor. Concurrent requests use cross-process
file locks to deduplicate builds and delivery. Different build keys can reference
the same content package. Different backend installations have separate delivery
receipts. Recovery verifies cached bytes; corrupted retained packages fail rather
than being silently rebuilt under the same identity.

The immutable package cache is bounded to 2 GiB. Deployment references pin active,
pending and retained rollback packages; GC removes only unreferenced packages and
fails if retained packages exhaust capacity. Two preparation workspaces can run
concurrently; abandoned workspaces are recovered using process-owned file locks.
Go/module/compiler caches are separate derived caches. Each backend retains its
own staging budget and cleanup policy; Server does not assume unlimited staging.

## Delivery and recovery

All artifact routes are relative to the **registered private backend endpoint**:

1. Persist the package ID, platform, executable digest and full immutable metadata
   in the deployment.
2. `POST /artifacts` with `id`, `app_name`, `app_version`, `platform`, `executable`,
   `files: [{path,size,sha256}]`, and `app_capabilities`.
3. Try `POST /artifacts/:id/seal`. A matching already sealed package is reused.
4. If files are incomplete, stream each `PUT /artifacts/:id/files/:path`, verify
   its byte-count/checksum receipt, then seal and verify its acknowledgment.
5. Only then call `validate-target` and `prepare`, supplying `options.artifact_id`,
   `artifact_platform`, and the generic package reference.
6. Compare returned platform, executable digest and lifecycle capabilities with
   the persisted package. Continue the existing start/readiness/activation flow.

Delivery uses the backend installation's current private credential. Redirects
are forbidden. Each streaming request has a ten-minute budget, with a thirty-minute
overall handoff budget; ordinary thirty-second lifecycle RPCs remain unchanged.
Responses are bounded and raw provider errors do not expose tokens or bodies.
Retries repeat whole files, not partial byte ranges. Stable IDs and verified seal
acknowledgments handle interrupted uploads, lost responses and backend staging
loss. Existing explicitly supplied `options.artifact_id` handoffs remain usable
without automatic package preparation.

The additive package reference separates full package identity, executable SHA-256
and original archive SHA-256. Backend support for verified archive conversion must
be explicit (`verified_archives: true`). Server verifies/extracts the selected
target-platform archive and retains its original manifest and checksum. Instances
0.7.0 rejects archive manifests, so that format remains rejected for that backend;
Server never strips the verification or compares an archive hash to an executable
hash. Source-manifest binary packages work with its existing endpoints.

## Verification and scope

Local tests cover generic endpoint/credential use with two backend IDs, actual
Linux cross-compilation, build/upload deduplication, interrupted upload and durable
reopen/recovery, backend staging loss, duplicate requests, retained-cache GC,
checksum/platform/capability mismatch, private build environments and unchanged
legacy runtime behavior.

An optional `APTEVA_RUNTIME_PACKAGE_RELEASE_TEST=1` test prepares Telephony 0.12.14
and uploads only to a loopback backend fixture. It never starts Telephony or uses
a carrier/VPS/production installation. Real DigitalOcean execution, systemd
recovery and SIP/WebRTC runtime/library conformance remain separate staging
acceptance checks. This increment performs no migration, replication, public media
setup or production deployment.

## Lifecycle recovery and retained data

Restart keeps the persisted backend/target. Delete confirms owned processes and
listeners are stopped, removes execution resources and clears stale active or
pending pointers; the installation's placement and backend-owned data remain.
A later deployment can reuse the package and retained installation data.

Remote rollback creates a fresh execution and credential generation for the
previous pinned release. It never assumes a stopped handle is restartable, and
journals failed/recovery deployment identities before preparation. Recovery uses
the original package and state reference, verifies readiness and commits routing
only after activation. Existing database restore restrictions still apply.
SDK/source tags and package inputs do not change during recovery.
