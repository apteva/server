package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// Dynamic cross-app call bypass.
//
// The MCP gate in apps_callbacks.go normally rejects any cross-app
// call whose target isn't in the caller's integration_bindings map.
// That works for apps with a static, declarable set of cross-app
// deps (jobs targeting other apps' HTTP routes, deploy calling
// certs, etc.) but breaks for generic-FaaS-style apps whose targets
// are user-supplied at runtime — functions, future workflow runners.
//
// The bypass lets such an app reach any installed sibling, gated by
// two independent axes:
//
//   1. Capability declaration in the caller's manifest:
//      requires.dynamic_app_calls: true  (sdk.Requires.DynamicAppCalls)
//   2. Trust: the caller is identified as official — its
//      runtime.source.repo matches one of officialAppPrefixes(),
//      which defaults to "github.com/apteva/" and can be extended
//      via APTEVA_OFFICIAL_APP_PREFIXES.
//
// Either alone is a no-op: a third-party app that flips the flag
// gains nothing; an operator that adds a prefix doesn't grant access
// to an app that hasn't declared the intent. Default off — apps
// without the flag stay on the strict gate.
//
// This is explicitly a stopgap: a proper per-call permission model
// (per-function allowed_apps, target-side opt-out, signed manifests,
// etc.) supersedes this whenever it lands.

// officialAppPrefixes returns the source-repo prefixes that mark a
// caller as official. Default github.com/apteva/;
// APTEVA_OFFICIAL_APP_PREFIXES extends it (comma-separated). Read
// per call so tests using t.Setenv work without bouncing the
// process.
func officialAppPrefixes() []string {
	v := strings.TrimSpace(os.Getenv("APTEVA_OFFICIAL_APP_PREFIXES"))
	if v == "" {
		return []string{"github.com/apteva/"}
	}
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"github.com/apteva/"}
	}
	return out
}

// isOfficialCaller reports whether m is an official Apteva app: the code
// the server runs for it comes from a trusted location.
//
// The manifest names where its code comes from, and anyone can write a
// manifest, so every location the server could take runnable code from has
// to be trusted, not just runtime.source.repo:
//
//   - runtime.source.repo matches one of the trusted prefixes, and
//   - every prebuilt artifact, binary and bundle the manifest lists is
//     an https URL under one of them.
//
// A prebuilt artifact takes precedence over building from source, so
// without the second rule a manifest could claim an official repo while
// the server downloads and runs something else. Apps with no source repo
// are NOT considered official by this function; if a binary-only app ever
// needs dynamic-call access, add a digest check alongside this one.
func isOfficialCaller(m *sdk.Manifest) bool {
	if m == nil || m.Runtime.Source == nil {
		return false
	}
	prefixes := officialAppPrefixes()
	if !officialLocation(m.Runtime.Source.Repo, prefixes) {
		return false
	}
	for _, artifact := range m.Runtime.Artifacts {
		if !officialDownload(artifact.URL, prefixes) {
			return false
		}
	}
	for _, binary := range m.Runtime.Binaries {
		if !officialDownload(binary, prefixes) {
			return false
		}
	}
	if m.Runtime.Bundle != nil && !officialDownload(m.Runtime.Bundle.URL, prefixes) {
		return false
	}
	return true
}

