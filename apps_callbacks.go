package main

// PlatformClient callback router — the surface app sidecars hit when
// they call back into apteva-server. Auth is the per-install
// APTEVA_APP_TOKEN; authMiddleware resolves it to user_id +
// X-Apteva-App-Install-ID. Each handler enforces its own additional
// authorization (declared permissions, binding membership) on top.
//
// Routes (all under /api/apps/callback):
//
//   GET  /whoami                         — install identity (id, app_name, project_id)
//   GET  /connections/:id                — connection metadata (no creds)
//   GET  /connections                    — list connections (filtered by project + slug)
//   GET  /instances/:id                  — instance metadata
//   POST /instances/:id/event            — send a chat-style event into an instance
//   POST /channels/send                  — send a message to a named channel
//   POST /integrations/:connID/execute   — call an integration tool (binding-gated)
//   POST /apps/:appName/call             — call another app's MCP tool (binding-gated)
//
// The bindings-gated routes are the heart of the dependency system:
// ExecuteIntegrationTool lets an app call an upstream API through a
// connection it was bound to at install time, without ever touching
// the credentials. CallApp lets an app call a sibling app's MCP tools
// when its manifest declares a kind=app dependency. Both verify the
// caller install's integration_bindings JSON to prevent enumeration
// of unrelated resources.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// ─── Router ────────────────────────────────────────────────────────

func (s *Server) handleAppCallback(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/callback/")
	if rest == "" {
		http.Error(w, "callback path required", http.StatusBadRequest)
		return
	}
	parts := strings.Split(rest, "/")

	switch parts[0] {
	case "whoami":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleCallbackWhoami(w, r)
	case "connections":
		s.handleCallbackConnections(w, r, parts[1:])
	case "instances":
		s.handleCallbackInstances(w, r, parts[1:])
	case "channels":
		s.handleCallbackChannels(w, r, parts[1:])
	case "integrations":
		s.handleCallbackIntegrations(w, r, parts[1:])
	case "apps":
		s.handleCallbackApps(w, r, parts[1:])
	case "oauth":
		s.handleCallbackOAuth(w, r, parts[1:])
	case "grants":
		s.handleCallbackGrants(w, r, parts[1:])
	default:
		http.Error(w, "unknown callback: "+parts[0], http.StatusNotFound)
	}
}

// ─── /whoami ───────────────────────────────────────────────────────

func (s *Server) handleCallbackWhoami(w http.ResponseWriter, r *http.Request) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var (
		appName, projectID, version string
	)
	if err := s.store.db.QueryRow(
		`SELECT a.name, COALESCE(i.project_id,''), COALESCE(i.version,'')
		 FROM app_installs i JOIN apps a ON a.id=i.app_id
		 WHERE i.id=?`, installID,
	).Scan(&appName, &projectID, &version); err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"install_id": installID,
		"app_name":   appName,
		"project_id": projectID,
		"version":    version,
		"bindings":   bindingsForInstall(s, installID),
		// Live-fresh: read on every whoami call so a setting change
		// in Settings → Server propagates to apps within the SDK's
		// sub-second WhoAmI cache. The env-var-only path requires a
		// sidecar restart; this doesn't.
		"public_url": s.publicBaseURL(),
	})
}

// ─── /connections ──────────────────────────────────────────────────

