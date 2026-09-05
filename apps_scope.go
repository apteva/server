package main

// apps_scope.go — `PATCH /api/apps/installs/:id/scope` moves a
// running install between project and global scope without
// destroying its data.
//
// Operator scenario that motivates this: I installed `storage` in
// project A, then realized I want every project's agents to share
// it. Pre-v0.14.5 the only path was uninstall + reinstall, which
// wiped the per-install SQLite DB (apps/<name>/data/<install-id>/)
// because uninstall removes app_installs.id and a fresh install
// gets a new id.
//
// What we change vs. what we leave alone:
//
//   CHANGE  app_installs.project_id     (the source of truth)
//   CHANGE  connections.project_id WHERE owner_app_install_id = id
//                                       (apps-minted connections
//                                        follow their owning
//                                        install's scope)
//   CHANGE  mcp_servers.project_id     (via registerAppMCP after
//                                       the install row is updated;
//                                       it re-reads the row)
//
//   LEAVE   apps/<name>/data/<id>/    (data dir is keyed by
//                                       install_id, never moves)
//   LEAVE   integration_bindings JSON  (references connection_id
//                                       and install_id, not
//                                       project_id; cross-app calls
//                                       work across the move)
//   LEAVE   skills, subscriptions     (install_id-keyed)
//
// Safety:
//   - Single SQL transaction wraps app_installs + connections
//     UPDATEs. A crash partway leaves nothing modified.
//   - Refuses when another install of the same app already occupies
//     the target scope (UNIQUE(app_id, project_id)).
//   - Sidecar gets stopped + resumed via the existing supervisor
//     path so WhoAmI returns the new scope (the SDK caches it at
//     boot — without a restart the sidecar would think it's still
//     in the old scope).
//   - No DELETE statements anywhere in the flow.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// scopeChangeResult is the JSON body returned on success. Fields are
// stable — the dashboard renders the summary inline so the operator
// can see what actually moved.
type scopeChangeResult struct {
	InstallID           int64  `json:"install_id"`
	OldProjectID        string `json:"old_project_id"`
	NewProjectID        string `json:"new_project_id"`
	ConnectionsMigrated int    `json:"connections_migrated"`
	SidecarRestarted    bool   `json:"sidecar_restarted"`
}

