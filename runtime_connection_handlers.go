package main

// runtime_connection_handlers.go — the two write endpoints the Models
// settings tab needs once providers stop being their own table.
//
//	PATCH /api/connections/:id/primary        which key backs the runtime
//	PATCH /api/connections/:id/runtime-config which models it runs
//
// Both are connection-scoped equivalents of things the providers table
// used to own (implicit lowest-id dedup, and model_* fields inside the
// encrypted blob).

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SetConnectionPrimary makes one connection the runtime pick for its
// app within its scope, demoting any sibling.
//
// Demotion and promotion run in one transaction because the partial
// unique index on (user_id, project_id, app_slug) WHERE is_primary = 1
// would reject the promotion outright if the old primary were still
// standing. Doing it in two statements outside a transaction would leave
// a window with no primary at all — an agent booting in that window
// would fall back to lowest-id and could pick up the wrong credential.
func (s *Store) SetConnectionPrimary(userID, connID int64) error {
	var projectID, appSlug string
	err := s.db.QueryRow(
		`SELECT COALESCE(project_id,''), app_slug FROM connections WHERE id = ? AND user_id = ?`,
		connID, userID,
	).Scan(&projectID, &appSlug)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE connections SET is_primary = 0
		 WHERE user_id = ? AND COALESCE(project_id,'') = ? AND app_slug = ? AND id != ?`,
		userID, projectID, appSlug, connID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE connections SET is_primary = 1 WHERE id = ? AND user_id = ?`,
		connID, userID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// handleSetConnectionPrimary — PATCH /api/connections/:id/primary
//
// Only meaningful for connections whose catalog app declares a runtime
// block; for anything else there is no pool to be primary in, so the
// request is refused rather than silently recorded.
func (s *Server) handleSetConnectionPrimary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	connID, ok := connectionIDFromPath(r.URL.Path, "/primary")
	if !ok {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}

	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Runtime == nil {
		http.Error(w, "connection is not a runtime backend", http.StatusBadRequest)
		return
	}
	if err := s.store.SetConnectionPrimary(userID, connID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id":           connID,
		"is_primary":   true,
		"app_slug":     conn.AppSlug,
		"provider_key": app.Runtime.ProviderKey,
	})
}

// handleConnectionRuntimeConfig — GET/PATCH
// /api/connections/:id/runtime-config
//
// Holds model picks and non-secret knobs. PATCH merges rather than
// replaces so the dashboard can set one tier without having to send back
// the model_capabilities blob the Codex hydration wrote.
func (s *Server) handleConnectionRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	connID, ok := connectionIDFromPath(r.URL.Path, "/runtime-config")
	if !ok {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	current, err := s.store.GetConnectionRuntimeConfig(userID, connID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, current)
	case http.MethodPatch:
		app := s.catalog.Get(conn.AppSlug)
		if app == nil || app.Runtime == nil {
			http.Error(w, "connection is not a runtime backend", http.StatusBadRequest)
			return
		}
		var patch map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		for key, value := range patch {
			// A null clears the key, which is how the dashboard says
			// "back to the provider default" without inventing a
			// sentinel string that could collide with a real model ID.
			if value == nil {
				delete(current, key)
				continue
			}
			current[key] = value
		}
		encoded, err := json.Marshal(current)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.UpdateConnectionRuntimeConfig(connID, string(encoded)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, current)
	default:
		http.Error(w, "GET or PATCH", http.StatusMethodNotAllowed)
	}
}

