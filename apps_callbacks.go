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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
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
	case "instances", "agents":
		// Phase 2 alias — sidecars can call /api/apps/callback/agents/:id
		// or the original /instances/:id; the handler treats them as
		// identical. Apps are migrating from PlatformInstance to
		// PlatformAgent and the SDK's GetAgent helper points here.
		s.handleCallbackInstances(w, r, parts[1:])
	case "channels":
		s.handleCallbackChannels(w, r, parts[1:])
	case "integrations":
		s.handleCallbackIntegrations(w, r, parts[1:])
	case "integration-webhooks":
		s.handleCallbackIntegrationWebhooks(w, r, parts[1:])
	case "apps":
		s.handleCallbackApps(w, r, parts[1:])
	case "oauth":
		s.handleCallbackOAuth(w, r, parts[1:])
	case "grants":
		s.handleCallbackGrants(w, r, parts[1:])
	case "ingress":
		s.handleCallbackIngress(w, r, parts[1:])
	case "dns":
		s.handleCallbackDNS(w, r, parts[1:])
	case "projects":
		s.handleCallbackProjects(w, r, parts[1:])
	case "threads":
		s.handleCallbackThreads(w, r, parts[1:])
	case "runtimes":
		s.handleCallbackRuntimes(w, r, parts[1:])
	case "platform-info":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleCallbackPlatformInfo(w, r)
	case "delegated-keys":
		s.handleCallbackDelegatedKeys(w, r, parts[1:])
	default:
		http.Error(w, "unknown callback: "+parts[0], http.StatusNotFound)
	}
}

// ─── /platform-info ────────────────────────────────────────────────
//
// GET /api/apps/callback/platform-info — returns the small bag of
// platform-level facts the SDK's PlatformInfo() helper consumes.
// Used by sidecars to get a hot-refreshed public_url + apteva version
// instead of relying on the spawn-time APTEVA_PUBLIC_URL env (which
// goes stale when operators change settings without restarting every
// sidecar). The SDK caches the response for 60s so the call frequency
// is bounded.
//
// Permission-free: public_url is the URL operators publish for
// external services anyway (webhooks, signed URLs). Version is the
// release string already returned by /api/platform-status.
func (s *Server) handleCallbackPlatformInfo(w http.ResponseWriter, r *http.Request) {
	publicURL := s.publicBaseURL()
	if installID, err := requireInstallID(r); err == nil {
		if runtimeURL := s.runtimePlatformURLForInstall(installID); runtimeURL != "" {
			publicURL = runtimeURL
		}
	}
	writeJSON(w, map[string]any{
		"public_url": publicURL,
		"version":    Version,
	})
}

// ─── /projects ─────────────────────────────────────────────────────
//
// GET /api/apps/callback/projects — returns the projects this install
// can dispatch against. Project-scoped installs see a singleton list
// holding only their pinned project; global installs see every
// project the owning user has access to. The SDK's worker dispatcher
// uses this to fan workers out per project.
//
// No declared permission required — every install is allowed to
// enumerate its own projection. The listing is scoped server-side
// to the install's project (project-scoped) or the install's owner
// (global).