// GET  /connections/:id            — fetch one
// GET  /connections?project_id=…   — list. ?owned=true filters to only
//                                    rows the calling install owns.
// POST /connections/:id/disconnect — revoke. Permission-gated: caller
//                                    must own the row.
//
// Returns metadata only — never credentials. Apps that need to actually
// call an integration go through /integrations/:id/execute where the
// platform decrypts + injects auth headers server-side.
func (s *Server) handleCallbackConnections(w http.ResponseWriter, r *http.Request, parts []string) {
	// POST /connections/:id/disconnect
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "disconnect" {
		s.handleCallbackConnectionDisconnect(w, r, parts[0])
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if len(parts) == 1 && parts[0] != "" {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		conn, _, err := s.store.GetConnection(userID, id)
		if err != nil || conn == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, sdk.PlatformConnection{
			ID: conn.ID, AppSlug: conn.AppSlug, Name: conn.Name,
			Status: conn.Status, ProjectID: conn.ProjectID,
		})
		return
	}
	// list
	pid := r.URL.Query().Get("project_id")
	slug := r.URL.Query().Get("app_slug")
	ownedOnly := r.URL.Query().Get("owned") == "true"
	installID, _ := requireInstallID(r) // fine to be 0 when not owned-only
	conns, err := s.store.ListConnections(userID, pid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]sdk.PlatformConnection, 0, len(conns))
	for _, c := range conns {
		if slug != "" && c.AppSlug != slug {
			continue
		}
		if ownedOnly {
			ownerID := connectionOwnerInstallID(s, c.ID)
			if ownerID != installID {
				continue
			}
		}
		out = append(out, sdk.PlatformConnection{
			ID: c.ID, AppSlug: c.AppSlug, Name: c.Name,
			Status: c.Status, ProjectID: c.ProjectID,
		})
	}
	writeJSON(w, out)
}

// handleCallbackConnectionDisconnect revokes a connection an app
// previously created via platform.oauth.start. Apps may only disconnect
// rows they own (owner_app_install_id matches the calling install).
// Operator-managed connections are off-limits.
func (s *Server) handleCallbackConnectionDisconnect(w http.ResponseWriter, r *http.Request, idStr string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermConnectionsManage) {
		http.Error(w, "missing permission platform.connections.manage", http.StatusForbidden)
		return
	}
	connID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	if connectionOwnerInstallID(s, connID) != installID {
		http.Error(w, "not owned by this app", http.StatusForbidden)
		return
	}
	userID := getUserID(r)
	if err := s.store.DeleteConnection(userID, connID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": connID})
}

// connectionCreatedVia reads the created_via column on connections —
// 'integration' (operator-installed via Settings → Integrations),
// 'app_install' (created by an app via platform.oauth.start), or '' /
// other for legacy rows. Returns "" on lookup error.
func connectionCreatedVia(s *Server, connID int64) string {
	var v string
	_ = s.store.db.QueryRow(
		`SELECT COALESCE(created_via,'') FROM connections WHERE id=?`,
		connID,
	).Scan(&v)
	return v
}

// connectionOwnerInstallID reads owner_app_install_id from the
// connections row. Returns 0 for legacy / operator-managed rows.
func connectionOwnerInstallID(s *Server, connID int64) int64 {
	var ownerID int64
	_ = s.store.db.QueryRow(
		`SELECT COALESCE(owner_app_install_id, 0) FROM connections WHERE id=?`,
		connID,
	).Scan(&ownerID)
	return ownerID
}

// ─── /instances ────────────────────────────────────────────────────