// handleConnectionModels — GET /api/connections/:id/models[?refresh=1]
//
// Live model list for a runtime connection, so the Models and Helper
// tabs can offer a picker instead of a free-text box.
//
// The providers path found its API key by scanning the blob for a
// *_API_KEY suffix. Here the catalog says which env var carries the key,
// so we render runtime.env and read it — no guessing, and a provider
// whose key doesn't end in _API_KEY still works.
func (s *Server) handleConnectionModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	connID, ok := connectionIDFromPath(r.URL.Path, "/models")
	if !ok {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}
	conn, encrypted, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Runtime == nil {
		http.Error(w, "connection is not a runtime backend", http.StatusBadRequest)
		return
	}

	runtimeConn := runtimeConnection{
		ID: conn.ID, AppSlug: conn.AppSlug, Name: conn.Name,
		ProjectID: conn.ProjectID, Status: conn.Status,
		EncryptedCreds: encrypted,
	}
	if config, err := s.store.GetConnectionRuntimeConfig(userID, connID); err == nil {
		if encoded, marshalErr := json.Marshal(config); marshalErr == nil {
			runtimeConn.RuntimeConfig = string(encoded)
		}
	}
	src, err := buildRuntimeSources(runtimeConn, s.secret)
	if err != nil {
		http.Error(w, "could not read connection credentials", http.StatusInternalServerError)
		return
	}

	if app.Runtime.ProviderKey == openAICodexAuthProvider {
		accessToken, _ := lookupRuntimeRef("credentials.access_token", src)
		accountID, _ := lookupRuntimeRef("credentials.account_id", src)
		models, fetchErr := fetchCodexModelCatalog(r.Context(), accessToken, accountID,
			r.URL.Query().Get("refresh") == "1")
		if fetchErr != nil {
			http.Error(w, "failed to fetch models: "+fetchErr.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, models)
		return
	}

	env := renderRuntimeEnv(app.Runtime, src)
	apiKey := ""
	for _, name := range sortedEnvNames(app.Runtime.Env) {
		if strings.HasSuffix(name, "_KEY") && env[name] != "" {
			apiKey = env[name]
			break
		}
	}
	if apiKey == "" {
		http.Error(w, "connection has no API key to list models with", http.StatusBadRequest)
		return
	}
	models, err := FetchModels(app.Runtime.ProviderKey, apiKey, runtimeBaseURLFor(src))
	if err != nil {
		http.Error(w, "failed to fetch models: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, models)
}

// handleConnectionUsage — GET /api/connections/:id/usage[?refresh=1]
//
// Subscription quota for a runtime connection. Unlike
// integration_usage_events (recorded calls), this is a live poll of the
// upstream, so it reuses the same fetcher + cache the providers path
// used rather than reading anything from our DB.
func (s *Server) handleConnectionUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	connID, ok := connectionIDFromPath(r.URL.Path, "/usage")
	if !ok {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}
	conn, encrypted, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Runtime == nil {
		writeJSON(w, ProviderUsageSnapshot{Supported: false, ProviderID: connID})
		return
	}
	fetcher := providerUsageFetcherFor(app.Runtime.ProviderKey)
	if fetcher == nil {
		writeJSON(w, ProviderUsageSnapshot{Supported: false, ProviderID: connID})
		return
	}

	plaintext, err := Decrypt(s.secret, encrypted)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &credentials); err != nil {
		http.Error(w, "invalid connection credentials", http.StatusInternalServerError)
		return
	}

	// The fetcher reads the providers-era nested shape
	// (state.credentials.access_token); connections store their
	// credentials flat. Reshaping here keeps one fetcher and one cache
	// for both paths rather than forking the upstream call.
	state := map[string]any{
		"credentials": map[string]any{
			"access_token": credentials["access_token"],
			"account_id":   credentials["account_id"],
			"expires_at":   credentials["token_expires_at"],
		},
	}
	snapshot, err := s.fetchConnectionUsage(r, conn, state, fetcher, credentials)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, snapshot)
}

// fetchConnectionUsage mirrors fetchProviderUsage's cache-then-refresh
// shape, but refreshes through the connection's own credential path.
func (s *Server) fetchConnectionUsage(
	r *http.Request,
	conn *Connection,
	state map[string]any,
	fetcher providerUsageFetcher,
	credentials map[string]string,
) (*ProviderUsageSnapshot, error) {
	force := r.URL.Query().Get("refresh") == "1"
	key := fetcher.CacheKey(state)
	if entry, ok := globalProviderUsageCache.entry(key); ok {
		age := time.Since(entry.fetched)
		if (!force && age < providerUsageFreshTTL) || (force && age < providerUsageManualRefreshMin) {
			snapshot := entry.snapshot
			snapshot.ProviderID = conn.ID
			return &snapshot, nil
		}
	}

	lock := globalProviderUsageCache.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	// Re-check under the lock. Without this, N concurrent readers all miss
	// the first check, queue on the mutex, and then each fires its own
	// upstream request — turning one page load with several provider cards
	// into N calls against a rate-limited quota endpoint.
	if entry, ok := globalProviderUsageCache.entry(key); ok {
		age := time.Since(entry.fetched)
		if (!force && age < providerUsageFreshTTL) || (force && age < providerUsageManualRefreshMin) {
			snapshot := entry.snapshot
			snapshot.ProviderID = conn.ID
			return &snapshot, nil
		}
	}

	staleEntry, hasStale := globalProviderUsageCache.entry(key)
	if connectionOpenAICodexNeedsRefresh(credentials, 10*time.Minute) {
		if err := refreshIntegrationOpenAICodexCredentials(credentials); err != nil {
			return providerUsageStaleOrError(conn.ID, staleEntry, hasStale, err)
		}
		if encoded, err := json.Marshal(credentials); err == nil {
			if reEncrypted, encErr := Encrypt(s.secret, string(encoded)); encErr == nil {
				_ = s.store.UpdateConnectionCredentials(conn.ID, reEncrypted)
			}
		}
		state["credentials"] = map[string]any{
			"access_token": credentials["access_token"],
			"account_id":   credentials["account_id"],
			"expires_at":   credentials["token_expires_at"],
		}
	}

	snapshot, err := fetcher.FetchUsage(r.Context(), state)
	if err != nil {
		return providerUsageStaleOrError(conn.ID, staleEntry, hasStale, err)
	}
	now := time.Now().UTC()
	snapshot.ProviderID = conn.ID
	snapshot.FetchedAt = now
	snapshot.Stale = false
	globalProviderUsageCache.put(fetcher.CacheKey(state), providerUsageCacheEntry{snapshot: *snapshot, fetched: now})
	return snapshot, nil
}