func (s *Server) handleCallbackProjects(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) > 0 && parts[0] != "" {
		http.Error(w, "unexpected sub-path", http.StatusNotFound)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Look up the install's project + owner. Project-scoped installs
	// return only their own project — apps can't enumerate sibling
	// projects via the SDK; that's a global-only privilege.
	//
	// The owner column on app_installs is `installed_by`, NOT
	// `user_id` — a stale schema mismatch from an earlier rename. An
	// errant `user_id` here makes every call return 404 "install not
	// found" since the column doesn't exist, which silently neuters
	// every global-install worker's per-project fan-out.
	var (
		installProject string
		userID         int64
	)
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(project_id,''), COALESCE(installed_by, 0) FROM app_installs WHERE id=?`, installID,
	).Scan(&installProject, &userID); err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	if installProject != "" {
		// Singleton — same project as the install.
		var name, description string
		_ = s.store.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(description,'') FROM projects WHERE id=?`, installProject,
		).Scan(&name, &description)
		writeJSON(w, []map[string]any{{"id": installProject, "name": name, "description": description}})
		return
	}
	// Global install — return every project belonging to the owning
	// user. The SDK will fan workers out across this list.
	projects, err := s.store.ListProjects(userID)
	if err != nil {
		http.Error(w, "list projects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, map[string]any{"id": p.ID, "name": p.Name, "description": p.Description})
	}
	writeJSON(w, out)
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
	// Fetch project metadata so apps can use the operator-set name +
	// description as context (e.g. media's describer prepends them
	// to the LLM system prompt). Cheap — single indexed row read,
	// silent fall-through if the project was deleted out from under
	// the install.
	var projectName, projectDescription string
	if projectID != "" {
		_ = s.store.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(description,'') FROM projects WHERE id=?`, projectID,
		).Scan(&projectName, &projectDescription)
	}
	publicURL := s.publicBaseURL()
	if runtimeURL := s.runtimePlatformURLForInstall(installID); runtimeURL != "" {
		publicURL = runtimeURL
	}
	writeJSON(w, map[string]any{
		"install_id":          installID,
		"app_name":            appName,
		"project_id":          projectID,
		"project_name":        projectName,
		"project_description": projectDescription,
		"version":             version,
		"bindings":            bindingsForInstall(s, installID),
		// Live-fresh: read on every whoami call so a setting change
		// in Settings → Server propagates to apps within the SDK's
		// sub-second WhoAmI cache. The env-var-only path requires a
		// sidecar restart; this doesn't.
		"public_url": publicURL,
	})
}

// ─── /connections ──────────────────────────────────────────────────

// GET  /connections/:id            — fetch one
// GET  /connections?project_id=…   — list. ?owned=true filters to only
//
//	rows the calling install owns.
//
// POST /connections/:id/disconnect — revoke. Permission-gated: caller
//
//	must own the row.
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
	// GET /connections/:id/credentials
	if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "credentials" {
		s.handleCallbackConnectionCredentials(w, r, parts[0])
		return
	}
	if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "public-config" {
		s.handleCallbackConnectionPublicConfig(w, r, parts[0])
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

// handleCallbackConnectionCredentials returns the decrypted credential
// fields for a bound connection. The most sensitive callback in this
// surface — gated by three independent checks:
//
//  1. Install declared platform.connections.read_credentials in its
//     manifest. This is opt-in per app and the dashboard install
//     consent screen flags it specially.
//  2. The connection's slug appears in some
//     requires.integrations[].compatible_slugs entry on this install
//     (can't read creds for an integration the manifest never asked
//     about, even if a binding accidentally points there).
//  3. The connection ID is in the install's integration_bindings —
//     the operator actually bound this connection to a role.
//
// Owner-bypass and operator-bypass paths used by ExecuteIntegrationTool
// are intentionally NOT honored here. Those bypasses are appropriate
// for tool-call brokering (the runner sees the creds, the app
// doesn't); for raw-credential reads we want every release to be
// traceable to a manifest-declared role + operator binding.
//
// Each successful read appends a row to connection_credential_reads.
func (s *Server) handleCallbackConnectionCredentials(w http.ResponseWriter, r *http.Request, idStr string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	connID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}

	// 1. Permission gate.
	if !installHasPermission(s, installID, sdk.PermConnectionsReadCredentials) {
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsReadCredentials), http.StatusForbidden)
		return
	}

	// 2. & 3. Binding + slug compatibility.
	role, bound := installBoundConnection(s, installID, connID)
	if !bound {
		http.Error(w, "connection not bound to this install", http.StatusForbidden)
		return
	}
	dep, derr := installRoleDep(s, installID, role)
	if derr != nil {
		http.Error(w, derr.Error(), http.StatusInternalServerError)
		return
	}

	userID := getUserID(r)
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if dep != nil && dep.Kind != "app" && len(dep.CompatibleSlugs) > 0 && !contains(dep.CompatibleSlugs, conn.AppSlug) {
		http.Error(w, fmt.Sprintf("connection slug %q not in role %q compatible_slugs", conn.AppSlug, role), http.StatusForbidden)
		return
	}

	// Decrypt + parse.
	fields := map[string]string{}
	if encCreds != "" {
		plain, derr := Decrypt(s.secret, encCreds)
		if derr != nil {
			http.Error(w, "decrypt failed", http.StatusInternalServerError)
			return
		}
		// Catalog credentials are stored as a flat string-string map.
		// Coerce non-string values (rare) to their JSON repr so the
		// caller still gets something usable rather than a parse error.
		raw := map[string]any{}
		if jerr := json.Unmarshal([]byte(plain), &raw); jerr != nil {
			http.Error(w, "parse creds failed", http.StatusInternalServerError)
			return
		}
		for k, v := range raw {
			switch tv := v.(type) {
			case string:
				fields[k] = tv
			default:
				if b, _ := json.Marshal(tv); b != nil {
					fields[k] = string(b)
				}
			}
		}
	}

	log.Printf("[CRED-READ] install=%d conn=%d slug=%s role=%s fields=%d",
		installID, connID, conn.AppSlug, role, len(fields))

	writeJSON(w, sdk.ConnectionCredentials{
		ConnectionID: conn.ID,
		Slug:         conn.AppSlug,
		Fields:       fields,
		FetchedAt:    time.Now().UTC(),
	})
}

func (s *Server) handleCallbackConnectionPublicConfig(w http.ResponseWriter, r *http.Request, idStr string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	connID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	if !installHasPermission(s, installID, sdk.PermConnectionsReadPublicConfig) {
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsReadPublicConfig), http.StatusForbidden)
		return
	}
	role, bound := installBoundConnection(s, installID, connID)
	if !bound {
		http.Error(w, "connection not bound to this install", http.StatusForbidden)
		return
	}
	dep, derr := installRoleDep(s, installID, role)
	if derr != nil {
		http.Error(w, derr.Error(), http.StatusInternalServerError)
		return
	}
	userID := getUserID(r)
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if dep != nil && dep.Kind != "app" && len(dep.CompatibleSlugs) > 0 && !contains(dep.CompatibleSlugs, conn.AppSlug) {
		http.Error(w, fmt.Sprintf("connection slug %q not in role %q compatible_slugs", conn.AppSlug, role), http.StatusForbidden)
		return
	}
	if s.catalog == nil {
		http.Error(w, "integration catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "integration catalog entry not found", http.StatusNotFound)
		return
	}
	publicNames := map[string]bool{}
	for _, field := range app.Auth.CredentialFields {
		if field.Exposure == "public" {
			publicNames[field.Name] = true
		}
	}
	fields := map[string]string{}
	if encCreds != "" && len(publicNames) > 0 {
		plain, derr := Decrypt(s.secret, encCreds)
		if derr != nil {
			http.Error(w, "decrypt failed", http.StatusInternalServerError)
			return
		}
		raw := map[string]any{}
		if jerr := json.Unmarshal([]byte(plain), &raw); jerr != nil {
			http.Error(w, "parse creds failed", http.StatusInternalServerError)
			return
		}
		for name := range publicNames {
			if value, ok := raw[name]; ok {
				fields[name] = fmt.Sprint(value)
			}
		}
	}
	writeJSON(w, sdk.ConnectionPublicConfig{
		ConnectionID: conn.ID,
		Slug:         conn.AppSlug,
		Fields:       fields,
		FetchedAt:    time.Now().UTC(),
	})
}

// connectionCreatedVia reads the created_via column on connections —
// 'integration' (operator-installed via Settings → Integrations),
// 'app_install' (created by an app via platform.oauth.start), or ” /
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
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	agent, err := s.callbackAgentForInstall(r, installID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, sdk.PlatformInstance{
			ID: agent.ID, Name: agent.Name, Status: agent.Status,
			Mode: agent.Mode, ProjectID: agent.ProjectID,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "event" && r.Method == http.MethodPost {
		var body struct {
			Message  json.RawMessage `json:"message"`
			ThreadID string          `json:"thread_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if len(body.Message) == 0 || string(body.Message) == "null" {
			http.Error(w, "message required", http.StatusBadRequest)
			return
		}
		port := s.agents.GetPort(id)
		if port == 0 {
			http.Error(w, "agent is not running", http.StatusBadGateway)
			return
		}
		var message any
		if err := json.Unmarshal(body.Message, &message); err != nil {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		payload := map[string]any{"message": message}
		if strings.TrimSpace(body.ThreadID) != "" {
			payload["thread_id"] = strings.TrimSpace(body.ThreadID)
		}
		raw, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/event", port), bytes.NewReader(raw))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if key := s.agents.GetCoreAPIKey(id); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, "core event: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			raw, _ := io.ReadAll(resp.Body)
			http.Error(w, fmt.Sprintf("core event http %d: %s", resp.StatusCode, string(raw)), http.StatusBadGateway)
			return
		}
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
//  1. Caller is a sidecar (X-Apteva-App-Install-ID set by middleware).
//  2. Install's manifest declares the platform.connections.execute
//     permission.
//  3. The connection is reachable by this install — one of:
//     a. connID appears in the install's integration_bindings, OR
//     b. owner_app_install_id == installID (the app created this
//     connection itself via platform.oauth.start), OR
//     c. created_via='integration' (operator-installed in Settings
//     → Integrations) — any permitted install in the same user's
//     scope may call it. Operator connections are explicitly
//     shared resources; gating them behind a separate role-bind
//     ceremony defeats their purpose.
//  4. When the role is bound (3a), the connection's app_slug must be
//     in the role's compatible_slugs. Skipped for 3b/3c which have
//     no role-dep to validate against.
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
	// 50 MiB is generous enough for typical file-bearing tool calls
	// (storage.files_upload with a base64 PDF, media uploads, etc.).
	// 1 MiB was the prior cap and silently truncated mid-base64 for
	// any input larger than ~700 KB raw → cryptic "invalid json"
	// errors with no body info. The error includes err.Error() now
	// so EOF mid-decode is distinguishable from real malformed JSON.
	if err := json.NewDecoder(io.LimitReader(r.Body, 50<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	delegatedProject, executionInput := sanitizeIntegrationCallbackInput(body.Input)

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
		// Dynamic bypass — caller declares requires.dynamic_integration_
		// access and is identified as official (apps_dynamic_call.go).
		// Project isolation is preserved: the connection's project_id
		// must match the caller install's.
		if ok, msg := s.resolveDynamicIntegration(installID, connID, conn.ProjectID, delegatedProject); ok {
			log.Printf("[INTEGRATIONS-EXEC] grant=dynamic install=%d conn=%d slug=%s", installID, connID, conn.AppSlug)
		} else if msg != "" {
			// Eligible caller, wrong project — distinct diagnostic so
			// consumers can tell this apart from "not eligible".
			log.Printf("[INTEGRATIONS-EXEC] DENY install=%d conn=%d reason=%s", installID, connID, msg)
			http.Error(w, msg, http.StatusForbidden)
			return
		} else {
			log.Printf("[INTEGRATIONS-EXEC] DENY install=%d conn=%d slug=%s reason=not-bound-not-owned-not-operator owner=%d created_via=%q",
				installID, connID, conn.AppSlug, ownerID, createdVia)
			http.Error(w, "connection not reachable by this install (not bound, not owned, not operator-installed)", http.StatusForbidden)
			return
		}
	}

	// Resolve catalog tool. Accept the same three forms as
	// handleExecuteTool (the dashboard-facing /connections/:id/execute):
	// bare name, canonical MCP-prefixed form (what /tools returns), or
	// the legacy app-slug-prefixed form. Sidecars discovering tools via
	// /api/connections/:id/tools see the prefixed name, so refusing it
	// here makes the workflows-builder picker hand back tool names that
	// the executor rejects.
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "integration app not in catalog: "+conn.AppSlug, http.StatusBadGateway)
		return
	}
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == body.Tool || prefix+"_"+t.Name == body.Tool || conn.AppSlug+"_"+t.Name == body.Tool {
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

	if grant, ok, err := parseDelegatedProviderCredentials(plain); err != nil {
		http.Error(w, "delegated provider credentials invalid: "+err.Error(), http.StatusBadGateway)
		return
	} else if ok {
		result, err := s.executeDelegatedProviderTool(installID, connID, conn, grant, tool.Name, executionInput)
		if err != nil {
			writeJSON(w, map[string]any{"success": false, "data": err.Error()})
			return
		}
		if result == nil {
			writeJSON(w, map[string]any{"success": true})
			return
		}
		writeJSON(w, result)
		return
	}

	ctx, err := s.resolveConnectionContext(userID, app, credentials, executionInput)
	if err != nil {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, installID, s.callerAppName(installID), tool.Name, executionInput, nil, err))
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
	environmentID := r.Header.Get("X-Apteva-Environment-Id")
	if environmentID == "" {
		environmentID = r.Header.Get("X-Apteva-Environment-Id")
	}
	result, err := executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, environmentID, persist)
	if err != nil {
		if ev, ok := delegatedUsageFromHeaders(r, connID, conn, tool.Name, executionInput, "error", err.Error()); ok {
			s.recordDelegatedProviderUsage(ev)
		} else {
			s.recordIntegrationUsage(integrationUsageFromResult(conn, installID, s.callerAppName(installID), tool.Name, executionInput, nil, err))
		}
		writeJSON(w, map[string]any{"success": false, "data": err.Error()})
		return
	}
	if ev, ok := delegatedUsageFromHeaders(r, connID, conn, tool.Name, executionInput, "success", ""); ok {
		ev.Quantity, ev.Unit, _ = integrationUsageMetric(conn, tool.Name, executionInput, result)
		if result != nil && (!result.Success || result.Status >= 400) {
			ev.Status = "error"
			ev.Error = truncate(fmt.Sprintf("%v", result.Data), 500)
		}
		s.recordDelegatedProviderUsage(ev)
	} else {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, installID, s.callerAppName(installID), tool.Name, executionInput, result, nil))
	}
	// Match handleExecuteTool's response shape. The SDK caller can
	// json.Unmarshal the data field into whatever type they expect.
	if result == nil {
		writeJSON(w, map[string]any{"success": true})
		return
	}
	writeJSON(w, result)
}

