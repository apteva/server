package main

// HTTP handlers for credential-group ("suite") management. Mounted
// from main.go under /integrations/groups/*. The UI calls these three
// endpoints to drive the account-key flow:
//
//   POST   /integrations/groups/{id}/master                 — add or replace key, run discovery
//   POST   /integrations/groups/{id}/master/refresh         — re-run discovery, update cache
//   POST   /integrations/groups/{id}/master/enable          — fan out to (app, project) pairs
//   GET    /integrations/groups/{id}/master                 — fetch current master + cached projects
//   DELETE /integrations/groups/{id}/master                 — cascade-delete master + children

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// extractGroupID pulls the credential-group id out of a request path
// of the form /integrations/groups/{id}(/subpath). Returns "" if the
// URL doesn't match.
func extractGroupID(path, suffix string) string {
	rest := strings.TrimPrefix(path, "/integrations/groups/")
	if suffix != "" {
		rest = strings.TrimSuffix(rest, "/"+suffix)
		rest = strings.TrimSuffix(rest, suffix)
	}
	rest = strings.Trim(rest, "/")
	// When a caller includes a trailing /something we didn't match on,
	// fall back to the first segment.
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// findMasterConnection returns the master connection row for (user,
// project, group) when one exists. Returns (nil, nil) when there is no
// master yet — not an error.
func (s *Server) findMasterConnection(userID int64, projectID, groupID string) (*Connection, string, error) {
	rows, err := s.store.ListConnections(userID, projectID)
	if err != nil {
		return nil, "", err
	}
	masterSlug := MasterSlug(groupID)
	for i := range rows {
		if rows[i].AppSlug == masterSlug {
			_, enc, err := s.store.GetConnection(userID, rows[i].ID)
			if err != nil {
				return nil, "", err
			}
			return &rows[i], enc, nil
		}
	}
	return nil, "", nil
}

// Helper: any app in the catalog that belongs to the group — we need
// one to borrow the base_url + auth_headers for discovery calls.
func (s *Server) anyGroupMember(groupID string) *AppTemplate {
	g := s.catalog.GetGroup(groupID)
	if g == nil || len(g.Members) == 0 {
		return nil
	}
	return s.catalog.Get(g.Members[0])
}

// POST /integrations/groups/{id}/master
// Body: { "credentials": { "api_key": "..." }, "project_id": "optional-apteva-project", "name": "..." }
// Runs discovery, creates or updates the master row, stores the cache
// of external projects, and returns the master id + the project list.
func (s *Server) handleCreateGroupMaster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	groupID := extractGroupID(r.URL.Path, "master")
	if groupID == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	g := s.catalog.GetGroup(groupID)
	if g == nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	app := s.anyGroupMember(groupID)
	if app == nil {
		http.Error(w, "group has no members", http.StatusInternalServerError)
		return
	}

	var body struct {
		Credentials map[string]string `json:"credentials"`
		ProjectID   string            `json:"project_id"`
		Name        string            `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Credentials == nil {
		body.Credentials = map[string]string{}
	}

	// Run discovery — this both validates the credential and caches
	// the project list for the matrix UI.
	projects, err := discoverProjects(app, &g.Meta, body.Credentials)
	if err != nil {
		// Surface the upstream error as 400 since it's almost always a
		// bad key or a network issue the user can fix.
		http.Error(w, fmt.Sprintf("discovery failed: %v", err), http.StatusBadRequest)
		return
	}

	// Compose the master-row credential blob. Reserved keys describe
	// the suite membership; the real credential fields (api_key etc.)
	// are mixed in under their normal names so the existing header
	// substitution works when the master is later referenced.
	blob := map[string]string{
		credKeyType:  "master",
		credKeyGroup: groupID,
		credKeyScope: "account",
	}
	for k, v := range body.Credentials {
		blob[k] = v
	}
	cacheBytes, _ := json.Marshal(projects)
	blob[credKeyProjectsCache] = string(cacheBytes)

	encoded, err := json.Marshal(blob)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	enc, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Upsert: if a master row already exists for (user, project,
	// group), update its blob. Otherwise insert a new row.
	existing, _, err := s.findMasterConnection(userID, body.ProjectID, groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var masterID int64
	if existing != nil {
		if err := s.store.UpdateConnectionCredentials(existing.ID, enc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		masterID = existing.ID
	} else {
		name := body.Name
		if name == "" {
			name = g.Meta.Name + " master"
		}
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:               userID,
			AppSlug:              MasterSlug(groupID),
			AppName:              g.Meta.Name,
			Name:                 name,
			AuthType:             "api_key",
			EncryptedCreds: enc,
			Status:               "active",
			Source:               "local",
			ProjectID:            body.ProjectID,
		})
		if err != nil {
			log.Printf("[SUITE] master create failed user=%d group=%s project=%s: %v", userID, groupID, body.ProjectID, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		masterID = conn.ID
	}

	writeJSON(w, map[string]any{
		"master_id": masterID,
		"projects":  projects,
	})
}

// GET /integrations/groups/{id}/master?project_id=...
// Returns the current master's id + cached projects + a list of
// already-enabled (app_slug, external_project_id) pairs, so the UI
// can render the matrix pre-filled.
func (s *Server) handleGetGroupMaster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	groupID := extractGroupID(r.URL.Path, "master")
	if groupID == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	master, encCreds, err := s.findMasterConnection(userID, projectID, groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if master == nil {
		writeJSON(w, map[string]any{"master": nil})
		return
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var blob map[string]string
	json.Unmarshal([]byte(plain), &blob)

	var projects []CachedProject
	if c := blob[credKeyProjectsCache]; c != "" {
		_ = json.Unmarshal([]byte(c), &projects)
	}
	// Masked key preview ("okt_acc_….fb21") — just last 4 of any cred
	// value that looks like a key. Good enough for the manage-screen.
	mask := func(s string) string {
		if len(s) <= 6 {
			return strings.Repeat("•", len(s))
		}
		return s[:4] + strings.Repeat("•", 4) + s[len(s)-4:]
	}
	masked := map[string]string{}
	for k, v := range blob {
		if strings.HasPrefix(k, "_") {
			continue
		}
		masked[k] = mask(v)
	}

	// Enumerate children so the matrix can show existing enablements.
	children := []map[string]any{}
	allConns, _ := s.store.ListConnections(userID, projectID)
	for _, c := range allConns {
		if c.AppSlug == MasterSlug(groupID) || IsMasterSlug(c.AppSlug) {
			continue
		}
		// Fast filter: only load + parse connections whose app is a
		// member of this group.
		isMember := false
		g := s.catalog.GetGroup(groupID)
		if g != nil {
			for _, m := range g.Members {
				if m == c.AppSlug {
					isMember = true
					break
				}
			}
		}
		if !isMember {
			continue
		}
		_, encChild, err := s.store.GetConnection(userID, c.ID)
		if err != nil {
			continue
		}
		cp, err := Decrypt(s.secret, encChild)
		if err != nil {
			continue
		}
		var cb map[string]string
		json.Unmarshal([]byte(cp), &cb)
		if cb[credKeyType] != "child" {
			continue
		}
		if cb[credKeyMasterID] != strconv.FormatInt(master.ID, 10) {
			continue
		}
		children = append(children, map[string]any{
			"id":         c.ID,
			"app_slug":   c.AppSlug,
			"name":       c.Name,
			"project_id": cb[credKeyProjectID],
		})
	}

	writeJSON(w, map[string]any{
		"master": map[string]any{
			"id":         master.ID,
			"project_id": master.ProjectID,
			"name":       master.Name,
			"credentials_masked": masked,
		},
		"projects": projects,
		"children": children,
	})
}

// POST /integrations/groups/{id}/master/refresh
// Re-runs discovery for an existing master and updates its cache.
func (s *Server) handleRefreshGroupMaster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	// path: /integrations/groups/{id}/master/refresh
	groupID := extractGroupID(strings.TrimSuffix(r.URL.Path, "/refresh"), "master")
	if groupID == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	var body struct {
		ProjectID string `json:"project_id"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	master, encCreds, err := s.findMasterConnection(userID, body.ProjectID, groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if master == nil {
		http.Error(w, "no master for this group", http.StatusNotFound)
		return
	}
	g := s.catalog.GetGroup(groupID)
	app := s.anyGroupMember(groupID)
	if g == nil || app == nil {
		http.Error(w, "group missing from catalog", http.StatusNotFound)
		return
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var blob map[string]string
	json.Unmarshal([]byte(plain), &blob)

	real := stripReservedCreds(blob)
	projects, err := discoverProjects(app, &g.Meta, real)
	if err != nil {
		http.Error(w, fmt.Sprintf("discovery failed: %v", err), http.StatusBadGateway)
		return
	}
	cacheBytes, _ := json.Marshal(projects)
	blob[credKeyProjectsCache] = string(cacheBytes)
	encoded, _ := json.Marshal(blob)
	enc, _ := Encrypt(s.secret, string(encoded))
	if err := s.store.UpdateConnectionCredentials(master.ID, enc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"projects": projects})
}

// POST /integrations/groups/{id}/master/enable
// Body: { "project_id": "apteva-project",
//         "selections": [{"app_slug":"omnikit-storage","external_project_id":"proj_abc","label":"marketing-prod"}, ...],
//         "replace": false  // if true, remove child rows that aren't in the new selection
//       }
// Idempotent: re-calling with the same selections is a no-op.
func (s *Server) handleEnableGroupApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	groupID := extractGroupID(strings.TrimSuffix(r.URL.Path, "/enable"), "master")
	if groupID == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	g := s.catalog.GetGroup(groupID)
	if g == nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	var body struct {
		ProjectID  string `json:"project_id"`
		Selections []struct {
			AppSlug           string `json:"app_slug"`
			ExternalProjectID string `json:"external_project_id"`
			Label             string `json:"label"`
		} `json:"selections"`
		Replace bool `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	master, _, err := s.findMasterConnection(userID, body.ProjectID, groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if master == nil {
		http.Error(w, "no master for this group — add key first", http.StatusBadRequest)
		return
	}

	// Validate every slug belongs to this group.
	members := map[string]bool{}
	for _, m := range g.Members {
		members[m] = true
	}
	for _, sel := range body.Selections {
		if !members[sel.AppSlug] {
			http.Error(w, fmt.Sprintf("%q is not a member of group %q", sel.AppSlug, groupID), http.StatusBadRequest)
			return
		}
		if sel.ExternalProjectID == "" {
			http.Error(w, "external_project_id required on each selection", http.StatusBadRequest)
			return
		}
	}

	// Enumerate current children of this master so we can skip already
	// existing (app, project) pairs and optionally prune missing ones.
	existing := map[string]int64{} // key: "app_slug|external_project_id"
	allConns, _ := s.store.ListConnections(userID, body.ProjectID)
	for _, c := range allConns {
		if !members[c.AppSlug] {
			continue
		}
		_, encChild, err := s.store.GetConnection(userID, c.ID)
		if err != nil {
			continue
		}
		cp, err := Decrypt(s.secret, encChild)
		if err != nil {
			continue
		}
		var cb map[string]string
		json.Unmarshal([]byte(cp), &cb)
		if cb[credKeyType] != "child" {
			continue
		}
		if cb[credKeyMasterID] != strconv.FormatInt(master.ID, 10) {
			continue
		}
		key := c.AppSlug + "|" + cb[credKeyProjectID]
		existing[key] = c.ID
	}

	// Create missing child rows.
	created := []map[string]any{}
	wantKeys := map[string]bool{}
	for _, sel := range body.Selections {
		key := sel.AppSlug + "|" + sel.ExternalProjectID
		wantKeys[key] = true
		if _, ok := existing[key]; ok {
			continue
		}
		app := s.catalog.Get(sel.AppSlug)
		if app == nil {
			continue
		}
		blob := map[string]string{
			credKeyType:      "child",
			credKeyMasterID:  strconv.FormatInt(master.ID, 10),
			credKeyProjectID: sel.ExternalProjectID,
		}
		encoded, _ := json.Marshal(blob)
		enc, err := Encrypt(s.secret, string(encoded))
		if err != nil {
			continue
		}
		// Connection name encodes the project label so the
		// (user, project, slug, name) uniqueness constraint holds.
		connName := sel.Label
		if connName == "" {
			connName = sel.ExternalProjectID
		}
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:               userID,
			AppSlug:              sel.AppSlug,
			AppName:              app.Name,
			Name:                 connName,
			AuthType:             "api_key",
			EncryptedCreds: enc,
			Status:               "active",
			Source:               "local",
			ProjectID:            body.ProjectID,
		})
		if err != nil {
			log.Printf("[SUITE] child create failed user=%d slug=%s project=%s ext=%s: %v", userID, sel.AppSlug, body.ProjectID, sel.ExternalProjectID, err)
			continue
		}
		// Auto-create the MCP server row for this child so the
		// connection is actually reachable as a tool surface —
		// mirrors what handleCreateConnection does on the legacy
		// path. Without this, the connection exists but no MCP is
		// registered, so the agent sees nothing.
		//
		// MCP slug encodes BOTH the service and the external project
		// so fan-outs produce readable, distinct prefixes:
		//   omnikit-storage + Real Estate → omnikit-storage-real-estate
		//   omnikit-storage + AppForge    → omnikit-storage-appforge
		// Agents see tool calls like `omnikit-storage-real-estate_list`
		// and can tell at a glance which project is being hit.
		slugBase := sel.AppSlug + "-" + connName // connName is the project label
		if mcpID, merr := s.store.CreateMCPServerFromConnectionWithSlug(userID, conn, len(app.Tools), slugBase); merr != nil {
			log.Printf("[SUITE] child auto-mcp FAILED conn=%d (%s/%s): %v", conn.ID, conn.AppSlug, conn.Name, merr)
		} else {
			log.Printf("[SUITE] child auto-mcp mcp_id=%d conn=%d slug=%s tools=%d", mcpID, conn.ID, conn.AppSlug, len(app.Tools))
		}
		created = append(created, map[string]any{
			"id": conn.ID, "app_slug": conn.AppSlug, "project_id": sel.ExternalProjectID, "name": conn.Name,
		})
	}

	// Optional prune: delete children that exist but weren't in the
	// new selection. Opt-in so the default POST doesn't surprise users
	// who only want to add things.
	removed := []int64{}
	if body.Replace {
		for key, id := range existing {
			if !wantKeys[key] {
				if err := s.store.DeleteConnection(userID, id); err == nil {
					removed = append(removed, id)
				}
			}
		}
	}

	writeJSON(w, map[string]any{
		"created":        created,
		"already_exists": len(existing),
		"removed":        removed,
	})
}

// DELETE /integrations/groups/{id}/master?project_id=...
// Removes master + every child row pointing at it. The dashboard
// calls this from the "Disconnect all" button.
func (s *Server) handleDeleteGroupMaster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	groupID := extractGroupID(r.URL.Path, "master")
	if groupID == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	master, _, err := s.findMasterConnection(userID, projectID, groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if master == nil {
		writeJSON(w, map[string]any{"removed": 0})
		return
	}
	g := s.catalog.GetGroup(groupID)
	members := map[string]bool{}
	if g != nil {
		for _, m := range g.Members {
			members[m] = true
		}
	}
	all, _ := s.store.ListConnections(userID, projectID)
	masterIDStr := strconv.FormatInt(master.ID, 10)
	removed := 0
	for _, c := range all {
		if !members[c.AppSlug] {
			continue
		}
		_, encChild, err := s.store.GetConnection(userID, c.ID)
		if err != nil {
			continue
		}
		cp, err := Decrypt(s.secret, encChild)
		if err != nil {
			continue
		}
		var cb map[string]string
		json.Unmarshal([]byte(cp), &cb)
		if cb[credKeyType] != "child" || cb[credKeyMasterID] != masterIDStr {
			continue
		}
		if err := s.store.DeleteConnection(userID, c.ID); err == nil {
			removed++
		}
	}
	s.store.DeleteConnection(userID, master.ID)
	writeJSON(w, map[string]any{"removed": removed + 1})
}
