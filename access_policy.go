package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

const accessPolicySettingKey = "access_policy"

var (
	projectAdmissionMu sync.Mutex
	agentAdmissionMu   sync.Mutex
)

// AccessPolicy is the server-wide policy for installations that admit more
// than one user. It deliberately uses explicit fields instead of a generic
// policy language. An absent policy preserves the historical self-hosted
// behavior; once saved, every public entry point reads the same document.
type AccessPolicy struct {
	Registration       RegistrationAccessPolicy `json:"registration"`
	Provisioning       ProvisioningAccessPolicy `json:"provisioning"`
	Limits             ResourceAccessPolicy     `json:"limits"`
	Capabilities       CapabilityAccessPolicy   `json:"capabilities"`
	WorkspaceLifecycle WorkspaceLifecyclePolicy `json:"workspace_lifecycle"`
	ManagedLLM         ManagedLLMPolicy         `json:"managed_llm"`
}

type RegistrationAccessPolicy struct {
	Mode                      string `json:"mode"`
	RegistrationsPerIPPerHour int    `json:"registrations_per_ip_per_hour"`
}

type ProvisioningAccessPolicy struct {
	PresetID           string `json:"preset_id,omitempty"`
	ProjectName        string `json:"project_name"`
	ProjectDescription string `json:"project_description"`
}

type ResourceAccessPolicy struct {
	ProjectsPerUser          int `json:"projects_per_user"`
	AgentsPerProject         int `json:"agents_per_project"`
	RunningAgentsPerProject  int `json:"running_agents_per_project"`
	DailyModelCalls          int `json:"daily_model_calls"`
	DailyTokens              int `json:"daily_tokens"`
	ConcurrentLLMRequests    int `json:"concurrent_llm_requests"`
	GlobalConcurrentLLMCalls int `json:"global_concurrent_llm_calls"`
}

type CapabilityAccessPolicy struct {
	APIKeys              bool     `json:"api_keys"`
	CustomMCP            bool     `json:"custom_mcp"`
	ProviderManagement   bool     `json:"provider_management"`
	AppInstallation      bool     `json:"app_installation"`
	Invitations          bool     `json:"invitations"`
	Domains              bool     `json:"domains"`
	Backups              bool     `json:"backups"`
	RealtimeVoice        bool     `json:"realtime_voice"`
	AutonomousScheduling bool     `json:"autonomous_scheduling"`
	AllowedApps          []string `json:"allowed_apps,omitempty"`
	AllowedModels        []string `json:"allowed_models,omitempty"`
}

type WorkspaceLifecyclePolicy struct {
	ExpiresAfter      string `json:"expires_after,omitempty"`
	IdleShutdownAfter string `json:"idle_shutdown_after,omitempty"`
	ResetFromPreset   bool   `json:"reset_from_preset"`
}

type ManagedLLMPolicy struct {
	ConnectionID int64    `json:"connection_id,omitempty"`
	Configured   bool     `json:"configured,omitempty"`
	Path         string   `json:"path,omitempty"`
	Models       []string `json:"models,omitempty"`
}

