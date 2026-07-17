package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ProviderType struct {
	ID                  int64    `json:"id"`
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Fields              []string `json:"fields"`
	RequiresCredentials bool     `json:"requires_credentials"`
	AuthType            string   `json:"auth_type"`
	AuthProvider        string   `json:"auth_provider"`
	RuntimeStatus       string   `json:"runtime_status"`
	Capabilities        []string `json:"capabilities"`
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

var codexProviderRefreshLocks sync.Map

func codexProviderRefreshLock(providerID int64) *sync.Mutex {
	v, _ := codexProviderRefreshLocks.LoadOrStore(providerID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// --- Store methods ---

func (s *Store) ListProviderTypes() ([]ProviderType, error) {
	rows, err := s.db.Query(`SELECT id, type, name, description, fields, requires_credentials,
		COALESCE(auth_type, 'api_key'), COALESCE(auth_provider, ''),
		COALESCE(runtime_status, 'available'), COALESCE(capabilities, '[]'),
		sort_order FROM provider_types ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []ProviderType
	for rows.Next() {
		var pt ProviderType
		var fieldsJSON string
		var capabilitiesJSON string
		var reqCreds int
		rows.Scan(&pt.ID, &pt.Type, &pt.Name, &pt.Description, &fieldsJSON, &reqCreds, &pt.AuthType, &pt.AuthProvider, &pt.RuntimeStatus, &capabilitiesJSON, &pt.SortOrder)
		json.Unmarshal([]byte(fieldsJSON), &pt.Fields)
		json.Unmarshal([]byte(capabilitiesJSON), &pt.Capabilities)
		pt.RequiresCredentials = reqCreds == 1
		if pt.AuthType == "" {
			pt.AuthType = "api_key"
		}
		if pt.AuthProvider == "" {
			pt.AuthProvider = providerKeyFromName(pt.Name)
		}
		if pt.RuntimeStatus == "" {
			pt.RuntimeStatus = "available"
		}
		types = append(types, pt)
	}
	return types, nil
}

func (s *Store) GetProviderType(providerTypeID int64) (*ProviderType, error) {
	var pt ProviderType
	var fieldsJSON string
	var capabilitiesJSON string
	var reqCreds int
	err := s.db.QueryRow(`SELECT id, type, name, description, fields, requires_credentials,
		COALESCE(auth_type, 'api_key'), COALESCE(auth_provider, ''),
		COALESCE(runtime_status, 'available'), COALESCE(capabilities, '[]'),
		sort_order FROM provider_types WHERE id = ?`, providerTypeID).
		Scan(&pt.ID, &pt.Type, &pt.Name, &pt.Description, &fieldsJSON, &reqCreds, &pt.AuthType, &pt.AuthProvider, &pt.RuntimeStatus, &capabilitiesJSON, &pt.SortOrder)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(fieldsJSON), &pt.Fields)
	json.Unmarshal([]byte(capabilitiesJSON), &pt.Capabilities)
	pt.RequiresCredentials = reqCreds == 1
	if pt.AuthType == "" {
		pt.AuthType = "api_key"
	}
	if pt.AuthProvider == "" {
		pt.AuthProvider = providerKeyFromName(pt.Name)
	}
	if pt.RuntimeStatus == "" {
		pt.RuntimeStatus = "available"
	}
	return &pt, nil
}

// CreateProvider stores a new provider for a user. If projectID is provided
// and non-empty, the provider is scoped to that project; otherwise it is
// "unscoped" (project_id=”) and visible across all projects.
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
// AND unscoped (project_id=”) providers — the latter act as "global" so
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

func (s *Store) FindProviderByTypeForProject(userID, providerTypeID int64, projectID string) (*Provider, string, error) {
	var p Provider
	var encData string
	var createdAt, updatedAt string
	err := s.db.QueryRow(
		`SELECT id, provider_type_id, type, name, COALESCE(status,'active'), COALESCE(project_id,''), encrypted_data, created_at, updated_at
		 FROM providers
		 WHERE user_id = ? AND provider_type_id = ? AND COALESCE(project_id,'') = ?
		 ORDER BY id DESC LIMIT 1`,
		userID, providerTypeID, projectID,
	).Scan(&p.ID, &p.ProviderTypeID, &p.Type, &p.Name, &p.Status, &p.ProjectID, &encData, &createdAt, &updatedAt)
	if err != nil {
		return nil, "", err
	}
	p.UserID = userID
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updatedAt)
	return &p, encData, nil
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
// Precedence: a project-scoped row wins over a global (project_id=”)
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
			"SELECT id, encrypted_data FROM providers WHERE user_id = ? AND (project_id = ? OR project_id = '')",
			userID, projectID[0],
		)
	} else {
		rows, err = s.db.Query("SELECT id, encrypted_data FROM providers WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	envVars := map[string]string{}
	for rows.Next() {
		var providerID int64
		var encData string
		rows.Scan(&providerID, &encData)

		plaintext, err := Decrypt(secret, encData)
		if err != nil {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(plaintext), &data); err != nil {
			continue
		}

		if stateMap(data, "auth")["provider"] == openAICodexAuthProvider {
			data, _, err = s.RefreshOpenAICodexProviderState(providerID, 0, secret, 10*time.Minute, false, "provider_env")
			if err != nil {
				return nil, fmt.Errorf("OpenAI Codex auth refresh failed: %w. Please sign in again in Settings → Providers", err)
			}
			if token := stringFromNested(data, "credentials", "access_token"); strings.TrimSpace(token) != "" {
				envVars["OPENAI_CODEX_ACCESS_TOKEN"] = token
				envVars["OPENAI_CODEX_PROVIDER_ID"] = fmt.Sprint(providerID)
				if accountID := codexAccountIDFromState(data); accountID != "" {
					envVars["OPENAI_CODEX_ACCOUNT_ID"] = accountID
				}
			}
		}
		for k, v := range data {
			// Only inject UPPER_CASE keys as env vars
			if isEnvVar(k) {
				if s, ok := v.(string); ok {
					envVars[k] = s
				}
			}
		}
	}
	return envVars, nil
}

func (s *Store) RefreshOpenAICodexProviderState(providerID, userID int64, secret []byte, skew time.Duration, force bool, source string) (map[string]any, bool, error) {
	if skew <= 0 {
		skew = 10 * time.Minute
	}
	if strings.TrimSpace(source) == "" {
		source = "refresh_token"
	}
	lock := codexProviderRefreshLock(providerID)
	lock.Lock()
	defer lock.Unlock()

	allowForce := force
	for attempt := 0; attempt < 3; attempt++ {
		encrypted, err := s.getProviderEncryptedDataForRefresh(providerID, userID)
		if err != nil {
			return nil, false, err
		}
		plaintext, err := Decrypt(secret, encrypted)
		if err != nil {
			return nil, false, err
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(plaintext), &state); err != nil {
			return nil, false, err
		}
		if provider := stringFromNested(state, "auth", "provider"); provider != "" && provider != openAICodexAuthProvider {
			return state, false, nil
		}
		if !allowForce && !codexStateNeedsRefresh(state, skew) {
			return state, false, nil
		}
		refreshToken := stringFromNested(state, "credentials", "refresh_token")
		if strings.TrimSpace(refreshToken) == "" {
			if exp, ok := expiryFromState(state); ok && time.Until(exp) <= skew {
				return nil, false, fmt.Errorf("access token expires at %s and no refresh_token is stored", exp.Format(time.RFC3339))
			}
			return state, false, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		tokens, err := refreshOpenAICodexTokens(ctx, refreshToken)
		cancel()
		if err != nil {
			return nil, false, err
		}
		if nextRefresh, _ := tokens["refresh_token"].(string); strings.TrimSpace(nextRefresh) == "" {
			tokens["refresh_token"] = refreshToken
		}
		next := mergeOpenAICodexProviderState(state, buildOpenAICodexProviderState(tokens, source))
		encryptedNext, err := marshalEncryptProviderState(secret, next)
		if err != nil {
			return nil, false, err
		}
		updated, err := s.updateProviderEncryptedDataCAS(providerID, userID, encrypted, encryptedNext)
		if err != nil {
			return nil, false, err
		}
		if updated {
			return next, true, nil
		}
		allowForce = false
	}
	return nil, false, fmt.Errorf("OpenAI Codex provider changed during refresh; retry")
}

func (s *Store) getProviderEncryptedDataForRefresh(providerID, userID int64) (string, error) {
	var encrypted string
	var err error
	if userID > 0 {
		err = s.db.QueryRow("SELECT encrypted_data FROM providers WHERE id = ? AND user_id = ?", providerID, userID).Scan(&encrypted)
	} else {
		err = s.db.QueryRow("SELECT encrypted_data FROM providers WHERE id = ?", providerID).Scan(&encrypted)
	}
	return encrypted, err
}

func (s *Store) updateProviderEncryptedDataCAS(providerID, userID int64, previousEncrypted, nextEncrypted string) (bool, error) {
	var res sql.Result
	var err error
	if userID > 0 {
		res, err = s.db.Exec(
			`UPDATE providers
			    SET encrypted_data = ?, updated_at = CURRENT_TIMESTAMP
			  WHERE id = ? AND user_id = ? AND encrypted_data = ?`,
			nextEncrypted, providerID, userID, previousEncrypted,
		)
	} else {
		res, err = s.db.Exec(
			`UPDATE providers
			    SET encrypted_data = ?, updated_at = CURRENT_TIMESTAMP
			  WHERE id = ? AND encrypted_data = ?`,
			nextEncrypted, providerID, previousEncrypted,
		)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
	// Filter out integration providers that don't require credentials.
	// Today there's exactly one such row — "Apteva Local" — which used
	// to gate access to the bundled integration catalog. The catalog
	// is now always-on (auto-downloaded on first boot, baked into the
	// dev tree, served unconditionally), so the row no longer
	// represents anything the operator can meaningfully configure.
	// Composio (type=integrations + requires_credentials=1) still
	// shows up as a real activatable provider.
	filtered := types[:0]
	for _, t := range types {
		if t.Type == "integrations" && !t.RequiresCredentials {
			continue
		}
		filtered = append(filtered, t)
	}
	if filtered == nil {
		filtered = []ProviderType{}
	}
	writeJSON(w, filtered)
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

	// Pre-flight credential probe. Mirrors handleCreateConnection's
	// pre-flight: refuse to persist credentials we can't authenticate
	// against the upstream. Providers without a probe (Apteva Local,
	// or any new provider whose probe isn't in providerProbes yet)
	// return Skipped=true and we let the save through.
	//
	// On failure we return 400 with the same ProviderTestResult shape
	// the standalone /test endpoint emits, so the dashboard renders
	// one error-row component for both paths. Pre-flight bypass is
	// available via ?skip_health_check=1 for the rare case where an
	// operator wants to seed creds that won't be usable yet (e.g.
	// during initial setup before DNS propagates).
	skipCheck := r.URL.Query().Get("skip_health_check") == "1"
	if !skipCheck {
		res := runProviderHealthCheck(body.Name, body.Data)
		if !res.OK && !res.Skipped {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(res)
			return
		}
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

	var data map[string]any
	if err := json.Unmarshal([]byte(plaintext), &data); err != nil {
		http.Error(w, "invalid provider data", http.StatusInternalServerError)
		return
	}

	masked := map[string]string{}
	for k, raw := range data {
		v, ok := raw.(string)
		if !ok {
			continue
		}
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
	var existing map[string]any
	if plaintext, err := Decrypt(s.secret, encData); err == nil {
		json.Unmarshal([]byte(plaintext), &existing)
	}
	if existing == nil {
		existing = map[string]any{}
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
		case "fireworks", "openai", "openai-codex", "anthropic", "google", "ollama", "nvidia", "opencode-go", "venice", "xai":
			return true
		}
		return false
	}

	var pool []ProviderInfo
	var codexPool []ProviderInfo
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
		var data map[string]any
		if json.Unmarshal([]byte(plaintext), &data) != nil {
			pool = append(pool, ProviderInfo{Type: providerKey})
			continue
		}
		if providerKey == openAICodexAuthProvider {
			_, hasCapabilities := data["model_capabilities"]
			needsCatalog := !hasCapabilities || strings.TrimSpace(stringValue(data["model_large"])) == "" ||
				strings.TrimSpace(stringValue(data["model_medium"])) == "" || strings.TrimSpace(stringValue(data["model_small"])) == ""
			if needsCatalog {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				models, fetchErr := fetchCodexModelCatalog(ctx,
					stringFromNested(data, "credentials", "access_token"), codexAccountIDFromState(data), false)
				cancel()
				if fetchErr == nil {
					applyCodexCatalogToState(data, models)
					if err := s.persistCodexCatalogState(userID, &p, models); err != nil {
						log.Printf("[CODEX-MODELS] provider=%d could not persist catalog defaults: %v", p.ID, err)
					}
				} else {
					log.Printf("[CODEX-MODELS] provider=%d catalog unavailable; retaining saved models: %v", p.ID, fetchErr)
				}
			}
		}

		var builtinTools []string
		if bt, ok := data["builtin_tools"].(string); ok && bt != "" {
			_ = json.Unmarshal([]byte(bt), &builtinTools)
		} else if bt, ok := data["builtin_tools"].([]any); ok {
			for _, item := range bt {
				if value, ok := item.(string); ok {
					builtinTools = append(builtinTools, value)
				}
			}
		}
		modelCapabilities := map[string]ProviderModelCapabilities{}
		if rawCaps, ok := data["model_capabilities"]; ok {
			if encoded, marshalErr := json.Marshal(rawCaps); marshalErr == nil {
				_ = json.Unmarshal(encoded, &modelCapabilities)
			}
		}

		info := ProviderInfo{
			Type:              providerKey,
			ModelLarge:        normalizeStaleModel(providerKey, stringValue(data["model_large"])),
			ModelMedium:       normalizeStaleModel(providerKey, stringValue(data["model_medium"])),
			ModelSmall:        normalizeStaleModel(providerKey, stringValue(data["model_small"])),
			BuiltinTools:      builtinTools,
			ModelCapabilities: modelCapabilities,
		}
		if providerKey == "openai-codex" {
			codexPool = append(codexPool, info)
			continue
		}
		pool = append(pool, info)
	}
	combined := append(pool, codexPool...)
	// Realtime adapters reuse their text provider's credential but remain
	// separate core session types. Inject companions without synthetic DB rows.
	var realtimeCompanions []ProviderInfo
	for _, info := range combined {
		switch info.Type {
		case "openai":
			realtimeCompanions = append(realtimeCompanions, ProviderInfo{
				Type: "openai-realtime", ModelLarge: "gpt-realtime-2.1",
				ModelMedium: "gpt-realtime-2.1-mini", ModelSmall: "gpt-realtime-2.1-mini",
				RealtimeVoice: "marin",
			})
		case "xai":
			realtimeCompanions = append(realtimeCompanions, ProviderInfo{
				Type: "xai-realtime", ModelLarge: "grok-voice-latest",
				ModelMedium: "grok-voice-latest", ModelSmall: "grok-voice-latest",
				RealtimeVoice: "eve",
			})
		case "google":
			realtimeCompanions = append(realtimeCompanions, ProviderInfo{
				Type: "google-realtime", ModelLarge: "gemini-3.1-flash-live-preview",
				ModelMedium: "gemini-3.1-flash-live-preview", ModelSmall: "gemini-3.1-flash-live-preview",
				RealtimeVoice: "Kore",
			})
		}
	}
	return append(combined, realtimeCompanions...)
}

// GET /providers/:id/models — fetch live model list
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		s.handleSaveProviderModels(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
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
	var state map[string]any
	if err := json.Unmarshal([]byte(plaintext), &state); err != nil {
		http.Error(w, "invalid provider data", http.StatusInternalServerError)
		return
	}

	providerKey := strings.ToLower(provider.Type)
	if providerKey == "llm" {
		providerKey = providerKeyFromName(provider.Name)
	}
	if providerKey == openAICodexAuthProvider {
		if codexStateNeedsRefresh(state, 10*time.Minute) {
			state, _, err = s.store.RefreshOpenAICodexProviderState(providerID, userID, s.secret, 10*time.Minute, false, "model_catalog")
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to refresh Codex auth: %v", err), http.StatusBadGateway)
				return
			}
		}
		force := r.URL.Query().Get("refresh") == "1"
		models, fetchErr := fetchCodexModelCatalog(r.Context(),
			stringFromNested(state, "credentials", "access_token"), codexAccountIDFromState(state), force)
		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("failed to fetch models: %v", fetchErr), http.StatusBadGateway)
			return
		}
		if err := s.persistCodexCatalogState(userID, provider, models); err != nil {
			http.Error(w, "failed to persist Codex model defaults", http.StatusInternalServerError)
			return
		}
		writeJSON(w, models)
		return
	}

	// Find the API key
	apiKey := ""
	for k, raw := range state {
		v, _ := raw.(string)
		if strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_API_KEY") {
			apiKey = v
			break
		}
	}
	if apiKey == "" {
		http.Error(w, "no API key found in provider data", http.StatusBadRequest)
		return
	}

	models, err := FetchModels(providerKey, apiKey)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch models: %v", err), http.StatusBadGateway)
		return
	}

	writeJSON(w, models)
}

func (s *Server) persistCodexCatalogState(userID int64, provider *Provider, models []ModelInfo) error {
	lock := codexProviderRefreshLock(provider.ID)
	lock.Lock()
	defer lock.Unlock()
	_, encrypted, err := s.store.GetProvider(userID, provider.ID)
	if err != nil {
		return err
	}
	plaintext, err := Decrypt(s.secret, encrypted)
	if err != nil {
		return err
	}
	state := map[string]any{}
	if err := json.Unmarshal([]byte(plaintext), &state); err != nil {
		return err
	}
	applyCodexCatalogToState(state, models)
	next, err := marshalEncryptProviderState(s.secret, state)
	if err != nil {
		return err
	}
	return s.store.UpdateProvider(userID, provider.ID, provider.Type, provider.Name, next)
}

func (s *Server) handleSaveProviderModels(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/providers/")
	idStr := strings.TrimSuffix(path, "/models")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}
	provider, _, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	var body struct {
		Large  string `json:"large"`
		Medium string `json:"medium"`
		Small  string `json:"small"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	lock := codexProviderRefreshLock(providerID)
	lock.Lock()
	defer lock.Unlock()
	provider, encrypted, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	plaintext, err := Decrypt(s.secret, encrypted)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	state := map[string]any{}
	if err := json.Unmarshal([]byte(plaintext), &state); err != nil {
		http.Error(w, "invalid provider data", http.StatusInternalServerError)
		return
	}
	state["model_large"] = strings.TrimSpace(body.Large)
	state["model_medium"] = strings.TrimSpace(body.Medium)
	state["model_small"] = strings.TrimSpace(body.Small)

	providerKey := strings.ToLower(provider.Type)
	if providerKey == "llm" {
		providerKey = providerKeyFromName(provider.Name)
	}
	if providerKey == openAICodexAuthProvider {
		models, fetchErr := fetchCodexModelCatalog(r.Context(),
			stringFromNested(state, "credentials", "access_token"), codexAccountIDFromState(state), false)
		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("failed to validate Codex models: %v", fetchErr), http.StatusBadGateway)
			return
		}
		available := map[string]bool{}
		for _, model := range models {
			available[model.ID] = true
		}
		for tier, selected := range map[string]string{"large": body.Large, "medium": body.Medium, "small": body.Small} {
			if strings.TrimSpace(selected) != "" && !available[strings.TrimSpace(selected)] {
				http.Error(w, fmt.Sprintf("%s model %q is not available for this Codex account", tier, selected), http.StatusBadRequest)
				return
			}
		}
		applyCodexCatalogToState(state, models)
	} else if providerKey == "xai" {
		apiKey := strings.TrimSpace(stringValue(state["XAI_API_KEY"]))
		if apiKey == "" {
			http.Error(w, "xAI provider is missing XAI_API_KEY", http.StatusBadRequest)
			return
		}
		models, fetchErr := FetchModels(providerKey, apiKey)
		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("failed to validate xAI models: %v", fetchErr), http.StatusBadGateway)
			return
		}
		available := make(map[string]ModelInfo, len(models))
		for _, model := range models {
			available[model.ID] = model
		}
		selectedCapabilities := map[string]ProviderModelCapabilities{}
		for tier, selected := range map[string]string{"large": body.Large, "medium": body.Medium, "small": body.Small} {
			selected = strings.TrimSpace(selected)
			if selected == "" {
				continue
			}
			model, ok := available[selected]
			if !ok {
				http.Error(w, fmt.Sprintf("%s model %q is not available for this xAI account", tier, selected), http.StatusBadRequest)
				return
			}
			selectedCapabilities[selected] = model.Capabilities
		}
		state["model_capabilities"] = selectedCapabilities
	}
	next, err := marshalEncryptProviderState(s.secret, state)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateProvider(userID, providerID, provider.Type, provider.Name, next); err != nil {
		http.Error(w, "failed to update model settings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"status": "updated",
		"large":  stringValue(state["model_large"]),
		"medium": stringValue(state["model_medium"]),
		"small":  stringValue(state["model_small"]),
	})
}
