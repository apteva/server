package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
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

func (s *Server) authorizeDelegatedAppRequest(w http.ResponseWriter, r *http.Request, key *APIKey, appName, appPath string) bool {
	if appName != "channel-chat" {
		return true
	}
	action := delegatedChannelChatAction(r.Method, appPath)
	if action == "" {
		http.Error(w, "delegated user key is not allowed to call this channel-chat route", http.StatusForbidden)
		return false
	}
	if !delegatedUserScopeAllows(key.Scopes, appName, action) {
		http.Error(w, "delegated user key is not allowed to perform this chat action", http.StatusForbidden)
		return false
	}
	if !publicClientOriginAllowed(key.AllowedOrigins, r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return false
	}
	if !publicClientRateAllowed(key.ID, key.RateLimitPerMinute) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return false
	}
	return true
}

func delegatedChannelChatAction(method, path string) string {
	path = strings.TrimSuffix(path, "/")
	switch {
	case path == "/chats" && method == http.MethodPost:
		return "chat.create"
	case path == "/chats" && method == http.MethodGet:
		return "chat.list"
	case strings.HasPrefix(path, "/chats/") && method == http.MethodGet:
		return "chat.read"
	case strings.HasPrefix(path, "/chats/") && method == http.MethodPatch:
		return "chat.update"
	case path == "/messages" && method == http.MethodGet:
		return "message.read"
	case path == "/messages" && method == http.MethodPost:
		return "message.send"
	case path == "/stream" && method == http.MethodGet:
		return "stream.read"
	case path == "/seen" && method == http.MethodPost:
		return "chat.seen"
	case path == "/presence" && method == http.MethodPost:
		return "chat.presence"
	default:
		return ""
	}
}

func delegatedChannelChatScope(rawScopes string) (publicClientScope, bool) {
	var scopes []publicClientScope
	if err := json.Unmarshal([]byte(rawScopes), &scopes); err != nil {
		return publicClientScope{}, false
	}
	for _, scope := range scopes {
		if scope.Type == "app_user" && scope.App == "channel-chat" {
			return scope, true
		}
	}
	return publicClientScope{}, false
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
	AllowedOrigins   []string        `json:"allowed_origins,omitempty"`
	RateLimit        int             `json:"rate_limit_per_minute,omitempty"`
}

const delegatedConversationDirectiveMaxRunes = 8000

var delegatedChatActions = []string{
	"chat.create",
	"chat.list",
	"chat.read",
	"chat.update",
	"chat.archive",
	"chat.seen",
	"chat.presence",
	"message.read",
	"message.send",
	"stream.read",
}

type createDelegatedUserRequest struct {
	ProjectID        string   `json:"project_id"`
	SubjectType      string   `json:"subject_type"`
	SubjectID        string   `json:"subject_id"`
	ExpiresIn        int      `json:"expires_in"`
	AgentID          int64    `json:"agent_id,omitempty"`
	AllowedAgentIDs  []int64  `json:"allowed_agent_ids,omitempty"`
	AllowedOrigins   []string `json:"allowed_origins"`
	ConversationRule string   `json:"conversation_directive,omitempty"`
	RateLimit        int      `json:"rate_limit_per_minute,omitempty"`
}