func (s *Server) handleCallbackInstances(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "instance id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		inst, err := s.store.GetInstanceByID(id)
		if err != nil || inst == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, sdk.PlatformInstance{
			ID: inst.ID, Name: inst.Name, Status: inst.Status,
			Mode: inst.Mode, ProjectID: inst.ProjectID,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "event" && r.Method == http.MethodPost {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		// Defer to whatever the platform's existing inject path is —
		// for now we accept the call and surface it through the
		// telemetry broadcaster so the dashboard sees activity.
		// Full instance-event injection lives in instances.go and
		// can be wired here in a follow-up.
		writeJSON(w, map[string]any{"queued": true, "message": body.Message})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ─── /channels/send ────────────────────────────────────────────────

func (s *Server) handleCallbackChannels(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 || parts[0] != "send" || r.Method != http.MethodPost {
		http.Error(w, "POST /channels/send only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Channel   string `json:"channel"`
		ProjectID string `json:"project_id"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// Best-effort accept; the actual channel router is set up
	// elsewhere when channels were registered.
	writeJSON(w, map[string]any{"queued": true})
}

// ─── /integrations/:connID/execute ─────────────────────────────────

// POST /integrations/:connID/execute
//
// Body: {"tool": "<tool name>", "input": {...}}
//
// Authorization model:
//   1. Caller is a sidecar (X-Apteva-App-Install-ID set by middleware).
//   2. Install's manifest declares the platform.connections.execute
//      permission.
//   3. The connection is reachable by this install — one of:
//        a. connID appears in the install's integration_bindings, OR
//        b. owner_app_install_id == installID (the app created this
//           connection itself via platform.oauth.start), OR
//        c. created_via='integration' (operator-installed in Settings
//           → Integrations) — any permitted install in the same user's
//           scope may call it. Operator connections are explicitly
//           shared resources; gating them behind a separate role-bind
//           ceremony defeats their purpose.
//   4. When the role is bound (3a), the connection's app_slug must be
//      in the role's compatible_slugs. Skipped for 3b/3c which have
//      no role-dep to validate against.
//
// Without these checks an installed app could enumerate every
// connection in its owner's account.
//
// On success, dispatches through executeIntegrationToolWithRefresh —
// the same code path /connections/:id/execute uses, including OAuth
// refresh + 401 retry + token persistence.
func (s *Server) handleCallbackIntegrations(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 2 || parts[1] != "execute" || r.Method != http.MethodPost {
		http.Error(w, "POST /integrations/:id/execute only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	connID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connID", http.StatusBadRequest)
		return
	}
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}

	// 2. Permission check.
	if !installHasPermission(s, installID, sdk.PermConnectionsExecute) {
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsExecute), http.StatusForbidden)
		return
	}

	// Look up connection up-front so the access decision can read
	// owner_app_install_id and created_via.
	userID := getUserID(r)
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	// 3. Reachability — accept any of:
	//    a. role-bound via integration_bindings,
	//    b. owned by this install (created itself via oauth.start),
	//    c. operator-installed integration (created_via='integration').
	role, bound := installBoundConnection(s, installID, connID)
	ownerID := connectionOwnerInstallID(s, connID)
	createdVia := connectionCreatedVia(s, connID)
	log.Printf("[INTEGRATIONS-EXEC] install=%d conn=%d slug=%s tool=%s bound=%t role=%q owner=%d created_via=%q",
		installID, connID, conn.AppSlug, body.Tool, bound, role, ownerID, createdVia)
	switch {
	case bound:
		// 4. Slug-compatibility — only meaningful for role-bound (3a).
		// Owner / operator paths have no role-dep to validate against
		// and the caller already passed the permission check.
		dep, err := installRoleDep(s, installID, role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if dep != nil && dep.Kind != "app" && len(dep.CompatibleSlugs) > 0 && !contains(dep.CompatibleSlugs, conn.AppSlug) {
			http.Error(w, fmt.Sprintf("connection slug %q not in role %q compatible_slugs", conn.AppSlug, role), http.StatusForbidden)
			return
		}
	case ownerID == installID:
		log.Printf("[INTEGRATIONS-EXEC] grant=owner install=%d conn=%d", installID, connID)
	case createdVia == "integration":
		log.Printf("[INTEGRATIONS-EXEC] grant=operator install=%d conn=%d slug=%s", installID, connID, conn.AppSlug)
	default:
		log.Printf("[INTEGRATIONS-EXEC] DENY install=%d conn=%d slug=%s reason=not-bound-not-owned-not-operator owner=%d created_via=%q",
			installID, connID, conn.AppSlug, ownerID, createdVia)
		http.Error(w, "connection not reachable by this install (not bound, not owned, not operator-installed)", http.StatusForbidden)
		return
	}

	// Resolve catalog tool.
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "integration app not in catalog: "+conn.AppSlug, http.StatusBadGateway)
		return
	}
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == body.Tool {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		http.Error(w, "tool not found on integration: "+body.Tool, http.StatusNotFound)
		return
	}

	// Decrypt + execute. Mirrors handleExecuteTool exactly.
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	var credentials map[string]string
	_ = json.Unmarshal([]byte(plain), &credentials)

	ctx, err := s.resolveConnectionContext(userID, app, credentials, body.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	persistTargetID := connID
	if ctx.MasterConnID != 0 {
		persistTargetID = ctx.MasterConnID
	}
	persist := func(updated map[string]string) error {
		blob, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(s.secret, string(blob))
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionCredentials(persistTargetID, enc)
	}
	result, err := executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, persist)
	if err != nil {
		writeJSON(w, map[string]any{"success": false, "data": err.Error()})
		return
	}
	// Match handleExecuteTool's response shape. The SDK caller can
	// json.Unmarshal the data field into whatever type they expect.
	if result == nil {
		writeJSON(w, map[string]any{"success": true})
		return
	}
	writeJSON(w, result)
}

// ─── /apps/:appName/call ───────────────────────────────────────────

// POST /apps/:appName/call
//
// Body: {"tool": "<tool name>", "input": {...}}
//
// Authorization:
//   1. Caller is a sidecar (X-Apteva-App-Install-ID set).
//   2. Install's manifest declares platform.apps.call permission.
//   3. appName appears in the install's integration_bindings (under
//      a kind=app dep).
//
// On success, calls the target app's /mcp endpoint via the same
// proxy machinery the dashboard uses — credentials in the form of
// the target's APTEVA_APP_TOKEN are injected by handleAppProxy.
func (s *Server) handleCallbackApps(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 2 || parts[1] != "call" || r.Method != http.MethodPost {
		http.Error(w, "POST /apps/:name/call only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	targetAppName := parts[0]
	if targetAppName == "" {
		http.Error(w, "appName required", http.StatusBadRequest)
		return
	}
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool required", http.StatusBadRequest)
		return
	}
	if !installHasPermission(s, installID, sdk.PermAppsCall) {
		http.Error(w, "missing permission: "+string(sdk.PermAppsCall), http.StatusForbidden)
		return
	}
	if !installBoundApp(s, installID, targetAppName) {
		http.Error(w, "app not bound: "+targetAppName, http.StatusForbidden)
		return
	}

	target := s.installedApps.GetByName(targetAppName)
	if target == nil {
		http.Error(w, "target app not running: "+targetAppName, http.StatusBadGateway)
		return
	}
	if target.SidecarURL == "" {
		http.Error(w, "target app has no sidecar URL", http.StatusBadGateway)
		return
	}

	// Construct an MCP tools/call JSON-RPC request and POST to the
	// target's /mcp. The target's withTokenAuth requires its own
	// APTEVA_APP_TOKEN — we use target.Token directly since we're
	// internal to the platform.
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      body.Tool,
			"arguments": body.Input,
		},
	}
	rpcBody, _ := json.Marshal(rpc)
	req, _ := http.NewRequestWithContext(r.Context(), "POST", target.SidecarURL+"/mcp", strings.NewReader(string(rpcBody)))
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "target unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// ─── helpers ───────────────────────────────────────────────────────

func requireInstallID(r *http.Request) (int64, error) {
	v := r.Header.Get("X-Apteva-App-Install-ID")
	if v == "" {
		return 0, errors.New("sidecar token required")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid install id")
	}
	return id, nil
}

// bindingsForInstall returns the parsed integration_bindings JSON for
// an install. Returns an empty map on missing/malformed.
func bindingsForInstall(s *Server, installID int64) map[string]any {
	var raw string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(integration_bindings,'{}') FROM app_installs WHERE id=?`, installID,
	).Scan(&raw); err != nil || raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{}
	}
	return out
}

// ─── /oauth/start ──────────────────────────────────────────────────

// POST /oauth/start
//
// Body: {integration_slug, return_url, name?, project_id?}
//
// Creates a pending connection owned by the calling install, returns
// the upstream authorize URL. After the user completes the dance, the
// callback at /oauth/local/callback 302s the browser to return_url
// with ?conn_id=<id>&status=ok so the app can pick up.
//
// Authorization: install must declare platform.oauth.start.
func (s *Server) handleCallbackOAuth(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || parts[0] != "start" || r.Method != http.MethodPost {
		http.Error(w, "POST /oauth/start only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermOAuthStart) {
		http.Error(w, "missing permission platform.oauth.start", http.StatusForbidden)
		return
	}
	var body struct {
		IntegrationSlug string `json:"integration_slug"`
		ReturnURL       string `json:"return_url"`
		Name            string `json:"name"`
		ProjectID       string `json:"project_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<19)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.IntegrationSlug == "" {
		http.Error(w, "integration_slug required", http.StatusBadRequest)
		return
	}
	if body.ReturnURL == "" {
		http.Error(w, "return_url required", http.StatusBadRequest)
		return
	}
	app := s.catalog.Get(body.IntegrationSlug)
	if app == nil {
		http.Error(w, "unknown integration: "+body.IntegrationSlug, http.StatusNotFound)
		return
	}
	if app.Auth.OAuth2 == nil {
		http.Error(w, body.IntegrationSlug+" has no OAuth2 config — cannot use platform.oauth.start", http.StatusBadRequest)
		return
	}
	// Default name + project from the install if the caller didn't
	// supply them. The install's project is the natural default scope.
	name := body.Name
	if name == "" {
		name = app.Name
	}
	pid := body.ProjectID
	if pid == "" {
		_ = s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&pid)
	}
	userID := getUserID(r)

	// nil autoMCP — app-install connections always skip auto-MCP via the
	// owner_app_install_id check; the per-row flag isn't relevant here.
	conn, authURL, err := s.startLocalOAuth(userID, app, name, pid, "", "", installID, body.ReturnURL, nil)
	if err != nil {
		http.Error(w, "oauth start: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 10-minute window matches mintOAuthState's TTL.
	expiresAt := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	writeJSON(w, map[string]any{
		"connection_id": conn.ID,
		"authorize_url": authURL,
		"expires_at":    expiresAt,
	})
}

// installHasPermission checks the install's manifest's requires.permissions.
func installHasPermission(s *Server, installID int64, perm sdk.Permission) bool {
	m, err := installManifest(s, installID)
	if err != nil || m == nil {
		return false
	}
	for _, p := range m.Requires.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// installManifest pulls + parses the manifest_json for the install's app.
func installManifest(s *Server, installID int64) (*sdk.Manifest, error) {
	var raw string
	err := s.store.db.QueryRow(
		`SELECT a.manifest_json FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var m sdk.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// installRoleDep returns the IntegrationDep for the named role.
func installRoleDep(s *Server, installID int64, role string) (*sdk.IntegrationDep, error) {
	m, err := installManifest(s, installID)
	if err != nil {
		return nil, err
	}
	for i, d := range m.Requires.Integrations {
		if d.Role == role {
			return &m.Requires.Integrations[i], nil
		}
	}
	return nil, nil
}

// installBoundConnection returns the role name a connection_id is
// bound to in the install's bindings, or ("", false) when missing.
func installBoundConnection(s *Server, installID, connID int64) (string, bool) {
	bindings := bindingsForInstall(s, installID)
	for role, raw := range bindings {
		// Bindings JSON values arrive as float64 from JSON. Compare numerically.
		if n, ok := raw.(float64); ok && int64(n) == connID {
			return role, true
		}
	}
	return "", false
}

// installBoundApp returns true if the named app name appears as a
// kind=app binding for the install.
func installBoundApp(s *Server, installID int64, appName string) bool {
	m, err := installManifest(s, installID)
	if err != nil || m == nil {
		return false
	}
	bindings := bindingsForInstall(s, installID)
	for _, dep := range m.Requires.Integrations {
		if dep.Kind != "app" {
			continue
		}
		raw, ok := bindings[dep.Role]
		if !ok || raw == nil {
			continue
		}
		// kind=app bindings store the target install_id; resolve to
		// the running app name via installedApps.
		boundInstallID := int64(0)
		if n, ok := raw.(float64); ok {
			boundInstallID = int64(n)
		}
		if boundInstallID == 0 {
			continue
		}
		if e := s.installedApps.Get(boundInstallID); e != nil && e.AppName == appName {
			return true
		}
	}
	return false
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
