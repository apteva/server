package main

// Per-(install, instance) authorization grants — the storage backend
// for the parameterized-permissions feature. Two surfaces:
//
//   /api/apps/installs/:id/permissions
//                    /grants[?instance_id=N]
//                    /grants/by-instance/:iid          (PUT replace)
//                    /grants/evaluate                  (POST dry-run)
//                    /default-effect                   (PUT)
//     dashboard-facing — operator writes the policy through these.
//
//   /api/apps/callback/grants?instance_id=N
//     sidecar-facing — the app-sdk MCP handler fetches the live policy
//     for the calling agent before dispatching a tool call. Auth is
//     APTEVA_APP_TOKEN, resolved to the calling install's id by
//     authMiddleware.
//
// Back-compat: installs default to default_effect='allow' so an app
// that doesn't declare provides.permissions has identical behavior to
// before the migration. Installs flip to 'deny' the moment an operator
// wants fail-closed enforcement. Apps without a Requires annotation on
// their MCP tools also bypass the gate — the SDK only enforces when
// both ends opt in.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// ─── Types shared across surfaces ──────────────────────────────────

// grantRow mirrors one row of app_grants. JSON-serialized to dashboard
// + SDK alike.
type grantRow struct {
	ID         int64  `json:"id,omitempty"`
	Effect     string `json:"effect"`
	Permission string `json:"permission"`
	Resource   string `json:"resource"`
}

type grantsResponse struct {
	DefaultEffect string     `json:"default_effect"`
	Grants        []grantRow `json:"grants"`
}

// ─── Sidecar-facing: GET /api/apps/callback/grants?instance_id=N ───
//
// Called by app-sdk's mcpHandler.buildCaller on every tool call where
// X-Apteva-Caller-Agent was forwarded. Returns the policy for the
// (calling install, requested instance) pair, including default_effect
// from the install row. Empty rules + default 'allow' = full access
// (the back-compat default).

