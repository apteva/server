package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const delegatedUserKeyMaxTTL = 24 * time.Hour

func setDelegatedUserPrincipalHeaders(r *http.Request, key *APIKey) {
	if r == nil || key == nil {
		return
	}
	r.Header.Set("X-Apteva-Project-ID", key.ProjectID)
	r.Header.Set("X-Apteva-Issuer-App", key.IssuerApp)
	if key.IssuerInstallID > 0 {
		r.Header.Set("X-Apteva-Issuer-Install-ID", itoa64(key.IssuerInstallID))
	}
	r.Header.Set("X-Apteva-Subject-Type", key.SubjectType)
	r.Header.Set("X-Apteva-Subject-ID", key.SubjectID)
	r.Header.Set("X-Apteva-Subject-Email", key.SubjectEmail)
	r.Header.Set("X-Apteva-Organization-ID", key.OrganizationID)
	r.Header.Set("X-Apteva-Organization-Slug", key.OrganizationSlug)
	r.Header.Set("X-Apteva-Scopes", key.Scopes)
}

func delegatedUserScopeAllows(rawScopes, appName, action string) bool {
	if publicClientScopeAllows(rawScopes, appName, action) {
		return true
	}
	var scopes []publicClientScope
	if err := json.Unmarshal([]byte(rawScopes), &scopes); err != nil {
		return false
	}
	for _, scope := range scopes {
		if scope.Type != "app_user" {
			continue
		}
		if scope.App != "*" && scope.App != appName {
			continue
		}
		for _, allowed := range scope.Actions {
			if allowed == "*" || allowed == action {
				return true
			}
		}
	}
	return false
}

type delegatedUserKeyRequest struct {
	ProjectID        string          `json:"project_id"`
	SubjectType      string          `json:"subject_type"`
	SubjectID        string          `json:"subject_id"`
	SubjectEmail     string          `json:"subject_email"`
	OrganizationID   string          `json:"organization_id"`
	OrganizationSlug string          `json:"organization_slug"`
	Scopes           json.RawMessage `json:"scopes"`
	ExpiresIn        int             `json:"expires_in"`
}

func (s *Server) handleCallbackDelegatedKeys(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || parts[0] != "mint" || r.Method != http.MethodPost {
		http.Error(w, "POST /delegated-keys/mint only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var body delegatedUserKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	out, err := s.mintDelegatedUserKeyForInstall(installID, body)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errDelegatedForbidden) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, out)
}

var errDelegatedForbidden = errors.New("install cannot mint a delegated user key for that project")

func (s *Server) mintDelegatedUserKeyForInstall(installID int64, body delegatedUserKeyRequest) (map[string]any, error) {
	var (
		installedBy int64
		projectID   string
		appName     string
	)
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(i.installed_by,0), COALESCE(i.project_id,''), a.name
		   FROM app_installs i JOIN apps a ON a.id = i.app_id
		  WHERE i.id = ? AND i.status NOT IN ('disabled','error')`,
		installID,
	).Scan(&installedBy, &projectID, &appName); err != nil {
		return nil, errDelegatedForbidden
	}
	if installedBy == 0 {
		installedBy = 1
	}
	requestedProjectID := strings.TrimSpace(body.ProjectID)
	if projectID != "" {
		if requestedProjectID != "" && requestedProjectID != projectID {
			return nil, errDelegatedForbidden
		}
		requestedProjectID = projectID
	}
	if requestedProjectID == "" {
		return nil, errors.New("project_id required")
	}
	subjectType := strings.TrimSpace(body.SubjectType)
	if subjectType == "" {
		subjectType = "user"
	}
	subjectID := strings.TrimSpace(body.SubjectID)
	if subjectID == "" {
		return nil, errors.New("subject_id required")
	}
	subjectEmail := strings.TrimSpace(strings.ToLower(body.SubjectEmail))
	orgID := strings.TrimSpace(body.OrganizationID)
	orgSlug := strings.TrimSpace(strings.ToLower(body.OrganizationSlug))
	scopesJSON := "[]"
	if len(body.Scopes) > 0 && string(body.Scopes) != "null" {
		if !json.Valid(body.Scopes) {
			return nil, errors.New("scopes must be valid JSON")
		}
		scopesJSON = string(body.Scopes)
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > delegatedUserKeyMaxTTL {
		ttl = delegatedUserKeyMaxTTL
	}
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	raw := "uk_" + generateToken(24)
	keyHash := HashAPIKey(raw)
	keyPrefix := raw[:11]
	key, err := s.store.CreateAPIKey(installedBy, "delegated "+subjectType+" "+subjectID, keyHash, keyPrefix, APIKeyCreateOptions{
		Kind:             "delegated_user",
		ProjectID:        requestedProjectID,
		Scopes:           scopesJSON,
		ExpiresAt:        expiresAt,
		IssuerApp:        appName,
		IssuerInstallID:  installID,
		SubjectType:      subjectType,
		SubjectID:        subjectID,
		SubjectEmail:     subjectEmail,
		OrganizationID:   orgID,
		OrganizationSlug: orgSlug,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"access_token": raw,
		"token_type":   "Bearer",
		"expires_in":   int(ttl.Seconds()),
		"expires_at":   expiresAt,
		"key_prefix":   key.KeyPrefix,
		"project_id":   key.ProjectID,
		"subject": map[string]any{
			"type":              subjectType,
			"id":                subjectID,
			"email":             subjectEmail,
			"organization_id":   orgID,
			"organization_slug": orgSlug,
		},
	}, nil
}
