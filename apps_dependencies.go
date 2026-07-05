package main

// Dependency cascade for app installs.
//
// When an app's manifest declares `requires.apps`, the install handler
// walks the dependency graph and installs every missing app first
// (topological order, deps before dependents). Already-installed apps
// are skipped. Cycles are detected and rejected.
//
// Resolution: each dep is named (manifest.name); the registry tells
// us where to fetch its apteva.yaml. The cascade fetches the
// configured registry once, builds a name → manifest_url map, and
// uses that to resolve every dep recursively.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	sdk "github.com/apteva/app-sdk"
)

// installDependencies installs the required entries in
// `manifest.Requires.Apps` recursively. Optional deps are opt-in: they
// are only resolved when the operator supplied a non-null binding for
// that app name. Returns a map keyed by the parent's TOP-LEVEL dep names
// (not transitive grand-deps) → resolved install_id, so the caller can write
// `parent.bindings[depName] = installID`. Built-ins are satisfied
// without a binding entry (id=0); the caller skips writing those.
func (s *Server) installDependencies(userID int64, manifest *sdk.Manifest, projectID string, bindings map[string]any) (map[string]int64, error) {
	if len(manifest.Requires.Apps) == 0 {
		return nil, nil
	}
	if !appDepsNeedResolution(manifest.Requires.Apps, bindings) {
		return nil, nil
	}
	registryName2URL, err := s.loadRegistryNameMap()
	if err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	visiting := map[string]bool{} // cycle detection
	visited := map[string]bool{}  // already-resolved (installed or in this run)
	resolved := map[string]int64{}
	if err := s.installDepsRecursive(userID, projectID, manifest.Requires.Apps, bindings, registryName2URL, visiting, visited, resolved); err != nil {
		return nil, err
	}
	// Filter resolved down to the parent's top-level dep names — the
	// caller only writes those, since requires.apps is what
	// installBoundApp reads. Transitive grand-deps live on the dep's
	// own bindings (set by its own cascade run).
	out := map[string]int64{}
	for _, d := range manifest.Requires.Apps {
		if id, ok := resolved[normalizeAppName(d.Name)]; ok && id != 0 {
			out[d.Name] = id
		}
	}
	return out, nil
}

func appDepsNeedResolution(deps []sdk.RequiredAppRef, bindings map[string]any) bool {
	for _, dep := range deps {
		if !dep.Optional || appDepExplicitlySelected(dep.Name, bindings) {
			return true
		}
	}
	return false
}

func (s *Server) installDepsRecursive(
	userID int64,
	projectID string,
	deps []sdk.RequiredAppRef,
	bindings map[string]any,
	registryName2URL map[string]string,
	visiting, visited map[string]bool,
	resolved map[string]int64,
) error {
	for _, dep := range deps {
		key := normalizeAppName(dep.Name)
		if dep.Optional && !appDepExplicitlySelected(dep.Name, bindings) {
			log.Printf("[APPS-DEP] optional dep %q not selected — skipping", dep.Name)
			continue
		}
		if visited[key] {
			continue
		}
		if visiting[key] {
			return fmt.Errorf("dependency cycle detected involving %q", dep.Name)
		}

		// Already installed (or built-in)? Record the existing
		// install_id (0 for built-ins) and move on.
		if id, ok := s.findInstalledApp(dep.Name, projectID); ok {
			resolved[key] = id
			visited[key] = true
			continue
		}
		if info, ok := deprecatedApp(dep.Name); ok {
			return fmt.Errorf("dependency %q is deprecated and can no longer be installed: %s", dep.Name, info.Message)
		}

		// Resolve the dep's manifest URL via the registry.
		manifestURL := registryName2URL[key]
		if manifestURL == "" {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q not in registry — skipping", dep.Name)
				visited[key] = true
				continue
			}
			return fmt.Errorf("required dep %q not found in registry", dep.Name)
		}

		// Fetch + parse the dep's manifest.
		depManifest, err := s.fetchAndCacheManifest(manifestURL)
		if err != nil {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q manifest fetch failed: %v", dep.Name, err)
				visited[key] = true
				continue
			}
			return fmt.Errorf("fetch dep %q manifest: %w", dep.Name, err)
		}

		// Recurse into the dep's own deps before installing it
		// (topo order — leaves first).
		if len(depManifest.Requires.Apps) > 0 {
			visiting[key] = true
			if err := s.installDepsRecursive(userID, projectID, depManifest.Requires.Apps, bindings, registryName2URL, visiting, visited, resolved); err != nil {
				if dep.Optional {
					log.Printf("[APPS-DEP] optional dep %q sub-deps failed: %v", dep.Name, err)
					visiting[key] = false
					visited[key] = true
					continue
				}
				return err
			}
			visiting[key] = false
		}

		// Install the dep itself.
		newID, err := s.installAppFromManifest(userID, depManifest, projectID)
		if err != nil {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q install failed: %v", dep.Name, err)
				visited[key] = true
				continue
			}
			return fmt.Errorf("install dep %q: %w", dep.Name, err)
		}
		log.Printf("[APPS-DEP] installed %q (required by parent) install_id=%d", dep.Name, newID)
		resolved[key] = newID
		visited[key] = true
	}
	return nil
}

