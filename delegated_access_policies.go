package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultDelegatedPolicyTTL       = 3600
	defaultDelegatedPolicyRateLimit = 120
)

var errDelegatedPolicyNotFound = errors.New("delegated access policy not found")

// delegatedAccessPolicy is platform-owned authorization. The issuing app
// supplies identity and browser origins; it cannot choose these scopes.
// Scope objects remain raw JSON so apps can define resource constraints in
// addition to the shared type/app/actions fields (for example agent_ids).
type delegatedAccessPolicy struct {
	ProjectID          string          `json:"project_id"`
	OAuthClientID      string          `json:"oauth_client_id"`
	Scopes             json.RawMessage `json:"scopes"`
	TokenTTLSeconds    int             `json:"token_ttl_seconds"`
	RateLimitPerMinute int             `json:"rate_limit_per_minute"`
	IssuerInstallID    int64           `json:"issuer_install_id,omitempty"`
}

func normalizeDelegatedAccessPolicy(policy delegatedAccessPolicy, installProjectID string) (delegatedAccessPolicy, error) {
	policy.ProjectID = strings.TrimSpace(policy.ProjectID)
	if installProjectID != "" {
		if policy.ProjectID != "" && policy.ProjectID != installProjectID {
			return delegatedAccessPolicy{}, errors.New("policy project_id does not match install project")
		}
		policy.ProjectID = installProjectID
	} else if policy.ProjectID == "" {
		return delegatedAccessPolicy{}, errors.New("project_id is required for a global issuer install")
	}
	policy.OAuthClientID = strings.TrimSpace(policy.OAuthClientID)
	if policy.OAuthClientID == "" || len(policy.OAuthClientID) > 255 {
		return delegatedAccessPolicy{}, errors.New("oauth_client_id is required and must be at most 255 characters")
	}
	if err := validateDelegatedPolicyScopes(policy.Scopes); err != nil {
		return delegatedAccessPolicy{}, err
	}
	if policy.TokenTTLSeconds == 0 {
		policy.TokenTTLSeconds = defaultDelegatedPolicyTTL
	}
	if policy.TokenTTLSeconds < 1 || policy.TokenTTLSeconds > int(delegatedUserKeyMaxTTL.Seconds()) {
		return delegatedAccessPolicy{}, fmt.Errorf("token_ttl_seconds must be between 1 and %d", int(delegatedUserKeyMaxTTL.Seconds()))
	}
	if policy.RateLimitPerMinute == 0 {
		policy.RateLimitPerMinute = defaultDelegatedPolicyRateLimit
	}
	if policy.RateLimitPerMinute < 1 || policy.RateLimitPerMinute > 10000 {
		return delegatedAccessPolicy{}, errors.New("rate_limit_per_minute must be between 1 and 10000")
	}
	return policy, nil
}

func validateDelegatedPolicyScopes(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" || !json.Valid(raw) {
		return errors.New("scopes must be a valid non-empty JSON array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 || len(entries) > 50 {
		return errors.New("scopes must contain between 1 and 50 entries")
	}
	seenApps := make(map[string]bool, len(entries))
	for i, entry := range entries {
		var scope publicClientScope
		if err := json.Unmarshal(entry, &scope); err != nil {
			return fmt.Errorf("scope %d must be an object", i)
		}
		if scope.Type != "app_user" && scope.Type != "app_action" {
			return fmt.Errorf("scope %d type must be app_user or app_action", i)
		}
		cleanApp := strings.TrimSpace(scope.App)
		if cleanApp == "" || cleanApp == "*" || cleanApp != scope.App || len(cleanApp) > 128 {
			return fmt.Errorf("scope %d app must be an explicit app name", i)
		}
		if seenApps[cleanApp] {
			return fmt.Errorf("scope %d duplicates app %q", i, cleanApp)
		}
		seenApps[cleanApp] = true
		if len(scope.Actions) == 0 || len(scope.Actions) > 100 {
			return fmt.Errorf("scope %d actions must contain between 1 and 100 entries", i)
		}
		seenActions := make(map[string]bool, len(scope.Actions))
		for _, action := range scope.Actions {
			cleanAction := strings.TrimSpace(action)
			if cleanAction == "" || cleanAction == "*" || cleanAction != action || len(cleanAction) > 255 {
				return fmt.Errorf("scope %d actions must be explicit non-empty names", i)
			}
			if seenActions[cleanAction] {
				return fmt.Errorf("scope %d contains duplicate action %q", i, cleanAction)
			}
			seenActions[cleanAction] = true
		}
		for _, agentID := range scope.AgentIDs {
			if agentID <= 0 {
				return fmt.Errorf("scope %d agent_ids must contain positive integers", i)
			}
		}
	}
	return nil
}

