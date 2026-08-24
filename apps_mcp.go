package main

// Bridge between the Apps system and the platform's MCP registry.
//
// Apps that declare provides.mcp_tools in their manifest expose a
// JSON-RPC handler at the sidecar's /mcp endpoint (mounted by the
// SDK's framework routes). The platform's MCP registry — what
// agents query via list_mcp_servers + connect — is a separate
// system rooted in the mcp_servers table.
//
// Without a bridge, an installed app's tools are reachable HTTP-wise
// but invisible to agents. registerAppMCP closes that gap: every
// running app_install with mcp_tools gets one mcp_servers row of
// source='app', transport='http', URL pointing at this server's
// own /api/apps/<name>/mcp proxy with the install's APTEVA_APP_TOKEN
// embedded as ?api_key= so the proxy auth middleware accepts the
// agent's request and injects the right Bearer header before
// forwarding to the sidecar.
//
// Lifecycle:
//   - installFromSource / installLocally / seedBuiltinInstalls →
//     registerAppMCP
//   - handleUpgradeApp → registerAppMCP (re-register; URL stable but
//     allowed_tools may have grown across versions)
//   - handleUninstallApp → unregisterAppMCP
//   - server boot → backfillAppMCPs (one-time fixup for installs
//     created before this bridge existed, plus a safety net for
//     anything that drifted)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// appMCPUpstreamID returns the stable identifier the bridge writes
// into mcp_servers.upstream_id so we can find + delete the row on
// uninstall without depending on (user_id, project_id, name) staying
// the same. The install_id is the only field that's truly stable
// across renames / project moves.
func appMCPUpstreamID(installID int64) string {
	return fmt.Sprintf("app:%d", installID)
}

// localServerPort returns the port apteva-server is listening on,
// for building the loopback URL stored in mcp_servers.url. Falls
// back to 5280 (the dev default) so tests + early-init paths don't
// blow up before the PORT env is set.
func localServerPort() string {
	if v := os.Getenv("PORT"); v != "" {
		return v
	}
	return "5280"
}