func appDepExplicitlySelected(name string, bindings map[string]any) bool {
	if bindings == nil {
		return false
	}
	raw, ok := bindings[name]
	return ok && raw != nil
}

// findInstalledApp resolves an app dep by name to a concrete install_id
// in a scope that satisfies the caller. Returns (0, true) for
// built-in framework apps (no install row, but the dep is satisfied).
// Returns (0, false) when nothing matches.
//
// Scope rules:
//   - projectID == "" (a GLOBAL parent resolving its dep): any
//     existing install of the dep satisfies — sidecar proxies are
//     keyed on app name (apps_mcp.go: /api/apps/<name>/mcp), so we
//     never need two sidecars of the same app just because the
//     parents have different scopes. Without this, installing a
//     global app whose deps already exist project-scoped spawns a
//     second sidecar per dep.
//   - projectID != "" (a PROJECT parent): prefer a same-project
//     install of the dep, fall back to a global install.
func (s *Server) findInstalledApp(name, projectID string) (int64, bool) {
	target := normalizeAppName(name)
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			if normalizeAppName(m.Slug) == target || normalizeAppName(m.Name) == target {
				// Built-in: no install row to point at, but the dep
				// is satisfied. The cascade treats id=0 as "satisfied
				// without a binding to write" — built-ins aren't
				// reachable via /api/apps/callback/apps so binding
				// them is a no-op anyway.
				return 0, true
			}
		}
	}
	// Same-project match preferred; global match accepted. ORDER BY
	// project_id DESC pulls the project-scoped row first since the
	// empty-string global rows sort lowest. status='running' so we
	// don't bind to a half-installed or errored sibling. Name match
	// happens in Go because normalizeAppName strips non-alphanumerics
	// and SQLite collations don't replicate that for a SQL filter.
	query := `SELECT i.id, a.name FROM apps a JOIN app_installs i ON i.app_id = a.id
			  WHERE i.status = 'running' AND (i.project_id = '' OR i.project_id = ?)
			  ORDER BY i.project_id DESC, i.id ASC`
	args := []any{projectID}
	if projectID == "" {
		query = `SELECT i.id, a.name FROM apps a JOIN app_installs i ON i.app_id = a.id
				 WHERE i.status = 'running'
				 ORDER BY i.id ASC`
		args = nil
	}
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			n  string
		)
		if rows.Scan(&id, &n) == nil && normalizeAppName(n) == target {
			return id, true
		}
	}
	return 0, false
}

