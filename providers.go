package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ProviderType struct {
	ID                  int64    `json:"id"`
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Fields              []string `json:"fields"`
	RequiresCredentials bool     `json:"requires_credentials"`
	SortOrder           int      `json:"sort_order"`
}

type Provider struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	ProviderTypeID int64     `json:"provider_type_id"`
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	ProjectID      string    `json:"project_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// --- Store methods ---

func (s *Store) ListProviderTypes() ([]ProviderType, error) {
	rows, err := s.db.Query("SELECT id, type, name, description, fields, requires_credentials, sort_order FROM provider_types ORDER BY sort_order")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []ProviderType
	for rows.Next() {
		var pt ProviderType
		var fieldsJSON string
		var reqCreds int
		rows.Scan(&pt.ID, &pt.Type, &pt.Name, &pt.Description, &fieldsJSON, &reqCreds, &pt.SortOrder)
		json.Unmarshal([]byte(fieldsJSON), &pt.Fields)
		pt.RequiresCredentials = reqCreds == 1
		types = append(types, pt)
	}
	return types, nil
}

// CreateProvider stores a new provider for a user. If projectID is provided
// and non-empty, the provider is scoped to that project; otherwise it is
// "unscoped" (project_id='') and visible across all projects.
func (s *Store) CreateProvider(userID, providerTypeID int64, ptype, name, encryptedData string, projectID ...string) (*Provider, error) {
	pid := ""
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	result, err := s.db.Exec(
		"INSERT INTO providers (user_id, provider_type_id, type, name, encrypted_data, project_id) VALUES (?, ?, ?, ?, ?, ?)",
		userID, providerTypeID, ptype, name, encryptedData, pid,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Provider{ID: id, UserID: userID, ProviderTypeID: providerTypeID, Type: ptype, Name: name, Status: "active", ProjectID: pid, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

// ListProviders returns all providers for a user. If projectID is provided
// and non-empty, the result includes both providers scoped to that project
// AND unscoped (project_id='') providers — the latter act as "global" so
// existing providers stay visible after this per-project feature rolls out.
func (s *Store) ListProviders(userID int64, projectID ...string) ([]Provider, error) {
	const cols = `id, provider_type_id, type, name, COALESCE(status,'active'), COALESCE(project_id,''), created_at, updated_at`
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			`SELECT `+cols+` FROM providers WHERE user_id = ? AND (project_id = ? OR project_id = '')`,
			userID, projectID[0],
		)
	} else {
		rows, err = s.db.Query(
			`SELECT `+cols+` FROM providers WHERE user_id = ?`, userID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		var createdAt, updatedAt string
		rows.Scan(&p.ID, &p.ProviderTypeID, &p.Type, &p.Name, &p.Status, &p.ProjectID, &createdAt, &updatedAt)
		p.UserID = userID
		p.CreatedAt, _ = parseTime(createdAt)
		p.UpdatedAt, _ = parseTime(updatedAt)
		providers = append(providers, p)
	}
	return providers, nil
}

func (s *Store) GetProvider(userID, providerID int64) (*Provider, string, error) {
	var p Provider
	var encryptedData, createdAt, updatedAt string
	err := s.db.QueryRow(
		"SELECT id, type, name, encrypted_data, created_at, updated_at FROM providers WHERE id = ? AND user_id = ?",
		providerID, userID,
	).Scan(&p.ID, &p.Type, &p.Name, &encryptedData, &createdAt, &updatedAt)
	if err != nil {
		return nil, "", err
	}
	p.UserID = userID
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updatedAt)
	return &p, encryptedData, nil
}

func (s *Store) UpdateProvider(userID, providerID int64, ptype, name, encryptedData string) error {
	_, err := s.db.Exec(
		"UPDATE providers SET type=?, name=?, encrypted_data=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?",
		ptype, name, encryptedData, providerID, userID,
	)
	return err
}

func (s *Store) DeleteProvider(userID, providerID int64) error {
	_, err := s.db.Exec("DELETE FROM providers WHERE id = ? AND user_id = ?", providerID, userID)
	return err
}

// FindProviderByWebhookToken looks up a provider row by its webhook
// token. Used by the unified /webhooks/:token ingress handler to
// dispatch provider-backed trigger deliveries (Composio today, any
// other trigger backend tomorrow) alongside per-subscription
// deliveries. Returns the row + encrypted blob.
func (s *Store) FindProviderByWebhookToken(token string) (*Provider, string, error) {
	if token == "" {
		return nil, "", sql.ErrNoRows
	}
	var p Provider
	var encryptedData, createdAt, updatedAt string
	err := s.db.QueryRow(`
		SELECT id, user_id, type, name, encrypted_data, COALESCE(project_id,''), created_at, updated_at
		FROM providers
		WHERE webhook_token = ?
		LIMIT 1
	`, token).Scan(&p.ID, &p.UserID, &p.Type, &p.Name, &encryptedData, &p.ProjectID, &createdAt, &updatedAt)
	if err != nil {
		return nil, "", err
	}
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updatedAt)
	return &p, encryptedData, nil
}

// SetProviderWebhookToken persists a webhook_token for a provider row.
// Idempotent: safe to call repeatedly with the same token.
func (s *Store) SetProviderWebhookToken(providerID int64, token string) error {
	_, err := s.db.Exec("UPDATE providers SET webhook_token = ? WHERE id = ?", token, providerID)
	return err
}

// FindComposioProviderForProject returns the Composio provider row that
// owns the given (user, project) pair. Used by the webhook ingress path
// to locate the signing secret and by the subscription create path to
// bootstrap a per-project webhook subscription on first use.
//
// Pass userID=0 for the webhook ingress path, which knows only the
// project id from the URL. We look up by project alone in that case
// and the caller uses the resolved row's user_id for downstream
// subscription lookups.
//
// Precedence: a project-scoped row wins over a global (project_id='')
// row of the same type — matches how ListProviders surfaces both.
func (s *Store) FindComposioProviderForProject(userID int64, projectID string) (*Provider, string, error) {
	var p Provider
	var encryptedData, createdAt, updatedAt string
	var err error
	if userID > 0 {
		err = s.db.QueryRow(`
			SELECT id, user_id, type, name, encrypted_data, COALESCE(project_id,''), created_at, updated_at
			FROM providers
			WHERE user_id = ? AND type = 'integrations' AND name = 'Composio'
			  AND (project_id = ? OR project_id = '')
			ORDER BY CASE WHEN project_id = ? THEN 0 ELSE 1 END, id DESC
			LIMIT 1
		`, userID, projectID, projectID).Scan(&p.ID, &p.UserID, &p.Type, &p.Name, &encryptedData, &p.ProjectID, &createdAt, &updatedAt)
	} else {
		err = s.db.QueryRow(`
			SELECT id, user_id, type, name, encrypted_data, COALESCE(project_id,''), created_at, updated_at
			FROM providers
			WHERE type = 'integrations' AND name = 'Composio'
			  AND (project_id = ? OR project_id = '')
			ORDER BY CASE WHEN project_id = ? THEN 0 ELSE 1 END, id DESC
			LIMIT 1
		`, projectID, projectID).Scan(&p.ID, &p.UserID, &p.Type, &p.Name, &encryptedData, &p.ProjectID, &createdAt, &updatedAt)
	}
	if err != nil {
		return nil, "", err
	}
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updatedAt)
	return &p, encryptedData, nil
}

// GetAllProviderEnvVars decrypts all providers for a user and returns env vars
// (UPPER_CASE keys). If projectID is provided and non-empty, only providers
// scoped to that project (or unscoped globals) are included — matching the
// visibility rules in ListProviders.
func (s *Store) GetAllProviderEnvVars(userID int64, secret []byte, projectID ...string) (map[string]string, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			"SELECT encrypted_data FROM providers WHERE user_id = ? AND (project_id = ? OR project_id = '')",
			userID, projectID[0],
		)
	} else {
		rows, err = s.db.Query("SELECT encrypted_data FROM providers WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	envVars := map[string]string{}
	for rows.Next() {
		var encData string
		rows.Scan(&encData)

		plaintext, err := Decrypt(secret, encData)
		if err != nil {
			continue
		}

		var data map[string]string
		if err := json.Unmarshal([]byte(plaintext), &data); err != nil {
			continue
		}

		for k, v := range data {
			// Only inject UPPER_CASE keys as env vars
			if isEnvVar(k) {
				envVars[k] = v
			}
		}
	}
	return envVars, nil
}

// providerKeyFromName converts a display-pretty provider name into the
// kebab-case lookup key the rest of the stack uses ("OpenCode Go" →
// "opencode-go"). createProviderByName, FetchModels, isLLMKey, and the
// core's case-by-name dispatch all expect this normalized form.
func providerKeyFromName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// staleModelIDs is the per-provider set of model strings that older
// dashboards (or earlier core seeds) saved into provider_data and which
// no longer point to a model we want to use by default. When the read
// path encounters one of these, we treat it as if no override was set
// — the core's provider factory default takes over.
//
// We don't rewrite the DB row: that would silently change a value the
// user might have explicitly chosen. We just stop *honoring* the value
// at boot. Users who actually want one of these can re-pick it in
// the dashboard provider settings, which writes a fresh model_large
// the next time and bypasses this list.
var staleModelIDs = map[string]map[string]bool{
	"fireworks": {
		// kimi-k2p5-turbo was the prior default routing target before
		// kimi-k2p6 shipped. Saved in many existing user provider rows
		// from when it was the core factory default. Resurfaces as an
		// instance config.json value every time the agent reboots.
		"accounts/fireworks/routers/kimi-k2p5-turbo": true,
	},
}

// normalizeStaleModel returns "" when the saved model string is a
// known-deprecated default that should fall back to the provider
// factory; otherwise returns the input verbatim.
func normalizeStaleModel(providerKey, model string) string {
	if model == "" {
		return ""
	}
	if set, ok := staleModelIDs[providerKey]; ok && set[model] {
		return ""
	}
	return model
}

// isEnvVar returns true if the key looks like an env var (UPPER_CASE_WITH_UNDERSCORES).
func isEnvVar(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// --- HTTP Handlers ---

// GET /provider-types
func (s *Server) handleListProviderTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	types, err := s.store.ListProviderTypes()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if types == nil {
		types = []ProviderType{}
	}
	writeJSON(w, types)
}

// POST /providers
func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		ProviderTypeID int64             `json:"provider_type_id"`
		Type           string            `json:"type"`
		Name           string            `json:"name"`
		Data           map[string]string `json:"data"`
		ProjectID      string            `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Type == "" || body.Name == "" {
		http.Error(w, "type and name required", http.StatusBadRequest)
		return
	}

	// Allow empty data for providers that don't require credentials
	if body.Data == nil {
		body.Data = map[string]string{}
	}

	dataJSON, _ := json.Marshal(body.Data)
	encrypted, err := Encrypt(s.secret, string(dataJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	provider, err := s.store.CreateProvider(userID, body.ProviderTypeID, body.Type, body.Name, encrypted, body.ProjectID)
	if err != nil {
		http.Error(w, "failed to create provider", http.StatusInternalServerError)
		return
	}

	// Composio: mirror the user's existing connected accounts + custom MCP
	// servers into our tables so the dashboard reflects current upstream
	// state without forcing the user to rebuild connections here.
	// Best-effort async — failures are logged, provider create still succeeds.
	if strings.EqualFold(provider.Name, "Composio") {
		go s.syncComposioProviderData(userID, provider.ID, provider.ProjectID)
	}

	writeJSON(w, provider)
}

// GET /providers[?project_id=<id>]
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	providers, err := s.store.ListProviders(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if providers == nil {
		providers = []Provider{}
	}
	writeJSON(w, providers)
}

// GET /providers/:id — returns provider with masked data
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	provider, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	// Decrypt and mask secrets
	plaintext, err := Decrypt(s.secret, encData)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}

	var data map[string]string
	json.Unmarshal([]byte(plaintext), &data)

	masked := map[string]string{}
	for k, v := range data {
		if isEnvVar(k) && len(v) > 8 {
			masked[k] = v[:4] + "..." + v[len(v)-4:]
		} else {
			masked[k] = v
		}
	}

	writeJSON(w, map[string]any{
		"id":         provider.ID,
		"type":       provider.Type,
		"name":       provider.Name,
		"data":       masked,
		"created_at": provider.CreatedAt,
		"updated_at": provider.UpdatedAt,
	})
}