// GetConnectionRuntimeConfig returns the decoded runtime_config, or an
// empty map when unset or unparseable.
func (s *Store) GetConnectionRuntimeConfig(userID, connID int64) (map[string]any, error) {
	var raw string
	err := s.db.QueryRow(
		`SELECT COALESCE(runtime_config,'{}') FROM connections WHERE id = ? AND user_id = ?`,
		connID, userID,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	config := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &config)
	}
	return config, nil
}

// connectionIDFromPath pulls the numeric id out of
// /connections/<id><suffix>.
func connectionIDFromPath(path, suffix string) (int64, bool) {
	trimmed := strings.TrimPrefix(path, "/connections/")
	trimmed = strings.TrimSuffix(trimmed, suffix)
	id, err := atoi64(strings.Trim(trimmed, "/"))
	if err != nil {
		return 0, false
	}
	return id, true
}

// runtimeConnectionSummary describes one runtime-backed connection for
// the Models settings tab: enough to render the row, choose a primary,
// and pick models — without ever shipping a credential.
type runtimeConnectionSummary struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	AppSlug      string         `json:"app_slug"`
	AppName      string         `json:"app_name"`
	AuthType     string         `json:"auth_type"`
	ProviderKey  string         `json:"provider_key"`
	Role         string         `json:"role"`
	ProjectID    string         `json:"project_id"`
	Scope        string         `json:"scope"`
	IsPrimary    bool           `json:"is_primary"`
	Capabilities []string       `json:"capabilities,omitempty"`
	RuntimeConf  map[string]any `json:"runtime_config"`
	EnvVars      []string       `json:"env_vars,omitempty"`
}

// handleListRuntimeConnections — GET /api/connections/runtime[?project_id=]
//
// The Models tab's list. Mirrors what GetProviderPool would resolve, so
// the operator sees the same precedence the agents will get.
func (s *Server) handleListRuntimeConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")

	var conns []runtimeConnection
	var err error
	if projectID != "" {
		conns, err = s.store.ListRuntimeConnections(userID, projectID)
	} else {
		conns, err = s.store.ListRuntimeConnections(userID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Same group ordering as the agent pool (see runtimeGroupOrder): the
	// dashboard falls back to its first row exactly where the server
	// falls back to pool[0], so the two must agree on what "first" is —
	// otherwise the UI shows one provider as the effective default while
	// agents boot with another.
	order := runtimeGroupOrder(s, conns)
	sort.SliceStable(conns, func(i, j int) bool {
		appI, appJ := s.runtimeAppFor(conns[i]), s.runtimeAppFor(conns[j])
		if appI == nil || appJ == nil {
			return appJ == nil && appI != nil
		}
		return order[appI.Runtime.ProviderKey] < order[appJ.Runtime.ProviderKey]
	})

	out := []runtimeConnectionSummary{}
	for _, conn := range conns {
		app := s.runtimeAppFor(conn)
		if app == nil {
			continue
		}
		config := map[string]any{}
		if raw := strings.TrimSpace(conn.RuntimeConfig); raw != "" {
			_ = json.Unmarshal([]byte(raw), &config)
		}
		scope := "global"
		if conn.ProjectID != "" {
			scope = "project"
		}
		out = append(out, runtimeConnectionSummary{
			ID: conn.ID, Name: conn.Name,
			AppSlug: conn.AppSlug, AppName: app.Name, AuthType: conn.AuthType,
			ProviderKey: app.Runtime.ProviderKey, Role: app.Runtime.Role,
			ProjectID: conn.ProjectID, Scope: scope,
			IsPrimary:    conn.IsPrimary,
			Capabilities: app.Runtime.Capabilities,
			RuntimeConf:  config,
			EnvVars:      sortedEnvNames(app.Runtime.Env),
		})
	}
	writeJSON(w, out)
}
