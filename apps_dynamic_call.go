package main

import (
	"log"
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

// isOfficialCaller reports whether m is an official Apteva app —
// its runtime.source.repo matches one of the configured trusted
// prefixes. Apps installed as binaries or container images have no
// source repo and are NOT considered official by this function; if
// a binary-shipped app ever needs dynamic-call access, add a
// binary-source / image-digest check alongside this one.
func isOfficialCaller(m *sdk.Manifest) bool {
	if m == nil || m.Runtime.Source == nil {
		return false
	}
	repo := m.Runtime.Source.Repo
	if repo == "" {
		return false
	}
	for _, p := range officialAppPrefixes() {
		if strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

// installProjectID returns the project_id for an install row. Empty
// string for global-scope installs (and on lookup failure, treated
// as global by downstream consumers — the conservative read).
func installProjectID(s *Server, installID int64) string {
	var pid string
	_ = s.store.db.QueryRow(
		`SELECT COALESCE(project_id, '') FROM app_installs WHERE id = ?`,
		installID,
	).Scan(&pid)
	return pid
}

// resolveDynamicTarget is the cross-app-call gate's bypass path.
// Consulted by the gate when installBoundAppID returned 0. Returns
// the target install_id on success and emits an audit log; on
// failure, returns the appropriate 403 message so the consumer's
// error distinguishes "not eligible" from "eligible but target
// absent".
func (s *Server) resolveDynamicTarget(callerInstallID int64, targetAppName string) (int64, string, bool) {
	callerMan, err := installManifest(s, callerInstallID)
	if err != nil || callerMan == nil {
		return 0, "app not bound: " + targetAppName, false
	}
	if !callerMan.Requires.DynamicAppCalls || !isOfficialCaller(callerMan) {
		return 0, "app not bound: " + targetAppName, false
	}
	callerProject := installProjectID(s, callerInstallID)
	id, ok := s.findInstalledApp(targetAppName, callerProject)
	if !ok || id == 0 {
		scope := "globally"
		if callerProject != "" {
			scope = "project " + callerProject + " or globally"
		}
		return 0, "app not reachable: " + targetAppName + " (no install in " + scope + ")", false
	}
	log.Printf("[APPS-CALL] dynamic caller=%s caller_install=%d project=%s target=%s target_install=%d",
		callerMan.Name, callerInstallID, callerProject, targetAppName, id)
	return id, "", true
}