// PUT /providers/:id
func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	// Read existing data
	provider, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	_ = provider

	var body struct {
		Type string            `json:"type"`
		Name string            `json:"name"`
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Type == "" {
		body.Type = provider.Type
	}
	if body.Name == "" {
		body.Name = provider.Name
	}

	// Decrypt existing data and merge — skip masked values (contain "***")
	var existing map[string]string
	if plaintext, err := Decrypt(s.secret, encData); err == nil {
		json.Unmarshal([]byte(plaintext), &existing)
	}
	if existing == nil {
		existing = map[string]string{}
	}
	for k, v := range body.Data {
		// Skip masked values — keep existing secret
		if isEnvVar(k) && strings.Contains(v, "...") {
			continue
		}
		existing[k] = v
	}

	dataJSON, _ := json.Marshal(existing)
	encrypted, err := Encrypt(s.secret, string(dataJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	if err := s.store.UpdateProvider(userID, providerID, body.Type, body.Name, encrypted); err != nil {
		http.Error(w, "failed to update", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "updated"})
}

// DELETE /providers/:id
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}
	s.store.DeleteProvider(userID, providerID)
	writeJSON(w, map[string]string{"status": "deleted"})
}

// GetProviderInfo extracts provider type + model selections from the first LLM provider.
// Kept for backward compatibility — use GetProviderPool for multi-provider support.
func (s *Server) GetProviderInfo(userID int64, projectID ...string) ProviderInfo {
	pool := s.GetProviderPool(userID, projectID...)
	if len(pool) == 0 {
		return ProviderInfo{}
	}
	return pool[0]
}