type ManagedLLMUsage struct {
	Date         string `json:"date"`
	Calls        int64  `json:"calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

func defaultAccessPolicy(regMode string) AccessPolicy {
	if regMode == "" {
		regMode = "locked"
	}
	return AccessPolicy{
		Registration: RegistrationAccessPolicy{
			Mode: regMode, RegistrationsPerIPPerHour: 3,
		},
		Provisioning: ProvisioningAccessPolicy{
			ProjectName: "Default", ProjectDescription: "Default project",
		},
		Capabilities: CapabilityAccessPolicy{
			APIKeys: true, CustomMCP: true, ProviderManagement: true,
			AppInstallation: true, Invitations: true, Domains: true,
			Backups: true, RealtimeVoice: true, AutonomousScheduling: true,
		},
	}
}

// closedAccessPolicy is returned on a database or decode failure after an
// operator has chosen to use policy-backed access. Security controls must not
// silently become permissive because the old GetSetting helper treats errors
// as an empty optional setting.
func closedAccessPolicy() AccessPolicy {
	p := defaultAccessPolicy("locked")
	p.Registration.RegistrationsPerIPPerHour = 1
	p.Capabilities = CapabilityAccessPolicy{}
	return p
}

func (s *Server) loadAccessPolicy() (AccessPolicy, error) {
	if s == nil || s.store == nil {
		return closedAccessPolicy(), errors.New("settings store unavailable")
	}
	raw, found, err := s.store.GetSettingStrict(accessPolicySettingKey)
	if err != nil {
		return closedAccessPolicy(), err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return defaultAccessPolicy(s.regMode), nil
	}
	var policy AccessPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return closedAccessPolicy(), fmt.Errorf("decode access policy: %w", err)
	}
	if err := validateAccessPolicy(policy); err != nil {
		return closedAccessPolicy(), err
	}
	return normalizeAccessPolicy(policy), nil
}

func normalizeAccessPolicy(policy AccessPolicy) AccessPolicy {
	policy.Registration.Mode = strings.ToLower(strings.TrimSpace(policy.Registration.Mode))
	if policy.Registration.RegistrationsPerIPPerHour == 0 {
		policy.Registration.RegistrationsPerIPPerHour = 3
	}
	if strings.TrimSpace(policy.Provisioning.ProjectName) == "" {
		policy.Provisioning.ProjectName = "Default"
	}
	if strings.TrimSpace(policy.Provisioning.ProjectDescription) == "" {
		policy.Provisioning.ProjectDescription = "Default project"
	}
	policy.Provisioning.PresetID = strings.TrimSpace(policy.Provisioning.PresetID)
	policy.WorkspaceLifecycle.ExpiresAfter = strings.TrimSpace(policy.WorkspaceLifecycle.ExpiresAfter)
	policy.WorkspaceLifecycle.IdleShutdownAfter = strings.TrimSpace(policy.WorkspaceLifecycle.IdleShutdownAfter)
	policy.ManagedLLM.Path = strings.TrimSpace(policy.ManagedLLM.Path)
	policy.ManagedLLM.Configured = policy.ManagedLLM.ConnectionID > 0
	if policy.ManagedLLM.Path == "" {
		policy.ManagedLLM.Path = "/chat/completions"
	}
	policy.ManagedLLM.Models = cleanStringSet(policy.ManagedLLM.Models)
	policy.Capabilities.AllowedApps = cleanStringSet(policy.Capabilities.AllowedApps)
	policy.Capabilities.AllowedModels = cleanStringSet(policy.Capabilities.AllowedModels)
	if len(policy.ManagedLLM.Models) == 0 {
		policy.ManagedLLM.Models = append([]string(nil), policy.Capabilities.AllowedModels...)
	}
	return policy
}

func cleanStringSet(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validateAccessPolicy(policy AccessPolicy) error {
	policy = normalizeAccessPolicy(policy)
	if policy.Registration.Mode != "open" && policy.Registration.Mode != "locked" {
		return errors.New("registration.mode must be open or locked")
	}
	if policy.Registration.RegistrationsPerIPPerHour < 1 || policy.Registration.RegistrationsPerIPPerHour > 1000 {
		return errors.New("registration.registrations_per_ip_per_hour must be between 1 and 1000")
	}
	for name, value := range map[string]int{
		"projects_per_user":           policy.Limits.ProjectsPerUser,
		"agents_per_project":          policy.Limits.AgentsPerProject,
		"running_agents_per_project":  policy.Limits.RunningAgentsPerProject,
		"daily_model_calls":           policy.Limits.DailyModelCalls,
		"daily_tokens":                policy.Limits.DailyTokens,
		"concurrent_llm_requests":     policy.Limits.ConcurrentLLMRequests,
		"global_concurrent_llm_calls": policy.Limits.GlobalConcurrentLLMCalls,
	} {
		if value < 0 {
			return fmt.Errorf("limits.%s must not be negative", name)
		}
	}
	for name, value := range map[string]string{
		"workspace_lifecycle.expires_after":       policy.WorkspaceLifecycle.ExpiresAfter,
		"workspace_lifecycle.idle_shutdown_after": policy.WorkspaceLifecycle.IdleShutdownAfter,
	} {
		if value == "" {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a positive duration", name)
		}
	}
	if policy.ManagedLLM.ConnectionID < 0 {
		return errors.New("managed_llm.connection_id must not be negative")
	}
	if policy.ManagedLLM.ConnectionID > 0 && len(policy.ManagedLLM.Models) == 0 && len(policy.Capabilities.AllowedModels) == 0 {
		return errors.New("managed_llm.models or capabilities.allowed_models is required when a managed connection is selected")
	}
	if policy.ManagedLLM.Path != "" && !strings.HasPrefix(policy.ManagedLLM.Path, "/") {
		return errors.New("managed_llm.path must begin with /")
	}
	return nil
}

func (s *Server) saveAccessPolicy(caller int64, policy AccessPolicy) (AccessPolicy, error) {
	if err := validateAccessPolicy(policy); err != nil {
		return AccessPolicy{}, err
	}
	policy = normalizeAccessPolicy(policy)
	if policy.ManagedLLM.ConnectionID > 0 {
		conn, _, err := s.store.GetConnectionAny(policy.ManagedLLM.ConnectionID)
		if err != nil {
			return AccessPolicy{}, errors.New("managed_llm.connection_id does not exist")
		}
		if conn.Status != "active" {
			return AccessPolicy{}, errors.New("managed LLM connection must be active")
		}
		if s.store.GetPlatformRole(conn.UserID) != PlatformAdmin {
			return AccessPolicy{}, errors.New("managed LLM connection must belong to a platform admin")
		}
		if strings.TrimSpace(conn.ProjectID) != "" {
			return AccessPolicy{}, errors.New("managed LLM connection must be server-scoped")
		}
		app := s.catalog.Get(conn.AppSlug)
		if app == nil || app.Runtime == nil || !strings.EqualFold(app.Runtime.Role, "llm") {
			return AccessPolicy{}, errors.New("managed LLM connection must use an LLM integration")
		}
		if _, err := s.store.db.Exec(`UPDATE connections
			SET credential_management='platform', credential_export_policy='never', auto_mcp=0
			WHERE id=?`, conn.ID); err != nil {
			return AccessPolicy{}, fmt.Errorf("protect managed LLM connection: %w", err)
		}
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return AccessPolicy{}, err
	}
	if err := s.store.SetSetting(accessPolicySettingKey, string(raw)); err != nil {
		return AccessPolicy{}, err
	}
	_ = caller // retained for future audit attribution without changing the API.
	return policy, nil
}

func (s *Server) effectiveRegistrationMode() string {
	policy, err := s.loadAccessPolicy()
	if err != nil {
		return "locked"
	}
	return policy.Registration.Mode
}

func (s *Server) capabilityAllowed(userID int64, name string) bool {
	if s.store.GetPlatformRole(userID) == PlatformAdmin {
		return true
	}
	policy, err := s.loadAccessPolicy()
	if err != nil {
		return false
	}
	switch name {
	case "api_keys":
		return policy.Capabilities.APIKeys
	case "custom_mcp":
		return policy.Capabilities.CustomMCP
	case "provider_management":
		return policy.Capabilities.ProviderManagement
	case "app_installation":
		return policy.Capabilities.AppInstallation
	case "invitations":
		return policy.Capabilities.Invitations
	case "domains":
		return policy.Capabilities.Domains
	case "backups":
		return policy.Capabilities.Backups
	case "realtime_voice":
		return policy.Capabilities.RealtimeVoice
	case "autonomous_scheduling":
		return policy.Capabilities.AutonomousScheduling
	default:
		return false
	}
}

func (s *Server) requireCapability(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.capabilityAllowed(getUserID(r), name) {
		return true
	}
	writeJSONStatus(w, http.StatusForbidden, map[string]any{
		"error": "capability_disabled", "capability": name,
	})
	return false
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (s *Server) managedLLMPolicyForUser(userID int64) (ManagedLLMPolicy, bool) {
	policy, err := s.loadAccessPolicy()
	if err != nil || policy.ManagedLLM.ConnectionID <= 0 {
		return ManagedLLMPolicy{}, false
	}
	return policy.ManagedLLM, true
}

func (s *Server) managedProviderInfoForUser(userID int64) (ProviderInfo, bool) {
	managed, ok := s.managedLLMPolicyForUser(userID)
	if !ok {
		return ProviderInfo{}, false
	}
	models := managed.Models
	if len(models) == 0 {
		return ProviderInfo{}, false
	}
	large, medium, small := models[0], models[0], models[0]
	if len(models) > 1 {
		medium = models[1]
	}
	if len(models) > 2 {
		small = models[2]
	}
	return ProviderInfo{Type: "managed", ModelLarge: large, ModelMedium: medium, ModelSmall: small}, true
}

func (s *Server) accessStateForUser(userID int64, projectID string) map[string]any {
	policy, err := s.loadAccessPolicy()
	if err != nil {
		policy = closedAccessPolicy()
	}
	if s.store.GetPlatformRole(userID) == PlatformAdmin {
		policy.Limits = ResourceAccessPolicy{}
		policy.Capabilities = defaultAccessPolicy(policy.Registration.Mode).Capabilities
	}
	usage, _ := s.store.ManagedLLMUsageForDay(userID, projectID, time.Now().UTC())
	usageState := map[string]any{
		"date": usage.Date, "calls": usage.Calls,
		"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
		"model_calls_remaining": nil, "tokens_remaining": nil,
		"resets_at": time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Format(time.RFC3339),
	}
	if policy.Limits.DailyModelCalls > 0 {
		usageState["model_calls_remaining"] = max64(0, int64(policy.Limits.DailyModelCalls)-usage.Calls)
	}
	if policy.Limits.DailyTokens > 0 {
		usageState["tokens_remaining"] = max64(0, int64(policy.Limits.DailyTokens)-usage.InputTokens-usage.OutputTokens)
	}
	workspace := map[string]any{"project_id": projectID}
	if projectID != "" {
		if project, projectErr := s.store.GetProjectAny(projectID); projectErr == nil && project.ExpiresAt != nil {
			workspace["expires_at"] = project.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	return map[string]any{
		"limits":              policy.Limits,
		"capabilities":        policy.Capabilities,
		"usage":               usageState,
		"workspace_lifecycle": policy.WorkspaceLifecycle,
		"workspace":           workspace,
		"managed_llm": map[string]any{
			"configured": policy.ManagedLLM.ConnectionID > 0,
			"models":     policy.ManagedLLM.Models,
		},
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) startAgentOnDemand(agent *Agent) (*Agent, error) {
	if agent == nil || s.agents.IsRunning(agent.ID) {
		return agent, nil
	}
	policy, err := s.loadAccessPolicy()
	if err != nil || policy.WorkspaceLifecycle.IdleShutdownAfter == "" {
		return agent, nil
	}
	if s.store.GetPlatformRole(agent.UserID) != PlatformAdmin {
		if limit := policy.Limits.RunningAgentsPerProject; limit > 0 && s.store.CountProjectAgents(agent.ProjectID, true) >= limit {
			return nil, errors.New("running agent limit reached")
		}
	}
	providerEnv, err := s.GetAllProviderEnvVars(agent.UserID, agent.ProjectID)
	if err != nil {
		return nil, err
	}
	pool := s.GetProviderPool(agent.UserID, agent.ProjectID)
	if len(pool) == 0 {
		return nil, errors.New("no LLM provider configured")
	}
	if _, err := s.startManagedAgent(agent, providerEnv, pool, s.loadChannelConfigs(agent.ID)...); err != nil && !errors.Is(err, errAgentAlreadyRunning) {
		return nil, err
	}
	_ = s.store.UpdateAgent(agent)
	return s.store.GetAgentByID(agent.ID)
}

func (s *Server) validateAllowedAppInstalls(userID int64, installIDs []int64) error {
	if s.store.GetPlatformRole(userID) == PlatformAdmin || len(installIDs) == 0 {
		return nil
	}
	policy, err := s.loadAccessPolicy()
	if err != nil {
		return err
	}
	if len(policy.Capabilities.AllowedApps) == 0 {
		return nil
	}
	allowed := map[string]bool{}
	for _, slug := range policy.Capabilities.AllowedApps {
		allowed[slug] = true
	}
	for _, installID := range installIDs {
		var slug string
		if err := s.store.db.QueryRow(`SELECT a.name FROM app_installs ai JOIN apps a ON a.id=ai.app_id WHERE ai.id=?`, installID).Scan(&slug); err != nil {
			return errors.New("selected app is unavailable")
		}
		if !allowed[slug] {
			return fmt.Errorf("app %q is not allowed by server policy", slug)
		}
	}
	return nil
}

func (s *Store) GetSettingStrict(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM server_settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) GetConnectionAny(connID int64) (*Connection, string, error) {
	var c Connection
	var encrypted, createdAt string
	var autoMCP int
	err := s.db.QueryRow(`SELECT id,user_id,app_slug,app_name,name,auth_type,encrypted_credentials,status,
		COALESCE(source,'local'),COALESCE(provider_id,0),COALESCE(external_id,''),COALESCE(project_id,''),
		COALESCE(created_via,'integration'),COALESCE(owner_app_install_id,0),COALESCE(credential_management,'user'),
		COALESCE(credential_export_policy,'bound_app'),COALESCE(managed_key,''),COALESCE(auto_mcp,1),created_at
		FROM connections WHERE id=?`, connID).Scan(
		&c.ID, &c.UserID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &encrypted, &c.Status,
		&c.Source, &c.ProviderID, &c.ExternalID, &c.ProjectID, &c.CreatedVia, &c.OwnerAppInstallID,
		&c.CredentialManagement, &c.CredentialExportPolicy, &c.ManagedKey, &autoMCP, &createdAt,
	)
	if err != nil {
		return nil, "", err
	}
	c.AutoMCP = autoMCP != 0
	c.CreatedAt, _ = parseTime(createdAt)
	return &c, encrypted, nil
}

func (s *Store) CountProjectsForUser(userID int64) int {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM projects WHERE user_id=?`, userID).Scan(&count)
	return count
}