// loadRegistryNameMap fetches the configured registry once and
// returns a name (normalized) → manifest_url map. Goes through the
// existing registry URL resolution (env override or default github
// raw) so behaviour matches handleMarketplace.
func (s *Server) loadRegistryNameMap() (map[string]string, error) {
	url := getRegistryURLFromEnv()
	if url == "" {
		url = defaultRegistryURL
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry %s returned %d", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	var reg struct {
		Apps []RegistryEntry `json:"apps"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range reg.Apps {
		if e.ManifestURL == "" {
			continue
		}
		out[normalizeAppName(e.Name)] = e.ManifestURL
	}
	return out, nil
}

// dependentsBlockingUninstall returns the human-facing names of every
// running install whose manifest hard-requires the install referenced
// by `installID`. Empty list (and nil err) means the uninstall is
// safe. Optional dependents don't block — they degrade silently when
// the dep goes away.
func (s *Server) dependentsBlockingUninstall(installID int64) ([]string, error) {
	// First, find the name of the app we're trying to uninstall.
	var targetName string
	err := s.store.db.QueryRow(
		`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id WHERE i.id = ?`,
		installID,
	).Scan(&targetName)
	if err != nil {
		return nil, err
	}
	target := normalizeAppName(targetName)

	// Walk every other running install's manifest looking for a hard
	// requires.apps entry that names the target.
	rows, err := s.store.db.Query(
		`SELECT a.name, a.manifest_json FROM apps a JOIN app_installs i ON i.app_id = a.id
		 WHERE i.id != ? AND i.status IN ('running', 'pending')`,
		installID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blockers []string
	for rows.Next() {
		var name, manifestJSON string
		if err := rows.Scan(&name, &manifestJSON); err != nil {
			continue
		}
		var m sdk.Manifest
		if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
			continue
		}
		for _, dep := range m.Requires.Apps {
			if dep.Optional {
				continue
			}
			if normalizeAppName(dep.Name) == target {
				blockers = append(blockers, name)
				break
			}
		}
	}
	return blockers, nil
}

// writeJSONStatus is writeJSON + an explicit HTTP status. Used by
// the uninstall handler to return 409 with a structured payload
// instead of a plain text error so the dashboard can pretty-print
// the dependents list.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// installAppFromManifest creates the apps + app_installs rows for a
// dep and dispatches to the right local-install path. Mirrors the
// happy-path branches of handleInstallApp without the HTTP wrapping.
// Used only by the dependency cascade — operator-driven installs
// still go through handleInstallApp for full validation + responses.
//
// Returns the new install_id so the cascade can record it in the
// parent's bindings JSON.
func (s *Server) installAppFromManifest(userID int64, manifest *sdk.Manifest, projectID string) (int64, error) {
	if info, ok := deprecatedApp(manifest.Name); ok {
		return 0, fmt.Errorf("%s is deprecated and can no longer be installed: %s", manifest.Name, info.Message)
	}
	manifestJSON, _ := json.Marshal(manifest)

	var appID int64
	err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, manifest.Name).Scan(&appID)
	if err != nil {
		res, ierr := s.store.db.Exec(
			`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'registry', '', '', ?)`,
			manifest.Name, string(manifestJSON))
		if ierr != nil {
			return 0, fmt.Errorf("create app row: %w", ierr)
		}
		appID, _ = res.LastInsertId()
	} else {
		s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE id = ?`,
			string(manifestJSON), appID)
	}

	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, config_encrypted, status, upgrade_policy, version, permissions_json, installed_by)
		 VALUES (?, ?, '', 'pending', 'manual', ?, ?, ?)`,
		appID, projectID, manifest.Version, string(permsJSON), userID)
	if err != nil {
		return 0, fmt.Errorf("create install row: %w", err)
	}
	installID, _ := res.LastInsertId()

	// Static and source paths both go through installLocally —
	// installLocally's kind=static branch handles UI-only apps
	// inline; the source path is delegated for kind=source. Service
	// kind goes via the orchestrator and is rare locally; we let
	// installLocally reject it gracefully there.
	switch manifest.Runtime.Kind {
	case "source":
		return installID, s.installFromSource(installID, manifest, projectID, nil)
	default:
		return installID, s.installLocally(installID, manifest, projectID, nil)
	}
}

// reconcileAppDepBindings backfills any missing requires.apps[].name
// bindings on a single install — for parents installed before the
// cascade learned to write them, or whose deps came online after the
// parent (out-of-order installs).
//
// Idempotent: a key already present in integration_bindings is
// preserved verbatim, even if it's nil. That matters for operators
// who explicitly set a dep's binding to null to disable an optional
// integration — a missing key is treated as "never set", a present
// nil key is treated as "deliberately unbound".
//
// Returns true if any keys were written (so callers can decide
// whether to recomputePendingOptions / log).
func (s *Server) reconcileAppDepBindings(installID int64) (bool, error) {
	m, err := installManifest(s, installID)
	if err != nil || m == nil {
		return false, err
	}
	if len(m.Requires.Apps) == 0 {
		return false, nil
	}
	var (
		bindingsRaw string
		projectID   string
	)
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(integration_bindings, '{}'), COALESCE(project_id, '') FROM app_installs WHERE id = ?`,
		installID,
	).Scan(&bindingsRaw, &projectID); err != nil {
		return false, err
	}
	bindings := map[string]any{}
	_ = json.Unmarshal([]byte(bindingsRaw), &bindings)

	changed := false
	for _, dep := range m.Requires.Apps {
		if dep.Optional {
			continue
		}
		if _, present := bindings[dep.Name]; present {
			continue // operator may have set null; respect it
		}
		id, ok := s.findInstalledApp(dep.Name, projectID)
		if !ok || id == 0 {
			continue // built-in or not yet installed; nothing to record
		}
		bindings[dep.Name] = id
		changed = true
	}
	if !changed {
		return false, nil
	}
	bj, _ := json.Marshal(bindings)
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET integration_bindings = ? WHERE id = ?`,
		string(bj), installID,
	); err != nil {
		return false, err
	}
	return true, nil
}

// reconcileAllAppDepBindings walks every running install and runs
// reconcileAppDepBindings against it. Called once at server boot,
// after LoadInstalledApps so the in-memory registry is populated.
func (s *Server) reconcileAllAppDepBindings() {
	rows, err := s.store.db.Query(`SELECT id FROM app_installs WHERE status = 'running'`)
	if err != nil {
		log.Printf("[APPS-DEP] reconcile-all: %v", err)
		return
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	healed := 0
	for _, id := range ids {
		if changed, err := s.reconcileAppDepBindings(id); err != nil {
			log.Printf("[APPS-DEP] reconcile install=%d: %v", id, err)
		} else if changed {
			healed++
		}
	}
	if healed > 0 {
		log.Printf("[APPS-DEP] reconcile-all: backfilled bindings on %d install(s)", healed)
	}
}