// registerAppMCP creates (or refreshes) the mcp_servers row that
// makes an installed app's tools visible to agents.
//
// Idempotent: keyed on upstream_id, so calling it twice produces a
// single row. Fields refreshed on every call so a manifest update
// (new tool added in v0.2 of an app) lands in allowed_tools without
// a manual reset.
//
// Apps with no mcp_tools in their manifest get no row — there's
// nothing to expose. The caller can still call this safely; we
// just no-op.
func (s *Server) registerAppMCP(installID int64) error {
	// Pull everything we need in one query: app row's name, the
	// install's project, the user who owns it, and the cached
	// manifest_json (which we re-read on every call so manifest
	// changes flow through).
	var (
		appName, projectID, manifestJSON string
		installedBy                      int64
	)
	err := s.store.db.QueryRow(
		`SELECT a.name, COALESCE(i.project_id, ''), i.installed_by,
		        COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&appName, &projectID, &installedBy, &manifestJSON)
	if err != nil {
		return fmt.Errorf("install %d not found: %w", installID, err)
	}

	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		return fmt.Errorf("parse manifest for install %d: %w", installID, err)
	}

	tools := agentVisibleMCPTools(manifest.Provides.MCPTools)
	if len(tools) == 0 {
		// App has no MCP surface — nothing to expose. Make sure no
		// stale row lingers from a previous version that did.
		_ = s.unregisterAppMCP(installID)
		return nil
	}

	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}
	allowedJSON, _ := json.Marshal(toolNames)

	// Loopback URL through our own proxy, not the sidecar's direct
	// URL: the proxy port is stable across sidecar restarts and the
	// auth middleware injects APTEVA_APP_TOKEN before forwarding.
	// The random per-install capability is embedded only in this loopback URL.
	// authMiddleware accepts app-token query auth exclusively on loopback app
	// data-plane routes, never on management endpoints.
	appToken, err := s.appInstallToken(installID)
	if err != nil {
		return fmt.Errorf("create app credential: %w", err)
	}
	mcpURL := fmt.Sprintf("http://127.0.0.1:%s/api/apps/%s/mcp?api_key=%s&install_id=%d",
		localServerPort(), appName, appToken, installID)

	// user_id must be a real user. installed_by is 0 for built-ins
	// + global installs the platform seeded; fall back to user 1
	// (admin) so the FK to users(id) holds.
	ownerID := installedBy
	if ownerID == 0 {
		ownerID = 1
	}

	upstream := appMCPUpstreamID(installID)
	// Use DisplayName (short, e.g. "Storage") over Description (long
	// prose, multiple sentences). The dashboard's MCP list renders
	// the description as the row's primary label, so a long one would
	// take over the whole UI line.
	desc := strings.TrimSpace(manifest.DisplayName)
	if desc == "" {
		desc = appName
	}

	// Existing row? Update in place. Otherwise insert. We don't use
	// INSERT OR REPLACE because that would re-issue the primary key
	// and break any existing references (UI selections, etc.).
	var existingID int64
	err = s.store.db.QueryRow(
		`SELECT id FROM mcp_servers WHERE upstream_id = ?`, upstream,
	).Scan(&existingID)
	if err == nil {
		_, err = s.store.db.Exec(
			`UPDATE mcp_servers SET
				name = ?,
				description = ?,
				url = ?,
				allowed_tools = ?,
				tool_count = ?,
				status = 'running',
				project_id = ?,
				user_id = ?
			 WHERE id = ?`,
			appName, desc, mcpURL, string(allowedJSON), len(toolNames),
			projectID, ownerID, existingID,
		)
		if err != nil {
			return fmt.Errorf("update mcp_servers: %w", err)
		}
		log.Printf("[APPS-MCP] refreshed %s install=%d server_id=%d tools=%d",
			appName, installID, existingID, len(toolNames))
	} else {
		res, err := s.store.db.Exec(
			`INSERT INTO mcp_servers
				(user_id, name, description, status, tool_count,
				 source, transport, url, project_id, allowed_tools, upstream_id)
			 VALUES (?, ?, ?, 'running', ?, 'app', 'http', ?, ?, ?, ?)`,
			ownerID, appName, desc, len(toolNames),
			mcpURL, projectID, string(allowedJSON), upstream,
		)
		if err != nil {
			return fmt.Errorf("insert mcp_servers: %w", err)
		}
		newID, _ := res.LastInsertId()
		log.Printf("[APPS-MCP] registered %s install=%d server_id=%d tools=%d project=%q",
			appName, installID, newID, len(toolNames), projectID)
	}
	return nil
}

// agentVisibleMCPTools is the single platform-side filter for surfaces that
// become part of an agent's MCP configuration. app_only tools are still
// declared in the install manifest so authenticated apps can call them, but
// they never create or widen an agent-facing MCP row.
func agentVisibleMCPTools(tools []sdk.MCPToolSpec) []sdk.MCPToolSpec {
	out := make([]sdk.MCPToolSpec, 0, len(tools))
	for _, tool := range tools {
		if tool.Exposure == sdk.ToolExposureAppOnly {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// appMCPSurfaceSnapshot is the live agent-visible tools/list contract exposed
// by a running sidecar. available distinguishes a real empty surface from a
// sidecar that could not be inspected.
type appMCPSurfaceSnapshot struct {
	Tools     []installMCPToolInfo
	Available bool
}

func (s *Server) snapshotAppMCPSurface(installID int64) appMCPSurfaceSnapshot {
	if s.installedApps == nil {
		return appMCPSurfaceSnapshot{}
	}
	install := s.installedApps.Get(installID)
	if install == nil || strings.TrimSpace(install.SidecarURL) == "" {
		return appMCPSurfaceSnapshot{}
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		log.Printf("[APPS-MCP] snapshot credential install=%d: %v", installID, err)
		return appMCPSurfaceSnapshot{}
	}
	tools, err := listAppMCPTools(strings.TrimRight(install.SidecarURL, "/")+"/mcp", token)
	if err != nil {
		log.Printf("[APPS-MCP] snapshot tools/list install=%d: %v", installID, err)
		return appMCPSurfaceSnapshot{}
	}
	// The sidecar may expose tools that the current install manifest has not
	// made agent-visible yet. Apply the same manifest exposure/name filter as
	// registerAppMCP so the snapshot represents the surface Core can actually
	// receive, including built-in upgrades where the new binary is already live.
	var manifestJSON string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(NULLIF(i.manifest_json,''),a.manifest_json)
		 FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID,
	).Scan(&manifestJSON); err != nil {
		log.Printf("[APPS-MCP] snapshot manifest install=%d: %v", installID, err)
		return appMCPSurfaceSnapshot{}
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		log.Printf("[APPS-MCP] snapshot parse manifest install=%d: %v", installID, err)
		return appMCPSurfaceSnapshot{}
	}
	allowed := make(map[string]struct{})
	for _, tool := range agentVisibleMCPTools(manifest.Provides.MCPTools) {
		allowed[tool.Name] = struct{}{}
	}
	filtered := make([]installMCPToolInfo, 0, len(tools))
	for _, tool := range tools {
		if _, ok := allowed[tool.Name]; ok {
			filtered = append(filtered, tool)
		}
	}
	return appMCPSurfaceSnapshot{Tools: canonicalAppMCPSurface(filtered), Available: true}
}

func canonicalAppMCPSurface(tools []installMCPToolInfo) []installMCPToolInfo {
	out := append([]installMCPToolInfo(nil), tools...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Description < out[j].Description
	})
	return out
}

func appMCPSurfaceChanged(before, after appMCPSurfaceSnapshot) bool {
	if !before.Available || !after.Available {
		return false
	}
	return !reflect.DeepEqual(canonicalAppMCPSurface(before.Tools), canonicalAppMCPSurface(after.Tools))
}

// registerAppMCPAfterActivation refreshes the bridge only after the new
// sidecar has passed health checks and become the active registry entry. A
// changed live tool contract invalidates Core's tools/list cache, so restart
// only enabled, bound agents that are currently running.
func (s *Server) registerAppMCPAfterActivation(installID int64, before appMCPSurfaceSnapshot) error {
	if err := s.registerAppMCP(installID); err != nil {
		return err
	}
	after := s.snapshotAppMCPSurface(installID)
	if !appMCPSurfaceChanged(before, after) {
		return nil
	}
	return s.restartBoundRunningAgentsForAppMCPChange(installID, s.updateAgentCore)
}

func (s *Server) restartBoundRunningAgentsForAppMCPChange(
	installID int64,
	restart func(context.Context, int64) error,
) error {
	rows, err := s.store.db.Query(
		`SELECT agent_id FROM app_agent_bindings WHERE install_id=? AND enabled=1 ORDER BY agent_id`,
		installID,
	)
	if err != nil {
		return fmt.Errorf("list bound agents for install %d: %w", installID, err)
	}
	defer rows.Close()
	var agentIDs []int64
	for rows.Next() {
		var agentID int64
		if err := rows.Scan(&agentID); err != nil {
			return err
		}
		agentIDs = append(agentIDs, agentID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var restartErrs []error
	for _, agentID := range agentIDs {
		if !s.agents.IsRunning(agentID) {
			continue
		}
		if err := restart(context.Background(), agentID); err != nil {
			restartErrs = append(restartErrs, fmt.Errorf("restart agent %d after app MCP change: %w", agentID, err))
			continue
		}
		log.Printf("[APPS-MCP] restarted bound agent=%d after install=%d tool surface changed", agentID, installID)
	}
	return errors.Join(restartErrs...)
}

// unregisterAppMCP removes the bridge row for an install. Used on
// uninstall + as part of registerAppMCP's no-tools cleanup. Safe to
// call even when no row exists — the DELETE is a no-op.
func (s *Server) unregisterAppMCP(installID int64) error {
	res, err := s.store.db.Exec(
		`DELETE FROM mcp_servers WHERE upstream_id = ?`,
		appMCPUpstreamID(installID),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[APPS-MCP] unregistered install=%d (deleted %d row)", installID, n)
	}
	return nil
}

// backfillAppMCPs walks every running app_install and ensures it
// has a corresponding mcp_servers row. Safety net for installs
// created before this bridge existed; also catches drift after a
// botched manual DB edit.
//
// One-shot at boot. Failures for one install don't stop others.
func (s *Server) backfillAppMCPs() {
	rows, err := s.store.db.Query(
		`SELECT i.id FROM app_installs i WHERE i.status = 'running'`,
	)
	if err != nil {
		log.Printf("[APPS-MCP] backfill query failed: %v", err)
		return
	}
	defer rows.Close()
	type candidate struct {
		id int64
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id); err == nil {
			candidates = append(candidates, item)
		}
	}
	registered := 0
	for _, item := range candidates {
		if err := s.registerAppMCP(item.id); err != nil {
			log.Printf("[APPS-MCP] backfill install=%d failed: %v", item.id, err)
			continue
		}
		registered++
	}
	if registered > 0 {
		log.Printf("[APPS-MCP] backfill complete: refreshed=%d", registered)
	}
}

func appMCPURLMatchesTokenHash(rawURL, tokenHash string) bool {
	if rawURL == "" || tokenHash == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	token := strings.TrimSpace(parsed.Query().Get("api_key"))
	return token != "" && HashAPIKey(token) == tokenHash
}

// ListAppMCPsForProject returns app tool surfaces as project resources.
// Their user_id records who installed the app; it does not make the surface
// private to that operator. Every project member who can manage an agent must
// see the same project/global app inventory.
func (s *Store) ListAppMCPsForProject(projectID string) ([]MCPServerRecord, error) {
	rows, err := s.db.Query(
		`SELECT id FROM mcp_servers
		 WHERE source='app' AND (COALESCE(project_id,'')='' OR project_id=?)
		 ORDER BY id`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServerRecord
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		record, err := s.GetMCPServerByIDUnscoped(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}