func (s *Store) CountProjectAgents(projectID string, runningOnly bool) int {
	query := `SELECT COUNT(*) FROM agents WHERE project_id=? AND COALESCE(kind,'user')='user'`
	if runningOnly {
		query += ` AND status='running'`
	}
	var count int
	_ = s.db.QueryRow(query, projectID).Scan(&count)
	return count
}

// CreateProvisionedUser commits the identity, preferences, private project,
// owner membership, and lifecycle metadata together. Preset materialization
// happens immediately afterwards and is compensatable; no account is exposed
// to login until handleRegister has completed both stages.
func (s *Store) CreateProvisionedUser(email, passwordHash string, policy AccessPolicy) (*User, *Project, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO users(email,password_hash) VALUES(?,?)`, email, passwordHash)
	if err != nil {
		return nil, nil, fmt.Errorf("user exists or db error: %w", err)
	}
	userID, _ := result.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO user_preferences(user_id,interface_level,updated_at)
		VALUES(?,'business',CURRENT_TIMESTAMP)`, userID); err != nil {
		return nil, nil, err
	}
	if userID == 1 {
		if _, err := tx.Exec(`UPDATE users SET role='admin' WHERE id=?`, userID); err != nil {
			return nil, nil, err
		}
	}
	projectID := generateID()
	var expiresAt any
	if raw := policy.WorkspaceLifecycle.ExpiresAfter; raw != "" {
		d, _ := time.ParseDuration(raw)
		expiresAt = time.Now().UTC().Add(d).Format(time.RFC3339)
	}
	if _, err := tx.Exec(`INSERT INTO projects
		(id,user_id,name,description,color,expires_at,provisioning_preset_id)
		VALUES(?,?,?,?,?,?,?)`, projectID, userID, policy.Provisioning.ProjectName,
		policy.Provisioning.ProjectDescription, "#6366f1", expiresAt, policy.Provisioning.PresetID); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(`INSERT INTO project_members(project_id,user_id,role,added_by)
		VALUES(?,?,'owner',?)`, projectID, userID, userID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	return &User{ID: userID, Email: email, Role: map[bool]string{true: string(PlatformAdmin), false: string(PlatformUser)}[userID == 1], CreatedAt: now},
		&Project{ID: projectID, UserID: userID, Name: policy.Provisioning.ProjectName,
			Description: policy.Provisioning.ProjectDescription, Color: "#6366f1", CreatedAt: now}, nil
}