// GetProviderPool returns all LLM providers for a user, optionally scoped to a
// project (+ unscoped globals). First provider is marked as default. Only
// includes LLM providers (skips integrations, embeddings, browser, etc.).
//
// Two storage formats coexist in the providers table:
//
//   - Legacy (pre-provider-types): the `type` column held the specific
//     provider name ("google", "fireworks", ...). These rows have
//     provider_type_id = 0.
//   - New (seeded via provider_types): the `type` column holds the category
//     ("llm"), and the specific provider name is in the `name` column
//     ("Fireworks", "OpenAI", ...).
//
// This loop normalizes both into a single `providerKey` (lowercase specific
// name) and uses it as both the pool entry's Type and the downstream
// config.json provider name, so cores always get concrete names like
// "fireworks" rather than the category "llm".
func (s *Server) GetProviderPool(userID int64, projectID ...string) []ProviderInfo {
	providers, err := s.store.ListProviders(userID, projectID...)
	if err != nil || len(providers) == 0 {
		return nil
	}

	isLLMKey := func(k string) bool {
		switch k {
		case "fireworks", "openai", "anthropic", "google", "ollama", "nvidia", "opencode-go":
			return true
		}
		return false
	}

	var pool []ProviderInfo
	for _, p := range providers {
		// Normalize across the two formats. If type == "llm" this is a
		// new-format row and we use the name column as the provider key.
		// Otherwise we treat type as the key (legacy format).
		providerKey := strings.ToLower(p.Type)
		if providerKey == "llm" {
			providerKey = providerKeyFromName(p.Name)
		}
		if !isLLMKey(providerKey) {
			continue
		}

		_, encData, err := s.store.GetProvider(userID, p.ID)
		if err != nil {
			pool = append(pool, ProviderInfo{Type: providerKey})
			continue
		}
		plaintext, err := Decrypt(s.secret, encData)
		if err != nil {
			pool = append(pool, ProviderInfo{Type: providerKey})
			continue
		}
		var data map[string]string
		json.Unmarshal([]byte(plaintext), &data)

		var builtinTools []string
		if bt, ok := data["builtin_tools"]; ok && bt != "" {
			json.Unmarshal([]byte(bt), &builtinTools)
		}

		pool = append(pool, ProviderInfo{
			Type:         providerKey,
			ModelLarge:   normalizeStaleModel(providerKey, data["model_large"]),
			ModelMedium:  normalizeStaleModel(providerKey, data["model_medium"]),
			ModelSmall:   normalizeStaleModel(providerKey, data["model_small"]),
			BuiltinTools: builtinTools,
		})
	}
	return pool
}

// GET /providers/:id/models — fetch live model list
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	path := strings.TrimPrefix(r.URL.Path, "/providers/")
	idStr := strings.TrimSuffix(path, "/models")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	provider, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	// Decrypt to get API key
	plaintext, err := Decrypt(s.secret, encData)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	var data map[string]string
	json.Unmarshal([]byte(plaintext), &data)

	// Find the API key
	apiKey := ""
	for k, v := range data {
		if strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_API_KEY") {
			apiKey = v
			break
		}
	}
	if apiKey == "" {
		http.Error(w, "no API key found in provider data", http.StatusBadRequest)
		return
	}

	// Normalize provider type: "llm" rows use the name as the key.
	// Names may be display-pretty (e.g. "OpenCode Go") — collapse to
	// the kebab form FetchModels / createProviderByName expect.
	providerKey := strings.ToLower(provider.Type)
	if providerKey == "llm" {
		providerKey = providerKeyFromName(provider.Name)
	}

	models, err := FetchModels(providerKey, apiKey)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch models: %v", err), http.StatusBadGateway)
		return
	}

	writeJSON(w, models)
}