func (s *Server) handleCallbackGrants(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) > 0 && parts[0] != "" {
		http.Error(w, "no path segments expected", http.StatusNotFound)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Phase 2 — accept agent_id (canonical) and instance_id (legacy).
	instanceIDStr := readAgentIDParam(r)
	if instanceIDStr == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	instanceID, err := strconv.ParseInt(instanceIDStr, 10, 64)
	if err != nil || instanceID <= 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	resp, err := s.fetchGrants(installID, instanceID)
	if err != nil {
		http.Error(w, "grants: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// ─── Dashboard-facing: handleInstallGrants ─────────────────────────
//
// Routed from main.go's /apps/installs/ switch when the suffix is
// /permissions or /grants[/...] or /default-effect. handleInstallGrants
// dispatches by suffix + method.

func (s *Server) handleInstallGrants(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid install id", http.StatusBadRequest)
		return
	}
	switch parts[1] {
	case "permissions":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleListPermissionsCatalog(w, r, installID)
	case "default-effect":
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		s.handleSetDefaultEffect(w, r, installID)
	case "grants":
		s.handleGrantsRouter(w, r, installID, parts[2:])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleGrantsRouter(w http.ResponseWriter, r *http.Request, installID int64, sub []string) {
	switch {
	case len(sub) == 0:
		switch r.Method {
		case http.MethodGet:
			s.handleListGrants(w, r, installID)
		case http.MethodPost:
			s.handleAddGrant(w, r, installID)
		default:
			http.Error(w, "GET|POST only", http.StatusMethodNotAllowed)
		}
	case len(sub) == 2 && sub[0] == "by-instance":
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		s.handleReplaceGrantsByInstance(w, r, installID, sub[1])
	case len(sub) == 1 && sub[0] == "evaluate":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.handleEvaluateGrant(w, r, installID)
	case len(sub) == 1:
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		s.handleDeleteGrant(w, r, installID, sub[0])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// GET /permissions — return the catalog (resources + permissions) the
// app declared in its manifest, plus the per-tool requires/resource_from
// annotations so the dashboard can render an "Effective tools" view.
func (s *Server) handleListPermissionsCatalog(w http.ResponseWriter, r *http.Request, installID int64) {
	manifest, err := s.loadInstallManifest(installID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	type toolEntry struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Requires     string `json:"requires,omitempty"`
		ResourceFrom string `json:"resource_from,omitempty"`
	}
	tools := make([]toolEntry, 0, len(manifest.Provides.MCPTools))
	for _, t := range manifest.Provides.MCPTools {
		tools = append(tools, toolEntry{
			Name: t.Name, Description: t.Description,
			Requires: t.Requires, ResourceFrom: t.ResourceFrom,
		})
	}
	writeJSON(w, map[string]any{
		"resources":   manifest.Provides.Resources,
		"permissions": manifest.Provides.ProvidedPermissions,
		"tools":       tools,
	})
}

// GET /grants?instance_id=N — list rules for one agent (or all, when
// instance_id is omitted). Returns default_effect alongside.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request, installID int64) {
	instanceID, err := optionalInstanceID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defaultEffect := s.installDefaultEffect(installID)
	if instanceID > 0 {
		defaultEffect = s.defaultEffectForAgent(installID, instanceID)
	}
	rules, err := s.queryGrants(installID, instanceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, grantsResponse{DefaultEffect: defaultEffect, Grants: rules})
}

// POST /grants — add one rule. Body: {agent_id, effect, permission, resource}.
// instance_id is accepted as a legacy alias for back-compat.
func (s *Server) handleAddGrant(w http.ResponseWriter, r *http.Request, installID int64) {
	var body struct {
		AgentID    int64  `json:"agent_id"`
		InstanceID int64  `json:"instance_id"` // legacy alias
		Effect     string `json:"effect"`
		Permission string `json:"permission"`
		Resource   string `json:"resource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 {
		body.AgentID = body.InstanceID
	}
	if body.AgentID <= 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	manifest, err := s.loadInstallManifest(installID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := validateGrantBody(manifest, body.Effect, body.Permission); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Resource == "" {
		body.Resource = "*"
	}
	res, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO app_grants(install_id, agent_id, effect, permission, resource, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		installID, body.AgentID, body.Effect, body.Permission, body.Resource, getUserName(r),
	)
	if err != nil {
		http.Error(w, "insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, grantRow{ID: id, Effect: body.Effect, Permission: body.Permission, Resource: body.Resource})
}

// PUT /grants/by-instance/:iid — declarative replace. Body: {default_effect?, rules: [...]}.
// The whole policy for one agent is swapped atomically.
func (s *Server) handleReplaceGrantsByInstance(w http.ResponseWriter, r *http.Request, installID int64, iidStr string) {
	instanceID, err := strconv.ParseInt(iidStr, 10, 64)
	if err != nil || instanceID <= 0 {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return
	}
	var body struct {
		DefaultEffect string     `json:"default_effect,omitempty"`
		Rules         []grantRow `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	resp, err := s.replaceGrantsForInstance(installID, instanceID, body.DefaultEffect, body.Rules, getUserName(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) replaceGrantsForInstance(installID, instanceID int64, defaultEffect string, rules []grantRow, creator string) (*grantsResponse, error) {
	manifest, err := s.loadInstallManifest(installID)
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if err := validateGrantBody(manifest, rule.Effect, rule.Permission); err != nil {
			return nil, err
		}
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM app_grants WHERE install_id = ? AND agent_id = ?`, installID, instanceID); err != nil {
		return nil, fmt.Errorf("delete: %w", err)
	}
	for _, rule := range rules {
		resource := rule.Resource
		if resource == "" {
			resource = "*"
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO app_grants(install_id, agent_id, effect, permission, resource, created_by)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			installID, instanceID, rule.Effect, rule.Permission, resource, creator,
		); err != nil {
			return nil, fmt.Errorf("insert: %w", err)
		}
	}
	if defaultEffect != "" {
		if defaultEffect != "allow" && defaultEffect != "deny" {
			return nil, errors.New("default_effect must be allow|deny")
		}
		if _, err := tx.Exec(
			`INSERT INTO app_grant_defaults (install_id, agent_id, default_effect, updated_at)
			 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(install_id, agent_id) DO UPDATE SET
			   default_effect=excluded.default_effect,
			   updated_at=CURRENT_TIMESTAMP`,
			installID, instanceID, defaultEffect,
		); err != nil {
			return nil, fmt.Errorf("update default_effect: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	resp, err := s.fetchGrantsForInstance(installID, instanceID)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// DELETE /grants/:id — remove a single rule.
func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request, installID int64, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid grant id", http.StatusBadRequest)
		return
	}
	res, err := s.store.db.Exec(`DELETE FROM app_grants WHERE id = ? AND install_id = ?`, id, installID)
	if err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "grant not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /grants/evaluate — dry-run a (permission, resource) check
// without persisting anything. Powers the dashboard's "Test grant"
// probe. Body: {agent_id, permission, resource}. instance_id accepted
// as legacy alias.
func (s *Server) handleEvaluateGrant(w http.ResponseWriter, r *http.Request, installID int64) {
	var body struct {
		AgentID    int64  `json:"agent_id"`
		InstanceID int64  `json:"instance_id"` // legacy alias
		Permission string `json:"permission"`
		Resource   string `json:"resource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 {
		body.AgentID = body.InstanceID
	}
	if body.AgentID <= 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	manifest, err := s.loadInstallManifest(installID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	resp, err := s.fetchGrantsForInstance(installID, body.AgentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	caller := buildCallerFromGrants(manifest, resp)
	allowed := caller.Allows(body.Permission, body.Resource)
	writeJSON(w, map[string]any{
		"allowed":        allowed,
		"default_effect": resp.DefaultEffect,
		"grants":         resp.Grants,
	})
}

// PUT /default-effect — flip an install's fallback policy. Body: {default_effect}.
func (s *Server) handleSetDefaultEffect(w http.ResponseWriter, r *http.Request, installID int64) {
	var body struct {
		DefaultEffect string `json:"default_effect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.DefaultEffect != "allow" && body.DefaultEffect != "deny" {
		http.Error(w, "default_effect must be allow|deny", http.StatusBadRequest)
		return
	}
	res, err := s.store.db.Exec(`UPDATE app_installs SET default_effect = ? WHERE id = ?`, body.DefaultEffect, installID)
	if err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"default_effect": body.DefaultEffect})
}

// ─── Helpers ───────────────────────────────────────────────────────

// readAgentIDParam returns the agent identifier from the request,
// preferring the canonical `agent_id` query param and falling back to
// the legacy `instance_id` for older SDKs / clients. Returns "" when
// neither is set; the caller decides whether absence is an error.
//
// This lives in apps_grants.go because it's the first handler that
// hit the dual-name need; future handlers in this package should
// share it instead of re-implementing the precedence rule.
func readAgentIDParam(r *http.Request) string {
	if v := r.URL.Query().Get("agent_id"); v != "" {
		return v
	}
	return r.URL.Query().Get("instance_id")
}

func optionalInstanceID(r *http.Request) (int64, error) {
	v := readAgentIDParam(r)
	if v == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("agent_id required")
	}
	return id, nil
}

func (s *Server) installDefaultEffect(installID int64) string {
	var eff string
	err := s.store.db.QueryRow(`SELECT COALESCE(default_effect,'allow') FROM app_installs WHERE id = ?`, installID).Scan(&eff)
	if err != nil || eff == "" {
		return "allow"
	}
	return eff
}

func (s *Server) defaultEffectForAgent(installID, agentID int64) string {
	var eff string
	err := s.store.db.QueryRow(
		`SELECT default_effect FROM app_grant_defaults WHERE install_id = ? AND agent_id = ?`,
		installID, agentID,
	).Scan(&eff)
	if err == nil && eff != "" {
		return eff
	}
	return s.installDefaultEffect(installID)
}

func (s *Server) queryGrants(installID, instanceID int64) ([]grantRow, error) {
	q := `SELECT id, effect, permission, resource FROM app_grants WHERE install_id = ?`
	args := []any{installID}
	if instanceID > 0 {
		q += ` AND agent_id = ?`
		args = append(args, instanceID)
	}
	q += ` ORDER BY id`
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []grantRow{}
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.ID, &g.Effect, &g.Permission, &g.Resource); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// fetchGrants is the underlying query used by the sidecar callback —
// installs id is implicit (resolved from the bearer token), instance is
// required.
func (s *Server) fetchGrants(installID, instanceID int64) (*sdk.GrantsResponse, error) {
	resp, err := s.fetchGrantsForInstance(installID, instanceID)
	if err != nil {
		return nil, err
	}
	out := &sdk.GrantsResponse{DefaultEffect: resp.DefaultEffect}
	for _, g := range resp.Grants {
		out.Grants = append(out.Grants, sdk.Grant{
			Effect: g.Effect, Permission: g.Permission, Resource: g.Resource,
		})
	}
	return out, nil
}

func (s *Server) fetchGrantsForInstance(installID, instanceID int64) (*grantsResponse, error) {
	rules, err := s.queryGrants(installID, instanceID)
	if err != nil {
		return nil, err
	}
	return &grantsResponse{
		DefaultEffect: s.defaultEffectForAgent(installID, instanceID),
		Grants:        rules,
	}, nil
}

// loadInstallManifest pulls the parsed manifest JSON persisted on the
// apps row at install time. Returns ErrNoManifest when the install
// hasn't shipped a manifest_json yet (legacy rows pre-permissions feature).
func (s *Server) loadInstallManifest(installID int64) (*sdk.Manifest, error) {
	var raw string
	err := s.store.db.QueryRow(
		`SELECT a.manifest_json FROM app_installs i JOIN apps a ON a.id = i.app_id WHERE i.id = ?`,
		installID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, errors.New("install not found")
	}
	if err != nil {
		return nil, err
	}
	if raw == "" || raw == "null" {
		// No catalog declared. Return an empty manifest so the dashboard
		// can render "no scoped permissions yet" instead of 500ing.
		return &sdk.Manifest{}, nil
	}
	var m sdk.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("decode manifest_json: %w", err)
	}
	return &m, nil
}

// validateGrantBody enforces that the rule's permission exists in the
// app's catalog and the effect is one of allow|deny. Resource patterns
// are not validated against matchers here — the SDK matches at call
// time and ignores nonsense patterns rather than failing the install.
func validateGrantBody(m *sdk.Manifest, effect, permission string) error {
	if effect != "allow" && effect != "deny" {
		return errors.New("effect must be allow|deny")
	}
	if permission == "" {
		return errors.New("permission required")
	}
	if permission == "*" {
		return nil
	}
	for _, p := range m.Provides.ProvidedPermissions {
		if p.Name == permission {
			return nil
		}
	}
	// Permissive validation: when the manifest hasn't declared any
	// permissions yet (e.g. an early-adopter install racing with a
	// catalog ship), accept the rule rather than blocking. The SDK
	// gate is the source of truth at call time.
	if len(m.Provides.ProvidedPermissions) == 0 {
		return nil
	}
	return fmt.Errorf("permission %q not declared by this app", permission)
}

// buildCallerFromGrants constructs an sdk.Caller from a stored policy
// for evaluate/dry-run usage server-side. The dashboard's Test panel
// uses this so we get the same matcher semantics as the SDK gate.
func buildCallerFromGrants(m *sdk.Manifest, resp *grantsResponse) *sdk.Caller {
	c := &sdk.Caller{DefaultEffect: resp.DefaultEffect}
	if m != nil {
		c.Resources = m.Provides.Resources
	}
	for _, g := range resp.Grants {
		c.Grants = append(c.Grants, sdk.Grant{
			Effect: g.Effect, Permission: g.Permission, Resource: g.Resource,
		})
	}
	return c
}

// getUserName returns a stable identifier for the actor making a write
// — used as `created_by` on grant rows. Falls back to "" when the
// auth middleware didn't stamp a user header.
func getUserName(r *http.Request) string {
	if v := r.Header.Get("X-Apteva-User"); v != "" {
		return v
	}
	if id := getUserID(r); id > 0 {
		return strconv.FormatInt(id, 10)
	}
	return ""
}