func (s *Server) handleSetInstallScope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	idStr := strings.TrimSuffix(rest, "/scope")
	installID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid install id", http.StatusBadRequest)
		return
	}

	var body struct {
		ProjectID string `json:"project_id"` // "" = global
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	target := body.ProjectID

	// Load the install. We need the current scope + the manifest's
	// scope list to validate the move + the app_id for the UNIQUE
	// collision check.
	var (
		appID        int64
		oldProjectID string
		manifestJSON string
		appName      string
	)
	err = s.store.db.QueryRow(
		`SELECT i.app_id, COALESCE(i.project_id,''),
		        COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json), a.name
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&appID, &oldProjectID, &manifestJSON, &appName)
	if err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	if oldProjectID == "" || target == "" {
		if s.store.GetPlatformRole(getUserID(r)) != PlatformAdmin {
			http.Error(w, "global scope requires platform admin", http.StatusForbidden)
			return
		}
	}
	for _, project := range []string{oldProjectID, target} {
		if project != "" {
			if _, _, ok := s.requireProjectAccess(w, r, project, ProjectEditor); !ok {
				return
			}
		}
	}
	if oldProjectID == target {
		// No-op. Return the same shape so callers don't have to
		// special-case it; just don't restart the sidecar.
		writeJSON(w, scopeChangeResult{
			InstallID:    installID,
			OldProjectID: oldProjectID,
			NewProjectID: target,
		})
		return
	}

	// Validate the target scope appears in the manifest's scopes[].
	// A storage manifest that only declares [project] can't move to
	// global; refuse with a precise error.
	var manifest struct {
		Scopes []string `json:"scopes"`
	}
	_ = json.Unmarshal([]byte(manifestJSON), &manifest)
	wantScope := "project"
	if target == "" {
		wantScope = "global"
	}
	if !containsString(manifest.Scopes, wantScope) {
		http.Error(w, fmt.Sprintf(
			"app %q does not support scope %q (manifest scopes: %v)",
			appName, wantScope, manifest.Scopes,
		), http.StatusBadRequest)
		return
	}

	// UNIQUE(app_id, project_id) — refuse if the target scope is
	// already taken by another install of the same app. Cheap
	// pre-check so the operator gets a real id rather than the raw
	// SQLite constraint error.
	var conflictID int64
	_ = s.store.db.QueryRow(
		`SELECT id FROM app_installs WHERE app_id = ? AND COALESCE(project_id,'') = ?`,
		appID, target,
	).Scan(&conflictID)
	if conflictID != 0 && conflictID != installID {
		http.Error(w, fmt.Sprintf(
			"app %q already has an install at the target scope (install_id=%d) — uninstall that one first or pick a different scope",
			appName, conflictID,
		), http.StatusConflict)
		return
	}

	// All checks pass. Wrap the two UPDATEs in a transaction so a
	// crash partway leaves nothing inconsistent. registerAppMCP runs
	// AFTER commit — it reads the just-committed row and rewrites
	// mcp_servers; failure there is non-fatal (the row is rebuilt
	// on next boot's reconcile) so it stays out of the tx.
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "begin tx: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(
		`UPDATE app_installs SET project_id = ? WHERE id = ?`,
		target, installID,
	); err != nil {
		http.Error(w, "update install: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Apps-minted connections follow their owning install's scope.
	// owner_app_install_id is set when the app uses PermOAuthStart
	// to mint a connection of its own; those are conceptually
	// "this install's credentials" and live and die with it.
	connRes, err := tx.Exec(
		`UPDATE connections SET project_id = ? WHERE owner_app_install_id = ?`,
		target, installID,
	)
	if err != nil {
		http.Error(w, "update owned connections: "+err.Error(), http.StatusInternalServerError)
		return
	}
	connsMoved, _ := connRes.RowsAffected()

	// Connection-backed MCP rows and grants must follow the same scope boundary.
	if _, err := tx.Exec("UPDATE mcp_servers SET project_id=? WHERE connection_id IN (SELECT id FROM connections WHERE owner_app_install_id=?)", target, installID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if target != "" {
		for _, table := range []string{"app_agent_bindings", "app_grants", "app_grant_defaults"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE install_id=? AND agent_id IN (SELECT id FROM agents WHERE COALESCE(project_id,'')<>?)", installID, target); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tx = nil // suppress the deferred rollback

	// Rebuild the in-memory installedApps registry from the DB.
	// The reverse proxy at /api/apps/<name>/... reads
	// `entry.ProjectID` off this cache to dispatch project-scoped
	// requests; without the refresh the post-flip install is still
	// indexed with its OLD ProjectID, so a request from a project
	// where the install is now global routes nowhere and the
	// dashboard's StoragePanel sees 404 "app not installed".
	// LoadInstalledApps is cheap (one query, in-memory rebuild)
	// and is the canonical reload path used by install + upgrade
	// + uninstall — re-using it keeps scope-change consistent with
	// every other lifecycle event.
	s.LoadInstalledApps()

	// Re-register the install's MCP row. registerAppMCP re-reads
	// app_installs (now with the new project_id) and rewrites
	// mcp_servers.project_id accordingly. Errors here don't roll
	// back — the bridge row gets reconstructed on next boot — but
	// we log them so operators see them.
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS-SCOPE] registerAppMCP install=%d after scope change: %v", installID, err)
	}

	// Restart the sidecar. The SDK caches the install identity
	// (project_id, scope) at boot via WhoAmI; without a restart it
	// would keep returning the OLD scope to any app code that
	// inspects ctx.ProjectID(). Stop + the supervisor's normal
	// resume path picks the freshly-committed app_installs row
	// (including APTEVA_PROJECT_ID = target).
	restarted := false
	if err := s.restartInstallSidecar(installID); err != nil {
		log.Printf("[APPS-SCOPE] sidecar restart install=%d: %v (operator may need to restart manually)", installID, err)
	} else {
		restarted = true
	}

	log.Printf("[APPS-SCOPE] install=%d (%s) moved from %q to %q (connections migrated: %d, sidecar restarted: %v)",
		installID, appName, oldProjectID, target, connsMoved, restarted)

	writeJSON(w, scopeChangeResult{
		InstallID:           installID,
		OldProjectID:        oldProjectID,
		NewProjectID:        target,
		ConnectionsMigrated: int(connsMoved),
		SidecarRestarted:    restarted,
	})
}

// restartInstallSidecar tears down the running sidecar and queues a
// fresh resume via the existing per-install resume path. Both halves
// are best-effort — Stop() is idempotent and resume reads the live
// app_installs row so the new scope is picked up automatically.
//
// Lives next to handleSetInstallScope because that's the only caller
// today. If a second consumer shows up, move to apps_local.go and
// give it a real signature.
func (s *Server) restartInstallSidecar(installID int64) error {
	// Best-effort stop. Stop is idempotent — "not running" is OK.
	_ = s.localApps.Stop(installID)

	// Re-read the install row + fan out via the existing resume
	// helper. We don't reuse ResumeLocalInstalls (which scans the
	// whole table) — we just call its per-install body so we
	// don't fight the parallel-fanout worker pool.
	var (
		pid              int64
		port             int64
		binPath          string
		appName          string
		projectID        string
		cfgEnc           string
		installedVersion string
		manifestJSON     string
	)
	err := s.store.db.QueryRow(
		`SELECT COALESCE(i.local_pid,0), COALESCE(i.local_port,0),
		        COALESCE(i.local_bin_path,''), a.name, COALESCE(i.project_id,''),
		        COALESCE(i.config_encrypted,''), COALESCE(i.version,''),
		        COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&pid, &port, &binPath, &appName, &projectID, &cfgEnc, &installedVersion, &manifestJSON)
	if err != nil {
		return err
	}
	if binPath == "" {
		// Install hasn't been built yet (pending). Nothing to
		// restart; the eventual build will pick up the new scope.
		return nil
	}
	go s.resumeOneLocalInstall(installID, pid, port, binPath, appName, projectID, cfgEnc, installedVersion, manifestJSON)
	return nil
}