// sanitizeIntegrationCallbackInput separates server-owned routing metadata
// from provider input. App callbacks may use _project_id to authorize a
// dynamic integration call, but upstream APIs must never receive that field.
func sanitizeIntegrationCallbackInput(input map[string]any) (string, map[string]any) {
	projectID, _ := input["_project_id"].(string)
	clean := make(map[string]any, len(input))
	for key, value := range input {
		if key == "_project_id" {
			continue
		}
		clean[key] = value
	}
	return strings.TrimSpace(projectID), clean
}

// ─── /apps/:appName/call ───────────────────────────────────────────

// POST /apps/:appName/call
//
// Body: {"tool": "<tool name>", "input": {...}}
//
// Authorization:
//  1. Caller is a sidecar (X-Apteva-App-Install-ID set).
//  2. Install's manifest declares platform.apps.call permission.
//  3. appName appears in the install's integration_bindings (under
//     a kind=app dep).
//
// On success, calls the target app's /mcp endpoint via the same
// proxy machinery the dashboard uses — credentials in the form of
// the target's APTEVA_APP_TOKEN are injected by handleAppProxy.
func (s *Server) handleCallbackApps(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 2 && parts[1] == "proxy" {
		s.handleCallbackAppProxy(w, r, parts[0], parts[2:])
		return
	}
	if len(parts) != 2 || parts[1] != "call" || r.Method != http.MethodPost {
		http.Error(w, "use /apps/:name/call or /apps/:name/proxy/*", http.StatusMethodNotAllowed)
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
	// 50 MiB is generous enough for typical file-bearing tool calls
	// (storage.files_upload with a base64 PDF, media uploads, etc.).
	// 1 MiB was the prior cap and silently truncated mid-base64 for
	// any input larger than ~700 KB raw → cryptic "invalid json"
	// errors with no body info. The error includes err.Error() now
	// so EOF mid-decode is distinguishable from real malformed JSON.
	if err := json.NewDecoder(io.LimitReader(r.Body, 50<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
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
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	requestedProjectID, err := appCallProjectArg(body.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	effectiveProjectID, ok := s.appCallProject(w, installID, requestedProjectID)
	if !ok {
		return
	}
	// Resolve the binding's target install_id. The binding is
	// authoritative — last-wins GetByName(targetAppName) silently
	// dispatches to whichever install was registered last in
	// byName, which misroutes when multiple project-scoped installs
	// of the target exist. Using the bound install_id directly
	// keeps the call inside the project context the operator
	// originally wired up.
	targetInstallID := installBoundAppID(s, installID, targetAppName)
	if targetInstallID == 0 {
		// No static binding — try the dynamic bypass for callers
		// that declare requires.dynamic_app_calls and are identified
		// as official (apps_dynamic_call.go). resolveDynamicTarget
		// returns the correct 403 message on failure so consumers
		// can tell "not eligible" apart from "eligible but target
		// absent".
		id, msg, ok := s.resolveDynamicTarget(installID, targetAppName, effectiveProjectID)
		if !ok {
			http.Error(w, msg, http.StatusForbidden)
			return
		}
		targetInstallID = id
	}
	target := s.installedApps.Get(targetInstallID)
	if target == nil {
		http.Error(w, "target app not running: "+targetAppName, http.StatusBadGateway)
		return
	}
	if target.SidecarURL == "" {
		http.Error(w, "target app has no sidecar URL", http.StatusBadGateway)
		return
	}
	if target.ProjectID != "" {
		if effectiveProjectID == "" {
			// Compatibility for global callers with an exact binding to a
			// project-scoped target. Older SDKs did not send _project_id;
			// the binding itself makes the intended project unambiguous.
			effectiveProjectID, ok = s.appCallProject(w, installID, target.ProjectID)
			if !ok {
				return
			}
		} else if effectiveProjectID != target.ProjectID {
			http.Error(w, "project_id does not match target app install", http.StatusForbidden)
			return
		}
	}
	// Replace rather than preserve routing metadata. The value above is
	// pinned by the caller install or validated against its owning user.
	delete(body.Input, "_project_id")
	injectProjectArgAny(body.Input, effectiveProjectID)

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

// appCallProjectArg reads the SDK's delegated project routing field. It is
// accepted only as a string and is never forwarded until appCallProject has
// pinned or authorized it.
func appCallProjectArg(input map[string]any) (string, error) {
	raw, present := input["_project_id"]
	if !present || raw == nil {
		return "", nil
	}
	projectID, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("_project_id must be a string")
	}
	return strings.TrimSpace(projectID), nil
}

// appCallProject resolves the effective project for an authenticated app
// callback. Project installs are pinned to their installation project. Global
// installs may delegate a project only when their owning user can access it.
// An empty project remains valid for genuinely global app tools.
func (s *Server) appCallProject(w http.ResponseWriter, installID int64, requestedProjectID string) (string, bool) {
	var (
		installProjectID string
		ownerUserID      int64
	)
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(project_id,''), COALESCE(installed_by,0) FROM app_installs WHERE id=?`,
		installID,
	).Scan(&installProjectID, &ownerUserID); err != nil || ownerUserID == 0 {
		http.Error(w, "install not found", http.StatusUnauthorized)
		return "", false
	}
	requestedProjectID = strings.TrimSpace(requestedProjectID)
	if installProjectID != "" {
		if requestedProjectID != "" && requestedProjectID != installProjectID {
			http.Error(w, "app install is scoped to another project", http.StatusForbidden)
			return "", false
		}
		return installProjectID, true
	}
	if requestedProjectID == "" {
		return "", true
	}
	if s.store.GetPlatformRole(ownerUserID) != PlatformAdmin {
		role, err := s.store.GetProjectRole(requestedProjectID, ownerUserID)
		if err != nil || role.Rank() < ProjectViewer.Rank() {
			http.Error(w, "insufficient role on project", http.StatusForbidden)
			return "", false
		}
	}
	return requestedProjectID, true
}

// handleCallbackAppProxy is the streaming counterpart to CallApp.
// It is intentionally mounted below the authenticated callback
// surface rather than /api/apps/<name>: the caller keeps its own app
// credential, the server verifies platform.apps.call plus the exact
// requires.apps binding, then swaps in the target install's token.
// This supports large downloads, resumable uploads, Range requests,
// and SSE without exposing target credentials or weakening the
// ordinary app proxy's same-install credential rule.
func (s *Server) handleCallbackAppProxy(w http.ResponseWriter, r *http.Request, targetAppName string, tailParts []string) {
	callerInstallID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if targetAppName == "" || len(tailParts) == 0 {
		http.Error(w, "target app and proxy path required", http.StatusBadRequest)
		return
	}
	if !installHasPermission(s, callerInstallID, sdk.PermAppsCall) {
		http.Error(w, "missing permission: "+string(sdk.PermAppsCall), http.StatusForbidden)
		return
	}
	targetInstallID := installBoundAppID(s, callerInstallID, targetAppName)
	if targetInstallID == 0 {
		http.Error(w, "target app is not bound: "+targetAppName, http.StatusForbidden)
		return
	}
	target := s.installedApps.Get(targetInstallID)
	if target == nil || target.SidecarURL == "" {
		http.Error(w, "target app not reachable: "+targetAppName, http.StatusBadGateway)
		return
	}
	q := r.URL.Query()
	callerUserID, requestedProjectID, ok := s.runtimeCallerProject(
		w, r, callerInstallID, q.Get("project_id"), ProjectViewer,
	)
	if !ok {
		return
	}
	if target.ProjectID != "" && requestedProjectID != target.ProjectID {
		http.Error(w, "project_id does not match bound target install", http.StatusForbidden)
		return
	}
	q.Set("project_id", requestedProjectID)
	r.URL.RawQuery = q.Encode()
	for _, part := range tailParts {
		if part == "." || part == ".." {
			http.Error(w, "invalid proxy path", http.StatusBadRequest)
			return
		}
	}

	upstream, err := url.Parse(target.SidecarURL)
	if err != nil {
		http.Error(w, "invalid target sidecar URL", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = "/" + strings.Join(tailParts, "/")
		req.URL.RawPath = ""
		req.Header.Set("X-User-ID", strconv.FormatInt(callerUserID, 10))
		req.Header.Del("X-Apteva-Project-ID")
		if requestedProjectID != "" {
			req.Header.Set("X-Apteva-Project-ID", requestedProjectID)
		}
		req.Header.Del("X-Apteva-Subject-Type")
		req.Header.Del("X-Apteva-Original-Authorization")
		if target.Token != "" {
			req.Header.Set("Authorization", "Bearer "+target.Token)
		} else {
			req.Header.Del("Authorization")
		}
		req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(target.InstallID, 10))
		req.Header.Set("X-Apteva-Bound-Caller-Install-ID", strconv.FormatInt(callerInstallID, 10))
	}
	proxy.ServeHTTP(w, r)
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
	conn, authURL, err := s.startLocalOAuth(userID, app, name, pid, "", "", nil, installID, body.ReturnURL, nil)
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

// ─── /threads ──────────────────────────────────────────────────────
//
// Surface for app-spawned sub-threads, including realtime (voice/audio)
// threads bridged by the calling app:
//
//   POST   /threads/spawn-realtime — create a realtime thread inside
//                                    a target agent; return the audio
//                                    bridge URL the app dials to pipe
//                                    PCM frames.
//   DELETE /threads/{id}           — kill a thread the app spawned.
//                                    Idempotent — 404 on unknown id is
//                                    treated as success.
//
// Both paths require platform.realtime.spawn in the install's manifest.
// The target agent (RealtimeSpawnRequest.AgentID) must be owned by the
// install's user — otherwise installs could spawn threads inside other
// users' agents.

func (s *Server) handleCallbackThreads(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermRealtimeSpawn) {
		http.Error(w, "missing permission: "+string(sdk.PermRealtimeSpawn), http.StatusForbidden)
		return
	}

	switch {
	case len(parts) == 1 && parts[0] == "spawn-realtime" && r.Method == http.MethodPost:
		s.handleCallbackSpawnRealtime(w, r, installID)
	case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete:
		s.handleCallbackKillThread(w, r, installID, parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "audio-token" && r.Method == http.MethodPost:
		s.handleCallbackRenewRealtimeAudio(w, r, installID, parts[0])
	default:
		http.Error(w, "unsupported threads operation", http.StatusNotFound)
	}
}

func (s *Server) handleCallbackSpawnRealtime(w http.ResponseWriter, r *http.Request, installID int64) {
	var body sdk.RealtimeSpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 || body.ThreadID == "" || body.Directive == "" {
		http.Error(w, "agent_id, thread_id, directive all required", http.StatusBadRequest)
		return
	}

	// Authorize: the install's owning user must also own the target
	// agent. Prevents enumeration of other users' agents via
	// well-known thread ids.
	agent, err := s.callbackAgentForInstall(r, installID, body.AgentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// Apps that omit both capability fields inherit the target agent's
	// spawnable MCP surface. Explicit MCP names or a narrower tool allowlist
	// remain authoritative. Server resolves the concrete names so Core stays
	// provider- and caller-agnostic.
	if body.MCP == nil && body.Tools == nil {
		body.MCP, err = s.agentSpawnableMCPNames(agent.ID)
		if err != nil {
			log.Printf("[REALTIME-SPAWN] install=%d agent=%d load MCPs: %v", installID, body.AgentID, err)
			http.Error(w, "load agent realtime capabilities: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	inst := framework.InstanceInfo{
		ID:         agent.ID,
		Name:       agent.Name,
		UserID:     agent.UserID,
		ProjectID:  agent.ProjectID,
		Port:       s.agents.GetPort(agent.ID),
		CoreAPIKey: s.agents.GetCoreAPIKey(agent.ID),
	}
	res, err := s.resolver().SpawnRealtimeThread(inst, body)
	if err != nil {
		log.Printf("[REALTIME-SPAWN] install=%d agent=%d thread=%q: %v", installID, body.AgentID, body.ThreadID, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Return the server's public, token-scoped proxy URL. Sidecars run in
	// separate containers, where 127.0.0.1 is not the host/core process.
	// The proxy authenticates to core without exposing its long-lived key.
	if res.AudioToken != "" && inst.Port != 0 {
		baseURL := callbackReachableBaseURL(s.publicBaseURL(), r)
		bridgeURL, parseErr := publicRealtimeAudioURL(baseURL, body.AgentID, body.ThreadID, res.AudioToken)
		if parseErr != nil {
			http.Error(w, "invalid public server URL", http.StatusInternalServerError)
			return
		}
		res.AudioBridgeURL = bridgeURL
	}
	log.Printf("[REALTIME-SPAWN] install=%d agent=%d thread=%q status=%s",
		installID, body.AgentID, body.ThreadID, res.Status)
	writeJSON(w, res)
}

type callbackMCPServerConfig struct {
	Name    string `json:"name"`
	NoSpawn bool   `json:"no_spawn"`
}

func (s *Server) agentSpawnableMCPNames(agentID int64) ([]string, error) {
	port := s.agents.GetPort(agentID)
	if port == 0 {
		return nil, errors.New("agent is not running")
	}
	target := fmt.Sprintf("http://127.0.0.1:%d/config", port)
	resp, err := s.coreDoWithBootWait(agentID, http.MethodGet, target, nil, s.agents.GetCoreAPIKey(agentID))
	if err != nil {
		return nil, fmt.Errorf("read agent config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("read agent config: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var config struct {
		MCPServers []callbackMCPServerConfig `json:"mcp_servers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&config); err != nil {
		return nil, fmt.Errorf("decode agent config: %w", err)
	}
	return spawnableMCPNames(config.MCPServers), nil
}

func spawnableMCPNames(servers []callbackMCPServerConfig) []string {
	names := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		system := name == "apteva-server" || isServerOwnedOutputMCP(name)
		if name == "" || server.NoSpawn || system || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func (s *Server) handleCallbackKillThread(w http.ResponseWriter, r *http.Request, installID int64, threadID string) {
	// We don't know which agent owns the thread from the path alone —
	// the caller's install scope is the discriminator. For v1 we
	// require the caller to pass agent_id as a query param so the
	// server doesn't have to scan every running instance for the id.
	agentParam := r.URL.Query().Get("agent_id")
	if agentParam == "" {
		http.Error(w, "agent_id query param required for kill", http.StatusBadRequest)
		return
	}
	agentID, err := strconv.ParseInt(agentParam, 10, 64)
	if err != nil || agentID <= 0 {
		http.Error(w, "invalid agent_id", http.StatusBadRequest)
		return
	}
	agent, err := s.callbackAgentForInstall(r, installID, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	inst := framework.InstanceInfo{
		ID: agent.ID, Name: agent.Name, UserID: agent.UserID, ProjectID: agent.ProjectID,
		Port: s.agents.GetPort(agent.ID), CoreAPIKey: s.agents.GetCoreAPIKey(agent.ID),
	}
	if err := s.resolver().KillThread(inst, threadID); err != nil {
		log.Printf("[REALTIME-KILL] install=%d agent=%d thread=%q: %v", installID, agentID, threadID, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCallbackRenewRealtimeAudio(w http.ResponseWriter, r *http.Request, installID int64, threadID string) {
	agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	if err != nil || agentID <= 0 {
		http.Error(w, "valid agent_id query param required", http.StatusBadRequest)
		return
	}
	agent, err := s.callbackAgentForInstall(r, installID, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	inst := framework.InstanceInfo{
		ID: agent.ID, Name: agent.Name, UserID: agent.UserID, ProjectID: agent.ProjectID,
		Port: s.agents.GetPort(agent.ID), CoreAPIKey: s.agents.GetCoreAPIKey(agent.ID),
	}
	res, err := s.resolver().RenewRealtimeAudioBridge(inst, threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if res.AudioToken != "" {
		baseURL := callbackReachableBaseURL(s.publicBaseURL(), r)
		bridgeURL, parseErr := publicRealtimeAudioURL(baseURL, agentID, threadID, res.AudioToken)
		if parseErr != nil {
			http.Error(w, "invalid public server URL", http.StatusInternalServerError)
			return
		}
		res.AudioBridgeURL = bridgeURL
	}
	writeJSON(w, res)
}

func (s *Server) callbackAgentForInstall(r *http.Request, installID, agentID int64) (*Agent, error) {
	agent, err := s.store.GetAgent(getUserID(r), agentID)
	if err != nil || agent == nil {
		return nil, errors.New("agent not found or not owned by this user")
	}
	var installProject string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&installProject); err != nil {
		return nil, errors.New("app installation not found")
	}
	if installProject != "" && installProject != agent.ProjectID && !s.runtimeContainsInstallAndAgent(installID, agentID) {
		return nil, errors.New("agent does not belong to the app installation project")
	}
	return agent, nil
}

func (s *Server) runtimeContainsInstallAndAgent(installID, agentID int64) bool {
	if s.environments == nil || installID <= 0 || agentID <= 0 {
		return false
	}
	for _, runtime := range s.environments.List() {
		if runtime.GetAgent(agentID) == nil {
			continue
		}
		for _, name := range runtime.InstallNames() {
			if install, ok := runtime.Install(name); ok && install.InstallID == installID {
				return true
			}
		}
	}
	return false
}

// resolver returns the canonical serverResolver used for forwarding
// to core. Allocated once per call — cheap because it just wraps
// *Server.
func (s *Server) resolver() *serverResolver { return &serverResolver{srv: s} }

// installHasPermission checks the install's approved permission snapshot.
// app_installs.permissions_json is the consent boundary: a manifest update
// may declare new platform permissions, but those permissions are not granted
// until the install row is explicitly backfilled/approved.
func installHasPermission(s *Server, installID int64, perm sdk.Permission) bool {
	var rawPerms string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(permissions_json, '[]') FROM app_installs WHERE id=?`, installID,
	).Scan(&rawPerms); err == nil {
		var perms []sdk.Permission
		if json.Unmarshal([]byte(rawPerms), &perms) == nil {
			for _, p := range perms {
				if p == perm {
					return true
				}
			}
		}
	}
	return false
}

// installManifest pulls + parses the manifest_json for the install's app.
func installManifest(s *Server, installID int64) (*sdk.Manifest, error) {
	var raw string
	err := s.store.db.QueryRow(
		`SELECT COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
		 FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID,
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
		if appBindingContains(raw, connID) {
			return role, true
		}
	}
	return "", false
}

// installBoundApp returns true if the named app name appears as a
// dependency of the install. Two manifest shapes are recognised:
//
//   - Modern: requires.apps[].name (RequiredAppRef). Bindings are
//     keyed by the dep's app name.
//   - Legacy: requires.integrations[].kind="app" (IntegrationDep).
//     Bindings are keyed by the integration's role.
//
// The bound install must be persisted as running and its AppName must match
// the requested name. Runtime reachability is checked separately by the
// callback handler. Keeping authorization independent from the in-memory
// registry prevents a valid binding from becoming a false 403 during server
// boot, before all sidecars have been registered.
func installBoundApp(s *Server, installID int64, appName string) bool {
	return installBoundAppID(s, installID, appName) != 0
}

// installBoundAppID is the binding-aware sister to installBoundApp:
// returns the install_id of the target app the caller is bound to,
// or 0 when no binding (or the bound install isn't running). Used by
// handleCallbackApps to dispatch app→app calls to the EXACT bound
// install — last-wins GetByName misroutes when multiple project-
// scoped installs of the target exist.
func installBoundAppID(s *Server, installID int64, appName string) int64 {
	m, err := installManifest(s, installID)
	if err != nil || m == nil {
		return 0
	}
	bindings := bindingsForInstall(s, installID)
	resolve := func(key string) int64 {
		raw, ok := bindings[key]
		if !ok || raw == nil {
			return 0
		}
		ids, defaultID := appBindingIDs(raw)
		boundInstallID := defaultID
		if boundInstallID == 0 && len(ids) > 0 {
			boundInstallID = ids[0]
		}
		if boundInstallID == 0 {
			return 0
		}
		var boundName, status string
		if err := s.store.db.QueryRow(
			`SELECT a.name, i.status
				   FROM app_installs i JOIN apps a ON a.id = i.app_id
				  WHERE i.id = ?`,
			boundInstallID,
		).Scan(&boundName, &status); err != nil || boundName != appName || status != "running" {
			return 0
		}
		return boundInstallID
	}
	for _, dep := range m.Requires.Apps {
		if dep.Name != appName {
			continue
		}
		if id := resolve(dep.Name); id != 0 {
			return id
		}
	}
	for _, dep := range m.Requires.Integrations {
		if dep.Kind != "app" {
			continue
		}
		if id := resolve(dep.Role); id != 0 {
			return id
		}
	}
	return 0
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