// materializeProvisioningPreset reuses the existing project-preset compiler,
// but never installs software or starts a process during signup. Operators
// install approved apps once; registration only creates deterministic rows and
// bindings. Any failure is returned so the caller can remove the new account.
func (s *Server) materializeProvisioningPreset(userID int64, projectID string, policy AccessPolicy) error {
	if policy.Provisioning.PresetID == "" {
		return nil
	}
	preview, err := s.compileProjectPresetPreview(
		context.Background(), userID, projectID,
		ProjectPresetPreviewRequest{PresetID: policy.Provisioning.PresetID, Description: policy.Provisioning.ProjectDescription},
	)
	if err != nil {
		return err
	}
	project, err := s.store.GetProjectAny(projectID)
	if err != nil {
		return err
	}
	if err := s.store.UpdateProjectAny(projectID, preview.Project["name"], preview.Project["description"], project.Color); err != nil {
		return err
	}
	if err := s.mergeProjectPresetDashboardLayout(userID, projectID, preview.Layout); err != nil {
		return err
	}
	for _, agent := range preview.Agents {
		payload := map[string]any{
			"name": agent.Name, "directive": agent.Directive, "mode": agent.Mode,
			"project_id": projectID, "start": false, "unconscious": agent.Unconscious,
			"bound_app_install_ids": agent.AppInstallIDs,
		}
		raw, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-ID", fmt.Sprint(userID))
		rec := httptest.NewRecorder()
		s.handleCreateInstance(rec, req)
		if rec.Code < 200 || rec.Code >= 300 {
			return fmt.Errorf("provision agent %q: %s", agent.Name, strings.TrimSpace(rec.Body.String()))
		}
	}
	return nil
}