// handleCreateDelegatedUser mints a short-lived browser credential from a
// private API key. Session cookies and public/delegated keys cannot mint it.
// The resulting key is limited to Channel Chat, explicit agents, an exact
// browser-origin allowlist, and the stored external subject.
func (s *Server) handleCreateDelegatedUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rawPrivateKey := requestAPIKeyToken(r)
	if !strings.HasPrefix(rawPrivateKey, "sk-") {
		http.Error(w, "private API key required", http.StatusUnauthorized)
		return
	}
	issuer, err := s.store.GetUserByAPIKey(HashAPIKey(rawPrivateKey))
	if err != nil || issuer.ID != getUserID(r) {
		http.Error(w, "private API key required", http.StatusUnauthorized)
		return
	}
	var body createDelegatedUserRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	if body.ProjectID == "" {
		http.Error(w, "project_id required", http.StatusBadRequest)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectEditor); !ok {
		return
	}
	subjectType, subjectID, err := validateDelegatedSubject(body.SubjectType, body.SubjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentIDs := append([]int64(nil), body.AllowedAgentIDs...)
	if body.AgentID > 0 {
		agentIDs = append(agentIDs, body.AgentID)
	}
	agentIDs = uniquePositiveInt64s(agentIDs)
	if len(agentIDs) == 0 {
		http.Error(w, "at least one allowed agent is required", http.StatusBadRequest)
		return
	}
	for _, agentID := range agentIDs {
		agent, err := s.store.GetAgent(issuer.ID, agentID)
		if err != nil || agent.ProjectID != body.ProjectID {
			http.Error(w, fmt.Sprintf("agent %d not found in project", agentID), http.StatusNotFound)
			return
		}
	}
	origins, err := validateDelegatedOrigins(body.AllowedOrigins)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len([]rune(strings.TrimSpace(body.ConversationRule))) > delegatedConversationDirectiveMaxRunes {
		http.Error(w, "conversation_directive is too long", http.StatusBadRequest)
		return
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > delegatedUserKeyMaxTTL {
		ttl = delegatedUserKeyMaxTTL
	}
	rateLimit := body.RateLimit
	if rateLimit <= 0 {
		rateLimit = 120
	}
	scopes, _ := json.Marshal([]publicClientScope{{
		Type:      "app_user",
		App:       "channel-chat",
		Actions:   append([]string(nil), delegatedChatActions...),
		AgentIDs:  agentIDs,
		Directive: strings.TrimSpace(body.ConversationRule),
	}})
	originsJSON, _ := json.Marshal(origins)
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	raw := "uk_" + generateToken(24)
	key, err := s.store.CreateAPIKey(issuer.ID, "external chat "+subjectType+" "+subjectID, HashAPIKey(raw), raw[:11], APIKeyCreateOptions{
		Kind:               "delegated_user",
		ProjectID:          body.ProjectID,
		Scopes:             string(scopes),
		AllowedOrigins:     string(originsJSON),
		RateLimitPerMinute: rateLimit,
		ExpiresAt:          expiresAt,
		IssuerApp:          "channel-chat",
		SubjectType:        subjectType,
		SubjectID:          subjectID,
	})
	if err != nil {
		http.Error(w, "failed to create delegated credential", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"access_token":      raw,
		"token_type":        "Bearer",
		"expires_in":        int(ttl.Seconds()),
		"expires_at":        expiresAt,
		"key_prefix":        key.KeyPrefix,
		"project_id":        key.ProjectID,
		"allowed_agent_ids": agentIDs,
		"subject": map[string]string{
			"type": subjectType,
			"id":   subjectID,
		},
	})
}

func validateDelegatedSubject(rawType, rawID string) (string, string, error) {
	subjectType := strings.ToLower(strings.TrimSpace(rawType))
	if subjectType == "" {
		subjectType = "website_user"
	}
	if len(subjectType) > 64 {
		return "", "", errors.New("subject_type is too long")
	}
	for _, r := range subjectType {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return "", "", errors.New("subject_type must be a lowercase identifier")
		}
	}
	subjectID := strings.TrimSpace(rawID)
	if subjectID == "" || len([]rune(subjectID)) > 255 {
		return "", "", errors.New("subject_id is required and must be at most 255 characters")
	}
	for _, r := range subjectID {
		if unicode.IsControl(r) {
			return "", "", errors.New("subject_id cannot contain control characters")
		}
	}
	return subjectType, subjectID, nil
}

func validateDelegatedOrigins(raw []string) ([]string, error) {
	seen := map[string]bool{}
	origins := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		parsed, err := url.Parse(item)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, fmt.Errorf("invalid allowed origin %q", item)
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if !seen[origin] {
			seen[origin] = true
			origins = append(origins, origin)
		}
	}
	if len(origins) == 0 {
		return nil, errors.New("at least one allowed_origins entry is required")
	}
	return origins, nil
}

func uniquePositiveInt64s(values []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value > 0 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func (s *Server) delegatedChatCORSOriginAllowed(r *http.Request, origin string) bool {
	if s == nil || s.store == nil || r == nil {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	appName, _, ok := splitAppProxyPath(path)
	if !ok || appName != "channel-chat" {
		return false
	}
	rows, err := s.store.db.Query(`
		SELECT COALESCE(allowed_origins, '[]')
		FROM api_keys
		WHERE kind = 'delegated_user'
		  AND issuer_app = 'channel-chat'
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var allowed string
		if rows.Scan(&allowed) == nil && publicClientOriginAllowed(allowed, origin) {
			return true
		}
	}
	return false
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
	originsJSON := "[]"
	if len(body.AllowedOrigins) > 0 {
		origins, err := validateDelegatedOrigins(body.AllowedOrigins)
		if err != nil {
			return nil, err
		}
		rawOrigins, _ := json.Marshal(origins)
		originsJSON = string(rawOrigins)
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
		Kind:               "delegated_user",
		ProjectID:          requestedProjectID,
		Scopes:             scopesJSON,
		AllowedOrigins:     originsJSON,
		RateLimitPerMinute: body.RateLimit,
		ExpiresAt:          expiresAt,
		IssuerApp:          appName,
		IssuerInstallID:    installID,
		SubjectType:        subjectType,
		SubjectID:          subjectID,
		SubjectEmail:       subjectEmail,
		OrganizationID:     orgID,
		OrganizationSlug:   orgSlug,
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
