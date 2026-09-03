package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
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
			`SELECT `+cols+` FROM providers
			 WHERE user_id = ? AND (project_id = ? OR project_id = '')
			 ORDER BY CASE WHEN project_id = ? THEN 0 ELSE 1 END, id ASC`,
			userID, projectID[0], projectID[0],
		)
	} else {
		rows, err = s.db.Query(
			`SELECT `+cols+` FROM providers WHERE user_id = ? ORDER BY id ASC`, userID,
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

// GetAllProviderEnvVars decrypts all providers for a user and returns env vars
// (UPPER_CASE keys). If projectID is provided and non-empty, only providers
// scoped to that project (or unscoped globals) are included — matching the
// visibility rules in ListProviders.
func (s *Store) GetAllProviderEnvVars(userID int64, secret []byte, projectID ...string) (map[string]string, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			`SELECT id, encrypted_data FROM providers
			 WHERE user_id = ? AND (project_id = ? OR project_id = '')
			 ORDER BY CASE WHEN project_id = '' THEN 0 ELSE 1 END, id ASC`,
			userID, projectID[0],
		)
	} else {
		rows, err = s.db.Query("SELECT id, encrypted_data FROM providers WHERE user_id = ? ORDER BY id ASC", userID)
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

// GetAllProviderEnvVars is the dual-read env resolver every agent-spawn
// path calls (providers/connections fusion).
//
// It merges the legacy providers table with connections whose catalog
// entry declares a `runtime` block. A connection only overrides a
// provider-supplied var when it was migrated from that row; otherwise
// the provider keeps it, so adding a connection never silently changes
// which credential a running install uses. Once the last provider row is
// migrated the first half returns empty and this becomes a thin wrapper
// over runtimeEnvFromConnections.
//
// The selection rule differs between the two halves in a way worth
// naming: the providers table WAS the filter — every row's UPPER_CASE
// keys got injected, which is why Browserbase credentials reached every
// core. Connections are filtered by the catalog's runtime block instead,
// so a Stripe or Twilio credential never lands in an agent's environment
// just because the row exists.
func (s *Server) GetAllProviderEnvVars(userID int64, projectID ...string) (map[string]string, error) {
	if _, managed := s.managedLLMPolicyForUser(userID); managed && s.store.GetPlatformRole(userID) != PlatformAdmin {
		policy, err := s.loadAccessPolicy()
		if err != nil {
			return nil, err
		}
		if !policy.Capabilities.ProviderManagement {
			return map[string]string{
				"APTEVA_MANAGED_LLM_URL": "http://127.0.0.1:" + s.port + "/api/llm/chat/completions",
			}, nil
		}
	}
	envVars, err := s.store.GetAllProviderEnvVars(userID, s.secret, projectID...)
	if err != nil {
		// A Codex refresh failure surfaces here and must stay fatal: booting
		// an agent with a stale token produces a confusing 401 at first
		// inference instead of an actionable "sign in again".
		return nil, err
	}
	if envVars == nil {
		envVars = map[string]string{}
	}

	connEnv, err := s.runtimeEnvFromConnections(userID, envVars, projectID...)
	if err != nil {
		return nil, err
	}
	for name, value := range connEnv {
		envVars[name] = value
	}
	if _, managed := s.managedLLMPolicyForUser(userID); managed {
		envVars["APTEVA_MANAGED_LLM_URL"] = "http://127.0.0.1:" + s.port + "/api/llm/chat/completions"
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
// isLLMKey gates which provider names may reach apteva-core's
// config.json. Package-level because the connections path
// (runtimePoolFromConnections) applies the same gate to keys read from
// catalog JSON — catalog data ships separately from the binary that has
// to understand these names, so it is never trusted to name a provider
// core has no factory for.
func isLLMKey(k string) bool {
	switch k {
	case "managed", "fireworks", "openai", "openai-codex", "anthropic", "google", "ollama", "nvidia", "opencode-go", "venice", "xai":
		return true
	}
	return false
}

func (s *Server) GetProviderPool(userID int64, projectID ...string) []ProviderInfo {
	managed, hasManaged := s.managedProviderInfoForUser(userID)
	if hasManaged && s.store.GetPlatformRole(userID) != PlatformAdmin {
		if policy, err := s.loadAccessPolicy(); err == nil && !policy.Capabilities.ProviderManagement {
			return []ProviderInfo{managed}
		}
	}
	// Dual-read (providers/connections fusion). Connections resolve first
	// and seenProviderKeys comes back pre-seeded with what they supply,
	// so the providers loop skips any key already claimed.
	//
	// A connection only displaces a provider row when it was migrated
	// from one (legacy_provider_id set). An independently-created
	// connection for a key the providers table still serves is skipped —
	// otherwise connecting Gemini would silently switch which Google key
	// every agent in the project runs on. Once the last provider row is
	// migrated, shadowed is empty and this is just the connections half.
	shadowed := s.providerSuppliedLLMKeys(userID, projectID...)
	pool, codexPool, seenProviderKeys, ranks := s.runtimePoolFromConnections(userID, shadowed, projectID...)

	providers, err := s.store.ListProviders(userID, projectID...)
	if err != nil {
		providers = nil
	}
	if len(providers) == 0 && len(pool) == 0 && len(codexPool) == 0 && !hasManaged {
		return nil
	}

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
		// ListProviders orders project-scoped rows before global rows. Keep
		// exactly one runtime entry per provider type so a global credential
		// cannot unpredictably replace the selected project's credential.
		if seenProviderKeys[providerKey] {
			continue
		}
		seenProviderKeys[providerKey] = true
		rank := providerPoolRank{anchored: true, id: p.ID}
		if len(projectID) > 0 && projectID[0] != "" && p.ProjectID != projectID[0] {
			rank.scope = 1
		}
		ranks[providerKey] = rank

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
	// Merge connection-backed and unresolved provider-backed groups by the
	// ordering the legacy providers table exposed: project before global,
	// then provider id. Never-migrated connection-only groups come after all
	// legacy-anchored groups, so a skipped/conflicting migration cannot move
	// an unrelated connection into pool[0] and silently re-point agents.
	sort.SliceStable(pool, func(i, j int) bool {
		a, b := ranks[pool[i].Type], ranks[pool[j].Type]
		if a.anchored != b.anchored {
			return a.anchored
		}
		if a.scope != b.scope {
			return a.scope < b.scope
		}
		return a.id < b.id
	})
	combined := append(pool, codexPool...)
	if hasManaged {
		combined = append([]ProviderInfo{managed}, combined...)
	}
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