func managedLLMUsageDate(now time.Time) string { return now.UTC().Format("2006-01-02") }

func (s *Store) ManagedLLMUsageForDay(userID int64, projectID string, now time.Time) (ManagedLLMUsage, error) {
	usage := ManagedLLMUsage{Date: managedLLMUsageDate(now)}
	err := s.db.QueryRow(`SELECT calls,input_tokens,output_tokens FROM managed_llm_usage
		WHERE usage_date=? AND user_id=? AND project_id=?`, usage.Date, userID, projectID).
		Scan(&usage.Calls, &usage.InputTokens, &usage.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return usage, nil
	}
	return usage, err
}

func (s *Store) ManagedLLMAggregateForDay(now time.Time) (ManagedLLMUsage, error) {
	usage := ManagedLLMUsage{Date: managedLLMUsageDate(now)}
	err := s.db.QueryRow(`SELECT COALESCE(SUM(calls),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0)
		FROM managed_llm_usage WHERE usage_date=?`, usage.Date).
		Scan(&usage.Calls, &usage.InputTokens, &usage.OutputTokens)
	return usage, err
}

func (s *Store) RecordManagedLLMUsage(userID int64, projectID string, calls, inputTokens, outputTokens int64, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO managed_llm_usage
		(usage_date,user_id,project_id,calls,input_tokens,output_tokens,updated_at)
		VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(usage_date,user_id,project_id) DO UPDATE SET
		calls=managed_llm_usage.calls+excluded.calls,
		input_tokens=managed_llm_usage.input_tokens+excluded.input_tokens,
		output_tokens=managed_llm_usage.output_tokens+excluded.output_tokens,
		updated_at=CURRENT_TIMESTAMP`, managedLLMUsageDate(now), userID, projectID, calls, inputTokens, outputTokens)
	return err
}