// officialLocation reports whether a host/path location such as
// "github.com/apteva/apps" lies under a trusted prefix. Dot segments,
// encoded characters and backslashes are refused: a fetcher may resolve
// "github.com/apteva/../other/repo" to another owner's repository.
func officialLocation(location string, prefixes []string) bool {
	if location == "" || strings.ContainsAny(location, "%\\@?#") {
		return false
	}
	for _, segment := range strings.Split(location, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	for _, p := range prefixes {
		if strings.HasPrefix(location, p) {
			return true
		}
	}
	return false
}

// officialDownload reports whether a download URL is https and lies under
// a trusted prefix. Local paths and other schemes are never official.
func officialDownload(raw string, prefixes []string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Port() != "" {
		return false
	}
	return officialLocation(strings.ToLower(u.Host)+u.EscapedPath(), prefixes)
}

// installProjectID returns the project_id for an install row. Empty
// string for global-scope installs (and on lookup failure, treated
// as global by downstream consumers — the conservative read).
func installProjectID(s *Server, installID int64) string {
	metadata, err := s.appMetadata(installID)
	if err != nil {
		return ""
	}
	return metadata.project
}

// resolveDynamicIntegration is the integration-tool gate's bypass.
// Mirrors resolveDynamicTarget but for ExecuteIntegrationTool: when
// the connection isn't role-bound / owned / operator-installed, this
// is consulted before returning 403.
//
// Admits the call when (a) the caller's manifest declares
// requires.dynamic_integration_access, (b) the caller is identified
// as official, and (c) the connection's project_id matches the
// caller install's. Same defense-in-depth as the cross-app variant.
// Returns (true, "") to allow, (false, msg) otherwise — msg lets
// the caller surface a distinct diagnostic when the caller IS
// eligible but the project doesn't match.
func (s *Server) resolveDynamicIntegration(callerInstallID, connID int64, connProjectID, delegatedProjectID string) (bool, string) {
	callerMan, err := installManifest(s, callerInstallID)
	if err != nil || callerMan == nil {
		return false, ""
	}
	if !callerMan.Requires.DynamicIntegrationAccess || !isOfficialCaller(callerMan) {
		return false, ""
	}
	callerProject := installProjectID(s, callerInstallID)
	if callerProject == "" {
		// Global official runners carry the project selected by their
		// server-dispatched invocation. The caller app must overwrite this
		// field rather than forwarding handler input verbatim.
		callerProject = strings.TrimSpace(delegatedProjectID)
	}
	if connProjectID != callerProject {
		return false, fmt.Sprintf("connection %d is in another project (caller=%s, connection=%s)",
			connID, callerProject, connProjectID)
	}
	debugLogf("[INT-CALL] dynamic caller=%s caller_install=%d project=%s connection=%d",
		callerMan.Name, callerInstallID, callerProject, connID)
	return true, ""
}

// resolveDynamicTarget is the cross-app-call gate's bypass path.
// Consulted by the gate when installBoundAppID returned 0. Returns
// the target install_id on success and emits a debug trace; on
// failure, returns the appropriate 403 message so the consumer's
// error distinguishes "not eligible" from "eligible but target
// absent".
func (s *Server) resolveDynamicTarget(callerInstallID int64, targetAppName, delegatedProjectID string) (int64, string, bool) {
	callerMan, err := installManifest(s, callerInstallID)
	if err != nil || callerMan == nil {
		return 0, "app not bound: " + targetAppName, false
	}
	callerProject := installProjectID(s, callerInstallID)
	if callerProject == "" {
		callerProject = strings.TrimSpace(delegatedProjectID)
	}
	if !callerMan.Requires.DynamicAppCalls || !isOfficialCaller(callerMan) {
		// A caller that declares the target as a dependency is told when the
		// target simply is not installed, so an optional dependency can be
		// skipped instead of read as a permissions problem. Other callers
		// learn nothing about what is installed.
		if manifestDeclaresApp(callerMan, targetAppName) {
			if _, installed := s.findInstalledApp(targetAppName, callerProject); !installed {
				return 0, appNotInstalledPrefix + targetAppName, false
			}
			return 0, "app not bound: " + targetAppName + " (installed, but not linked to this app)", false
		}
		return 0, "app not bound: " + targetAppName, false
	}
	id, ok := s.findInstalledApp(targetAppName, callerProject)
	if !ok || id == 0 {
		scope := "globally"
		if callerProject != "" {
			scope = "project " + callerProject + " or globally"
		}
		return 0, "app not reachable: " + targetAppName + " (no install in " + scope + ")", false
	}
	// Without a project, findInstalledApp falls back to any default install,
	// including one scoped to some project. A dynamic caller has no binding
	// that chose that install, so it may only reach a global one, the same
	// rule resolveDynamicIntegration applies to connections.
	if callerProject == "" && installProjectID(s, id) != "" {
		return 0, "app not reachable: " + targetAppName + " (calls without a project reach only global installs)", false
	}
	debugLogf("[APPS-CALL] dynamic caller=%s caller_install=%d project=%s target=%s target_install=%d",
		callerMan.Name, callerInstallID, callerProject, targetAppName, id)
	return id, "", true
}

// Apps may rely on these: the message prefix of a refused app call and the
// X-Apteva-Error-Code header sent with it.
const (
	appNotInstalledPrefix  = "app not installed: "
	appCallErrorCodeHeader = "X-Apteva-Error-Code"
)

// appCallErrorCode maps a refusal message from resolveDynamicTarget to its
// stable code.
func appCallErrorCode(message string) string {
	switch {
	case strings.HasPrefix(message, appNotInstalledPrefix):
		return "app_not_installed"
	case strings.HasPrefix(message, "app not bound: "):
		return "app_not_bound"
	case strings.HasPrefix(message, "app not reachable: "):
		return "app_not_reachable"
	}
	return ""
}

// manifestDeclaresApp reports whether the manifest names the app as a
// dependency, required or optional.
func manifestDeclaresApp(m *sdk.Manifest, appName string) bool {
	target := normalizeAppName(appName)
	for _, dep := range m.Requires.Apps {
		if normalizeAppName(dep.Name) == target {
			return true
		}
	}
	for _, dep := range m.Requires.Integrations {
		if dep.Kind != "app" {
			continue
		}
		for _, slug := range dep.CompatibleSlugs {
			if normalizeAppName(slug) == target {
				return true
			}
		}
	}
	return false
}
