# Prebuilt native apps

Complete native packages are an optional delivery mode for any service/source
app. The same installer handles onboarding dependencies, marketplace installs,
upgrades, and exact-version runtime recovery. Apteva does not bundle app binaries.

```yaml
runtime:
  kind: source
  port: 8080
  source:
    repo: github.com/your-org/apps
    ref: <full-release-commit>
    entry: mcp/your-app
  artifacts:
    linux-amd64:
      url: https://example.com/releases/your-app-1.2.3-linux-amd64.tar.gz
      sha256: <64-hex-characters>
```

Selection is deterministic: use the host OS/architecture artifact when present;
reuse a verified local cache before downloading. Without a matching artifact,
use the existing source path (or the service's legacy binary delivery). A
selected artifact's download, checksum, extraction, manifest, or resource error
is reported; it never silently compiles another build. Local paths and file://
URLs support development. Publish HTTPS URLs and immutable versioned assets.

A gzip tar archive contains:

```
bin
src/mcp/your-app/apteva.yaml
src/mcp/your-app/ui/...
src/mcp/your-app/migrations/...
src/mcp/your-app/skills/...
```

`src/<runtime.source.entry>` is the app resource root; without source.entry it
is `src`. Include optional plugin.json/mcp.json and every runtime resource.
The packaged manifest must match the distribution manifest except for
runtime.artifacts, which is added after packaging to avoid a circular checksum.
Include precompiled UI bundles; no frontend build tools run during installation.
All declared local UI entries, icons, skills, and migrations must be present.
Native helper binaries declared in requires.binaries retain their existing
installation behavior.

Downloads are SHA-256 checked before extraction. Extraction rejects traversal,
links, duplicate files and special entries, with compressed/expanded size and
entry-count limits. A per-file receipt verifies cache contents on reuse and
restart. Packages are staged and atomically installed under
`<apps>/<name>/<version>/artifacts/<sha256>`. They skip compiler build slots.

Persistent data remains at `<apps>/<name>/data/<install-id>` across delivery
modes and versions. Config, install ID, bindings, permissions, migrations, health
checks and process handoff use the existing lifecycle. As with source updates,
reverting a binary does not undo database migrations already committed by it.

Conversations' release workflow and packaging instructions live in
`apps/docs/conversations-prebuilt-releases.md`. Only its registry manifest URL
needs to switch after its artifact release is available. Other apps can adopt
this manifest contract later without installer changes.