func (s *Server) delegatedAccessPolicyFor(installID int64, projectID, oauthClientID string) (*delegatedAccessPolicy, error) {
	var policy delegatedAccessPolicy
	var scopes string
	err := s.store.db.QueryRow(`
		SELECT project_id, oauth_client_id, scopes, token_ttl_seconds, rate_limit_per_minute
		FROM delegated_access_policies
		WHERE issuer_install_id = ? AND project_id = ? AND oauth_client_id = ?`,
		installID, strings.TrimSpace(projectID), strings.TrimSpace(oauthClientID),
	).Scan(&policy.ProjectID, &policy.OAuthClientID, &scopes, &policy.TokenTTLSeconds, &policy.RateLimitPerMinute)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errDelegatedPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	policy.IssuerInstallID = installID
	policy.Scopes = json.RawMessage(scopes)
	return &policy, nil
}

func (s *Server) replaceDelegatedAccessPolicies(installID int64, installProjectID string, policies []delegatedAccessPolicy) ([]delegatedAccessPolicy, error) {
	normalized := make([]delegatedAccessPolicy, 0, len(policies))
	seen := make(map[string]bool, len(policies))
	for _, policy := range policies {
		clean, err := normalizeDelegatedAccessPolicy(policy, installProjectID)
		if err != nil {
			return nil, err
		}
		key := clean.ProjectID + "\x00" + clean.OAuthClientID
		if seen[key] {
			return nil, fmt.Errorf("duplicate policy for project %q and oauth_client_id %q", clean.ProjectID, clean.OAuthClientID)
		}
		seen[key] = true
		clean.IssuerInstallID = installID
		normalized = append(normalized, clean)
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM delegated_access_policies WHERE issuer_install_id = ?`, installID); err != nil {
		return nil, err
	}
	for _, policy := range normalized {
		if _, err := tx.Exec(`
			INSERT INTO delegated_access_policies
				(issuer_install_id, project_id, oauth_client_id, scopes, token_ttl_seconds, rate_limit_per_minute)
			VALUES (?, ?, ?, ?, ?, ?)`,
			installID, policy.ProjectID, policy.OAuthClientID, string(policy.Scopes), policy.TokenTTLSeconds, policy.RateLimitPerMinute,
		); err != nil {
			return nil, err
		}
	}
	// Policy replacement takes effect immediately. Existing credentials carry
	// a snapshot of the old policy, so revoke every active key from this issuer
	// install in the same transaction; the next Auth refresh mints a new one.
	if _, err := tx.Exec(`
		UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP
		WHERE kind = 'delegated_user' AND issuer_install_id = ? AND revoked_at IS NULL`, installID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return normalized, nil
}

func (s *Server) listDelegatedAccessPolicies(installID int64) ([]delegatedAccessPolicy, error) {
	rows, err := s.store.db.Query(`
		SELECT project_id, oauth_client_id, scopes, token_ttl_seconds, rate_limit_per_minute
		FROM delegated_access_policies WHERE issuer_install_id = ?
		ORDER BY project_id, oauth_client_id`, installID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]delegatedAccessPolicy, 0)
	for rows.Next() {
		var policy delegatedAccessPolicy
		var scopes string
		if err := rows.Scan(&policy.ProjectID, &policy.OAuthClientID, &scopes, &policy.TokenTTLSeconds, &policy.RateLimitPerMinute); err != nil {
			return nil, err
		}
		policy.IssuerInstallID = installID
		policy.Scopes = json.RawMessage(scopes)
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// GET/PUT /api/apps/installs/:id/delegated-access-policies. PUT atomically
// replaces the install's complete policy set; an empty list disables minting.
func (s *Server) handleDelegatedAccessPolicies(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "delegated-access-policies" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || installID <= 0 {
		http.Error(w, "invalid install id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		policies, err := s.listDelegatedAccessPolicies(installID)
		if err != nil {
			http.Error(w, "failed to list delegated access policies", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"policies": policies})
	case http.MethodPut:
		var body struct {
			Policies []delegatedAccessPolicy `json:"policies"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		var installProjectID string
		if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id = ?`, installID).Scan(&installProjectID); err != nil {
			http.Error(w, "install not found", http.StatusNotFound)
			return
		}
		policies, err := s.replaceDelegatedAccessPolicies(installID, installProjectID, body.Policies)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"policies": policies})
	default:
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
	}
}
