package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	providerAuthTypeAPIKey       = "api_key"
	providerAuthTypeDeviceCode   = "oauth_device_code"
	providerAuthStatusPending    = "pending"
	providerAuthStatusConnected  = "connected"
	providerAuthStatusExpired    = "expired"
	providerAuthStatusFailed     = "failed"
	openAICodexAuthProvider      = "openai-codex"
	openAICodexIssuer            = "https://auth.openai.com"
	openAICodexClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAICodexTokenURL          = "https://auth.openai.com/oauth/token"
	openAICodexBackendAPIBaseURL = "https://chatgpt.com/backend-api/codex"
)

type providerAuthStartRequest struct {
	ProviderTypeID int64  `json:"provider_type_id"`
	ProjectID      string `json:"project_id"`
}

type providerAuthSession struct {
	ID             string
	UserID         int64
	ProviderTypeID int64
	ProviderType   ProviderType
	ProjectID      string
	DeviceAuthID   string
	UserCode       string
	ExpiresAt      time.Time
	Interval       int
	CreatedAt      time.Time
}

type providerAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*providerAuthSession
}

var globalProviderAuthSessions = &providerAuthSessionStore{sessions: map[string]*providerAuthSession{}}

func (s *providerAuthSessionStore) put(session *providerAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
}

func (s *providerAuthSessionStore) get(id string) (*providerAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	return session, ok
}

func (s *providerAuthSessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

type providerAuthDriver interface {
	Start(ctx context.Context, pt ProviderType, userID int64, projectID string) (*providerAuthSession, map[string]any, error)
	Poll(ctx context.Context, session *providerAuthSession) (*providerAuthPollResult, error)
	Refresh(ctx context.Context, state map[string]any) (map[string]any, error)
	Status(state map[string]any) map[string]any
	Logout(state map[string]any) map[string]any
	SmokeTest(ctx context.Context, state map[string]any, opts providerAuthSmokeOptions) ProviderTestResult
}

type providerAuthSmokeOptions struct {
	Model        string
	Computer     bool
	ComputerMode string
}

type providerAuthPollResult struct {
	Status     string
	ProviderID int64
	State      map[string]any
	Account    map[string]any
	Error      string
}

func providerAuthDriverFor(provider string) providerAuthDriver {
	switch provider {
	case openAICodexAuthProvider:
		return openAICodexAuthDriver{}
	default:
		return nil
	}
}

func (s *Server) handleStartProviderAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	var body providerAuthStartRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.ProviderTypeID == 0 {
		http.Error(w, "provider_type_id required", http.StatusBadRequest)
		return
	}
	pt, err := s.store.GetProviderType(body.ProviderTypeID)
	if err != nil {
		http.Error(w, "provider type not found", http.StatusNotFound)
		return
	}
	if pt.AuthType != providerAuthTypeDeviceCode {
		http.Error(w, "provider does not use device-code auth", http.StatusBadRequest)
		return
	}
	driver := providerAuthDriverFor(pt.AuthProvider)
	if driver == nil {
		http.Error(w, "provider auth driver not available", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	session, response, err := driver.Start(ctx, *pt, userID, body.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	globalProviderAuthSessions.put(session)
	writeJSON(w, response)
}

func (s *Server) handlePollProviderAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/providers/auth/")
	sessionID = strings.Trim(sessionID, "/")
	if sessionID == "" || sessionID == "start" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	session, ok := globalProviderAuthSessions.get(sessionID)
	if !ok || session.UserID != getUserID(r) {
		http.Error(w, "auth session not found", http.StatusNotFound)
		return
	}
	if time.Now().After(session.ExpiresAt) {
		globalProviderAuthSessions.delete(sessionID)
		writeJSON(w, map[string]any{"status": providerAuthStatusExpired})
		return
	}
	driver := providerAuthDriverFor(session.ProviderType.AuthProvider)
	if driver == nil {
		http.Error(w, "provider auth driver not available", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := driver.Poll(ctx, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if result.Status == providerAuthStatusPending {
		writeJSON(w, map[string]any{
			"status":            providerAuthStatusPending,
			"next_poll_seconds": session.Interval,
		})
		return
	}
	if result.Status != providerAuthStatusConnected {
		writeJSON(w, map[string]any{
			"status": result.Status,
			"error":  result.Error,
		})
		return
	}
	provider, err := s.upsertProviderAuthState(session.UserID, session.ProviderType, session.ProjectID, result.State)
	if err != nil {
		http.Error(w, "failed to persist provider auth", http.StatusInternalServerError)
		return
	}
	globalProviderAuthSessions.delete(sessionID)
	writeJSON(w, map[string]any{
		"status": providerAuthStatusConnected,
		"provider": map[string]any{
			"id":             provider.ID,
			"type":           provider.Type,
			"name":           provider.Name,
			"project_id":     provider.ProjectID,
			"auth_status":    providerAuthStatusConnected,
			"runtime_status": session.ProviderType.RuntimeStatus,
		},
		"account": result.Account,
	})
}

func (s *Server) handleProviderAuthAction(w http.ResponseWriter, r *http.Request, action string) {
	userID := getUserID(r)
	idPart := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/providers/"), "/auth/"+action)
	providerID, err := atoi64(strings.Trim(idPart, "/"))
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}
	provider, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	pt, _ := s.store.GetProviderType(provider.ProviderTypeID)
	authProvider := providerKeyFromName(provider.Name)
	if pt != nil && pt.AuthProvider != "" {
		authProvider = pt.AuthProvider
	}
	driver := providerAuthDriverFor(authProvider)
	if driver == nil {
		http.Error(w, "provider auth driver not available", http.StatusBadRequest)
		return
	}
	state := map[string]any{}
	if plaintext, err := Decrypt(s.secret, encData); err == nil {
		_ = json.Unmarshal([]byte(plaintext), &state)
	}
	switch action {
	case "status":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		status := driver.Status(state)
		if pt != nil {
			status["runtime_status"] = pt.RuntimeStatus
		}
		writeJSON(w, status)
	case "refresh":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		refreshed, err := driver.Refresh(ctx, state)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.saveProviderAuthState(userID, provider, refreshed); err != nil {
			http.Error(w, "failed to persist provider auth", http.StatusInternalServerError)
			return
		}
		writeJSON(w, driver.Status(refreshed))
	case "logout":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		loggedOut := driver.Logout(state)
		if err := s.saveProviderAuthState(userID, provider, loggedOut); err != nil {
			http.Error(w, "failed to persist provider auth", http.StatusInternalServerError)
			return
		}
		writeJSON(w, driver.Status(loggedOut))
	case "smoke-test":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
		defer cancel()
		opts := providerAuthSmokeOptions{
			Model:        strings.TrimSpace(r.URL.Query().Get("model")),
			Computer:     truthyQuery(r.URL.Query().Get("computer")),
			ComputerMode: strings.TrimSpace(r.URL.Query().Get("computer_mode")),
		}
		writeJSON(w, driver.SmokeTest(ctx, state, opts))
	case "runtime-token":
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
			return
		}
		if authProvider != openAICodexAuthProvider {
			http.Error(w, "runtime tokens are not available for this provider", http.StatusBadRequest)
			return
		}
		force := r.URL.Query().Get("force") == "1"
		if force || codexStateNeedsRefresh(state, 10*time.Minute) {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			refreshed, err := driver.Refresh(ctx, state)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if err := s.saveProviderAuthState(userID, provider, refreshed); err != nil {
				http.Error(w, "failed to persist provider auth", http.StatusInternalServerError)
				return
			}
			state = refreshed
		}
		token := stringFromNested(state, "credentials", "access_token")
		if strings.TrimSpace(token) == "" {
			http.Error(w, "OpenAI Codex auth is missing access_token", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{
			"provider":     openAICodexAuthProvider,
			"token_type":   "Bearer",
			"access_token": token,
			"expires_at":   stringFromNested(state, "credentials", "expires_at"),
		})
	default:
		http.Error(w, "unknown auth action", http.StatusNotFound)
	}
}

func codexStateNeedsRefresh(state map[string]any, skew time.Duration) bool {
	exp, ok := expiryFromState(state)
	if !ok {
		return false
	}
	return time.Until(exp) <= skew
}

func truthyQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (s *Server) upsertProviderAuthState(userID int64, pt ProviderType, projectID string, state map[string]any) (*Provider, error) {
	encrypted, err := marshalEncryptProviderState(s.secret, state)
	if err != nil {
		return nil, err
	}
	if existing, _, err := s.store.FindProviderByTypeForProject(userID, pt.ID, projectID); err == nil {
		if err := s.store.UpdateProvider(userID, existing.ID, pt.Type, pt.Name, encrypted); err != nil {
			return nil, err
		}
		existing.Type = pt.Type
		existing.Name = pt.Name
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s.store.CreateProvider(userID, pt.ID, pt.Type, pt.Name, encrypted, projectID)
}

func (s *Server) saveProviderAuthState(userID int64, provider *Provider, state map[string]any) error {
	encrypted, err := marshalEncryptProviderState(s.secret, state)
	if err != nil {
		return err
	}
	return s.store.UpdateProvider(userID, provider.ID, provider.Type, provider.Name, encrypted)
}

func marshalEncryptProviderState(secret []byte, state map[string]any) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return Encrypt(secret, string(raw))
}

type openAICodexAuthDriver struct{}

type looseJSONInt int

func (v *looseJSONInt) UnmarshalJSON(raw []byte) error {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		*v = looseJSONInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	*v = looseJSONInt(parsed)
	return nil
}

func (openAICodexAuthDriver) Start(ctx context.Context, pt ProviderType, userID int64, projectID string) (*providerAuthSession, map[string]any, error) {
	var payload struct {
		UserCode     string       `json:"user_code"`
		DeviceAuthID string       `json:"device_auth_id"`
		ExpiresIn    looseJSONInt `json:"expires_in"`
		Interval     looseJSONInt `json:"interval"`
	}
	if err := postProviderAuthJSON(ctx, openAICodexIssuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": openAICodexClientID}, &payload); err != nil {
		return nil, nil, err
	}
	if payload.UserCode == "" || payload.DeviceAuthID == "" {
		return nil, nil, fmt.Errorf("OpenAI Codex device auth response was incomplete")
	}
	expiresIn := int(payload.ExpiresIn)
	interval := int(payload.Interval)
	if expiresIn <= 0 {
		expiresIn = 15 * 60
	}
	if interval <= 0 {
		interval = 5
	}
	session := &providerAuthSession{
		ID:             "pauth_" + generateToken(18),
		UserID:         userID,
		ProviderTypeID: pt.ID,
		ProviderType:   pt,
		ProjectID:      projectID,
		DeviceAuthID:   payload.DeviceAuthID,
		UserCode:       payload.UserCode,
		ExpiresAt:      time.Now().Add(time.Duration(expiresIn) * time.Second),
		Interval:       interval,
		CreatedAt:      time.Now(),
	}
	return session, map[string]any{
		"session_id":         session.ID,
		"provider":           openAICodexAuthProvider,
		"method":             providerAuthTypeDeviceCode,
		"verification_uri":   openAICodexIssuer + "/codex/device",
		"user_code":          payload.UserCode,
		"expires_at":         session.ExpiresAt.Format(time.RFC3339),
		"interval_seconds":   interval,
		"runtime_status":     pt.RuntimeStatus,
		"provider_type_id":   pt.ID,
		"provider_type_name": pt.Name,
	}, nil
}

func (openAICodexAuthDriver) Poll(ctx context.Context, session *providerAuthSession) (*providerAuthPollResult, error) {
	reqBody := map[string]string{
		"device_auth_id": session.DeviceAuthID,
		"user_code":      session.UserCode,
	}
	var codeResp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	status, body, err := postProviderAuthJSONStatus(ctx, openAICodexIssuer+"/api/accounts/deviceauth/token", reqBody, &codeResp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return &providerAuthPollResult{Status: providerAuthStatusPending}, nil
	}
	if status < 200 || status >= 300 {
		return &providerAuthPollResult{Status: providerAuthStatusFailed, Error: strings.TrimSpace(string(body))}, nil
	}
	if codeResp.AuthorizationCode == "" || codeResp.CodeVerifier == "" {
		return &providerAuthPollResult{Status: providerAuthStatusFailed, Error: "OpenAI Codex device auth response was incomplete"}, nil
	}
	tokens, err := exchangeOpenAICodexCode(ctx, codeResp.AuthorizationCode, codeResp.CodeVerifier)
	if err != nil {
		return nil, err
	}
	state := buildOpenAICodexProviderState(tokens, "device_code")
	account := stateMap(state, "account")
	return &providerAuthPollResult{
		Status:  providerAuthStatusConnected,
		State:   state,
		Account: account,
	}, nil
}

func (openAICodexAuthDriver) Refresh(ctx context.Context, state map[string]any) (map[string]any, error) {
	creds := stateMap(state, "credentials")
	refreshToken, _ := creds["refresh_token"].(string)
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("OpenAI Codex auth is missing refresh_token")
	}
	tokens, err := refreshOpenAICodexTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if nextRefresh, _ := tokens["refresh_token"].(string); strings.TrimSpace(nextRefresh) == "" {
		tokens["refresh_token"] = refreshToken
	}
	next := buildOpenAICodexProviderState(tokens, "refresh_token")
	if account := stateMap(state, "account"); len(account) > 0 {
		next["account"] = account
	}
	return next, nil
}

func (openAICodexAuthDriver) Status(state map[string]any) map[string]any {
	creds := stateMap(state, "credentials")
	accessToken, _ := creds["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		return map[string]any{"auth_status": "disconnected"}
	}
	status := providerAuthStatusConnected
	if exp, ok := expiryFromState(state); ok && time.Now().After(exp) {
		status = "needs_refresh"
	}
	return map[string]any{
		"auth_status":  status,
		"provider":     openAICodexAuthProvider,
		"auth_type":    providerAuthTypeDeviceCode,
		"account":      stateMap(state, "account"),
		"expires_at":   stringFromNested(state, "credentials", "expires_at"),
		"last_refresh": stringFromNested(state, "credentials", "last_refresh"),
	}
}

func (openAICodexAuthDriver) Logout(state map[string]any) map[string]any {
	return map[string]any{
		"auth": map[string]any{
			"type":     providerAuthTypeDeviceCode,
			"provider": openAICodexAuthProvider,
			"mode":     "chatgpt",
			"source":   "logout",
		},
		"account": stateMap(state, "account"),
		"runtime": map[string]any{"base_url": openAICodexBackendAPIBaseURL},
	}
}

func (openAICodexAuthDriver) SmokeTest(ctx context.Context, state map[string]any, opts providerAuthSmokeOptions) ProviderTestResult {
	accessToken := stringFromNested(state, "credentials", "access_token")
	if strings.TrimSpace(accessToken) == "" {
		return ProviderTestResult{OK: false, Error: "OpenAI Codex auth is missing access_token"}
	}
	if exp, ok := expiryFromState(state); ok && time.Now().After(exp) {
		return ProviderTestResult{OK: false, Error: "OpenAI Codex access token is expired; refresh auth first"}
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = "gpt-5.5"
	}

	textPayload := map[string]any{
		"model":        model,
		"instructions": "You are validating a provider connection. Answer exactly with the requested text.",
		"input": []map[string]string{
			{"role": "user", "content": "Reply with exactly: apteva-codex-smoke-ok"},
		},
		"store":  false,
		"stream": true,
	}
	t0 := time.Now()
	textResp, err := runOpenAICodexSmokeRequest(ctx, accessToken, textPayload)
	if err != nil {
		return ProviderTestResult{
			OK:         false,
			LatencyMS:  time.Since(t0).Milliseconds(),
			StatusCode: textResp.StatusCode,
			Error:      err.Error(),
			Model:      model,
		}
	}
	if !strings.Contains(textResp.Text, "apteva-codex-smoke-ok") {
		return ProviderTestResult{
			OK:         false,
			LatencyMS:  time.Since(t0).Milliseconds(),
			StatusCode: textResp.StatusCode,
			Error:      fmt.Sprintf("text stream missing marker, got %q", textResp.Text),
			Model:      model,
		}
	}

	toolPayload := map[string]any{
		"model":        model,
		"instructions": "You are validating native tool calling. Call the provided function exactly once with marker apteva-codex-tool-ok. Do not answer in prose.",
		"input": []map[string]string{
			{"role": "user", "content": "Call smoke_echo with marker apteva-codex-tool-ok."},
		},
		"tools": []map[string]any{{
			"type":        "function",
			"name":        "smoke_echo",
			"description": "Echo a marker for provider compatibility testing.",
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"marker": map[string]string{
						"type":        "string",
						"description": "The exact marker requested by the user.",
					},
				},
				"required": []string{"marker"},
			},
		}},
		"store":  false,
		"stream": true,
	}
	toolResp, err := runOpenAICodexSmokeRequest(ctx, accessToken, toolPayload)
	if err != nil {
		return ProviderTestResult{
			OK:               false,
			LatencyMS:        time.Since(t0).Milliseconds(),
			StatusCode:       toolResp.StatusCode,
			Error:            err.Error(),
			Model:            model,
			ResponseText:     textResp.Text,
			PromptTokens:     textResp.InputTokens,
			CompletionTokens: textResp.OutputTokens,
			CachedTokens:     textResp.CachedTokens,
		}
	}
	if len(toolResp.ToolCalls) == 0 {
		return ProviderTestResult{
			OK:               false,
			LatencyMS:        time.Since(t0).Milliseconds(),
			StatusCode:       toolResp.StatusCode,
			Error:            fmt.Sprintf("native tool-call stream missing function_call; text=%q", toolResp.Text),
			Model:            model,
			ResponseText:     textResp.Text,
			PromptTokens:     textResp.InputTokens + toolResp.InputTokens,
			CompletionTokens: textResp.OutputTokens + toolResp.OutputTokens,
			CachedTokens:     textResp.CachedTokens + toolResp.CachedTokens,
		}
	}
	firstTool := toolResp.ToolCalls[0]
	if firstTool.Name != "smoke_echo" || !strings.Contains(firstTool.Arguments, "apteva-codex-tool-ok") {
		return ProviderTestResult{
			OK:               false,
			LatencyMS:        time.Since(t0).Milliseconds(),
			StatusCode:       toolResp.StatusCode,
			Error:            fmt.Sprintf("unexpected tool call %s %s", firstTool.Name, firstTool.Arguments),
			Model:            model,
			ResponseText:     textResp.Text,
			PromptTokens:     textResp.InputTokens + toolResp.InputTokens,
			CompletionTokens: textResp.OutputTokens + toolResp.OutputTokens,
			CachedTokens:     textResp.CachedTokens + toolResp.CachedTokens,
			ToolCallCount:    len(toolResp.ToolCalls),
			ToolName:         firstTool.Name,
			ToolArguments:    firstTool.Arguments,
		}
	}

	result := ProviderTestResult{
		OK:               true,
		LatencyMS:        time.Since(t0).Milliseconds(),
		StatusCode:       toolResp.StatusCode,
		Model:            model,
		ResponseText:     textResp.Text,
		PromptTokens:     textResp.InputTokens + toolResp.InputTokens,
		CompletionTokens: textResp.OutputTokens + toolResp.OutputTokens,
		CachedTokens:     textResp.CachedTokens + toolResp.CachedTokens,
		ToolCallCount:    len(toolResp.ToolCalls),
		ToolName:         firstTool.Name,
		ToolArguments:    firstTool.Arguments,
	}
	if !opts.Computer {
		return result
	}

	computerMode := strings.ToLower(strings.TrimSpace(opts.ComputerMode))
	if computerMode == "" {
		computerMode = "native"
	}
	var computerResp openAICodexSmokeResponse
	switch computerMode {
	case "custom":
		computerResp, err = runOpenAICodexCustomVisionSmoke(ctx, accessToken, model)
		result.PromptTokens += computerResp.InputTokens
		result.CompletionTokens += computerResp.OutputTokens
		result.CachedTokens += computerResp.CachedTokens
		result.ResponseText = strings.TrimSpace(result.ResponseText + "\n" + computerResp.Text)
		result.VisionText = computerResp.Text
		if err != nil {
			result.OK = false
			result.StatusCode = computerResp.StatusCode
			result.Error = err.Error()
			return result
		}
		if !strings.Contains(strings.ToLower(computerResp.Text), "apteva-vision-red-left") {
			result.OK = false
			result.StatusCode = computerResp.StatusCode
			result.Error = fmt.Sprintf("custom vision smoke missing marker, got %q", computerResp.Text)
			return result
		}
	case "native":
		computerResp, err = runOpenAICodexNativeComputerSmoke(ctx, accessToken, model)
		result.PromptTokens += computerResp.InputTokens
		result.CompletionTokens += computerResp.OutputTokens
		result.CachedTokens += computerResp.CachedTokens
		result.ComputerCallCount = len(computerResp.ComputerCalls)
		if len(computerResp.ComputerCalls) > 0 {
			result.ComputerAction = computerResp.ComputerCalls[0].Action
		}
		if err != nil {
			result.OK = false
			result.StatusCode = computerResp.StatusCode
			result.Error = err.Error()
			return result
		}
		if len(computerResp.ComputerCalls) == 0 {
			result.OK = false
			result.StatusCode = computerResp.StatusCode
			result.Error = fmt.Sprintf("native computer smoke missing computer_call; text=%q", computerResp.Text)
			return result
		}
	default:
		result.OK = false
		result.Error = "computer_mode must be native or custom"
		return result
	}
	result.LatencyMS = time.Since(t0).Milliseconds()
	return result
}

func runOpenAICodexNativeComputerSmoke(ctx context.Context, accessToken, model string) (openAICodexSmokeResponse, error) {
	payload := map[string]any{
		"model":        model,
		"instructions": "You are validating native computer-use support. Use the provided computer tool exactly once to request a screenshot. Do not answer in prose.",
		"input": []map[string]string{
			{"role": "user", "content": "Request a browser screenshot using the computer tool."},
		},
		"tools": []map[string]any{{
			"type": "computer",
		}},
		"store":  false,
		"stream": true,
	}
	resp, err := runOpenAICodexSmokeRequest(ctx, accessToken, payload)
	if err != nil {
		return resp, err
	}
	if len(resp.ComputerCalls) == 0 {
		return resp, nil
	}
	return resp, nil
}

func runOpenAICodexCustomVisionSmoke(ctx context.Context, accessToken, model string) (openAICodexSmokeResponse, error) {
	imageURL, err := codexSmokeVisionImageURL()
	if err != nil {
		return openAICodexSmokeResponse{}, err
	}
	payload := map[string]any{
		"model":        model,
		"instructions": "You are validating image understanding for a custom function-tool fallback. Inspect the attached tool-result image. If the left square is red, reply exactly: apteva-vision-red-left",
		"input": []map[string]any{
			{
				"type":    "message",
				"role":    "user",
				"content": "A previous screenshot tool result is attached below. Read the image and reply with the requested marker.",
			},
			{
				"type":      "function_call",
				"call_id":   "call_codex_vision_smoke",
				"name":      "vision_screenshot",
				"arguments": `{"action":"screenshot"}`,
			},
			{
				"type":    "function_call_output",
				"call_id": "call_codex_vision_smoke",
				"output": []map[string]any{
					{"type": "input_text", "text": "Screenshot attached."},
					{"type": "input_image", "image_url": imageURL, "detail": "low"},
				},
			},
		},
		"tools": []map[string]any{{
			"type":        "function",
			"name":        "vision_screenshot",
			"description": "Return a browser screenshot.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"action": map[string]string{"type": "string"}},
				"required":   []string{"action"},
			},
		}},
		"store":  false,
		"stream": true,
	}
	return runOpenAICodexSmokeRequest(ctx, accessToken, payload)
}

func codexSmokeVisionImageURL() (string, error) {
	img := image.NewRGBA(image.Rect(0, 0, 160, 80))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(20, 20, 70, 70), &image.Uniform{C: color.RGBA{R: 230, G: 32, B: 32, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(90, 20, 140, 70), &image.Uniform{C: color.RGBA{R: 32, G: 72, B: 230, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode vision smoke image: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

type openAICodexSmokeResponse struct {
	StatusCode    int
	Text          string
	InputTokens   int
	OutputTokens  int
	CachedTokens  int
	ToolCalls     []openAICodexSmokeToolCall
	ComputerCalls []openAICodexSmokeComputerCall
}

type openAICodexSmokeToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type openAICodexSmokeComputerCall struct {
	ID     string
	Action string
	Raw    string
}

func runOpenAICodexSmokeRequest(ctx context.Context, accessToken string, payload map[string]any) (openAICodexSmokeResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return openAICodexSmokeResponse{}, fmt.Errorf("build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAICodexBackendAPIBaseURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		return openAICodexSmokeResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return openAICodexSmokeResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	parsed := parseOpenAICodexSmokeBody(body)
	parsed.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parsed, fmt.Errorf("HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}
	return parsed, nil
}

func parseOpenAICodexSmokeBody(body []byte) openAICodexSmokeResponse {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err == nil {
		return parseOpenAICodexResponseObject(data)
	}
	return parseOpenAICodexSmokeSSE(string(body))
}

func parseOpenAICodexResponseObject(data map[string]any) openAICodexSmokeResponse {
	out := openAICodexSmokeResponse{Text: extractOpenAICodexTextFromMap(data)}
	if usage, ok := data["usage"].(map[string]any); ok {
		out.InputTokens = codexIntFromJSONAny(usage["input_tokens"])
		out.OutputTokens = codexIntFromJSONAny(usage["output_tokens"])
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			out.CachedTokens = codexIntFromJSONAny(details["cached_tokens"])
		}
	}
	output, _ := data["output"].([]any)
	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch obj["type"] {
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, openAICodexSmokeToolCall{
				ID:        codexStringFromAny(obj["call_id"]),
				Name:      codexStringFromAny(obj["name"]),
				Arguments: codexStringFromAny(obj["arguments"]),
			})
		case "computer_call":
			out.ComputerCalls = append(out.ComputerCalls, codexComputerCallFromMap(obj))
		}
	}
	return out
}

func parseOpenAICodexSmokeSSE(body string) openAICodexSmokeResponse {
	type pendingTool struct {
		id   string
		name string
		args strings.Builder
	}
	var out openAICodexSmokeResponse
	var textDelta strings.Builder
	finalText := ""
	pending := map[string]*pendingTool{}

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		eventType := codexStringFromAny(event["type"])
		switch eventType {
		case "response.output_text.delta":
			if delta := codexStringFromAny(event["delta"]); delta != "" {
				textDelta.WriteString(delta)
			}
		case "response.output_text.done":
			if text := codexStringFromAny(event["text"]); text != "" {
				finalText = text
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				itemID := codexStringFromAny(item["id"])
				pending[itemID] = &pendingTool{
					id:   codexStringFromAny(item["call_id"]),
					name: codexStringFromAny(item["name"]),
				}
			}
		case "response.function_call_arguments.delta":
			itemID := codexStringFromAny(event["item_id"])
			if pt := pending[itemID]; pt != nil {
				pt.args.WriteString(codexStringFromAny(event["delta"]))
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			switch item["type"] {
			case "function_call":
				itemID := codexStringFromAny(item["id"])
				pt := pending[itemID]
				call := openAICodexSmokeToolCall{
					ID:        codexStringFromAny(item["call_id"]),
					Name:      codexStringFromAny(item["name"]),
					Arguments: codexStringFromAny(item["arguments"]),
				}
				if pt != nil {
					if call.ID == "" {
						call.ID = pt.id
					}
					if call.Name == "" {
						call.Name = pt.name
					}
					if call.Arguments == "" {
						call.Arguments = pt.args.String()
					}
					delete(pending, itemID)
				}
				out.ToolCalls = append(out.ToolCalls, call)
			case "computer_call":
				out.ComputerCalls = append(out.ComputerCalls, codexComputerCallFromMap(item))
			}
		case "response.completed":
			response, _ := event["response"].(map[string]any)
			if finalText == "" {
				finalText = extractOpenAICodexTextFromMap(response)
			}
			if usage, ok := response["usage"].(map[string]any); ok {
				out.InputTokens = codexIntFromJSONAny(usage["input_tokens"])
				out.OutputTokens = codexIntFromJSONAny(usage["output_tokens"])
				if details, ok := usage["input_tokens_details"].(map[string]any); ok {
					out.CachedTokens = codexIntFromJSONAny(details["cached_tokens"])
				}
			}
		}
	}

	if finalText != "" {
		out.Text = strings.TrimSpace(finalText)
	} else {
		out.Text = strings.TrimSpace(textDelta.String())
	}
	return out
}

func codexComputerCallFromMap(item map[string]any) openAICodexSmokeComputerCall {
	call := openAICodexSmokeComputerCall{ID: codexStringFromAny(item["call_id"])}
	if action, ok := item["action"].(map[string]any); ok {
		call.Action = codexStringFromAny(action["type"])
		call.Raw = codexJSONSummary(action)
		return call
	}
	if actions, ok := item["actions"].([]any); ok && len(actions) > 0 {
		if first, ok := actions[0].(map[string]any); ok {
			call.Action = codexStringFromAny(first["type"])
		}
		call.Raw = codexJSONSummary(actions)
		return call
	}
	call.Raw = codexJSONSummary(item)
	return call
}

func codexJSONSummary(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 400 {
		return string(b[:400]) + "..."
	}
	return string(b)
}

func codexStringFromAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

func codexIntFromJSONAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func exchangeOpenAICodexCode(ctx context.Context, code, verifier string) (map[string]any, error) {
	return postFormForTokens(ctx, openAICodexTokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  openAICodexIssuer + "/deviceauth/callback",
		"client_id":     openAICodexClientID,
		"code_verifier": verifier,
	})
}

func refreshOpenAICodexTokens(ctx context.Context, refreshToken string) (map[string]any, error) {
	return postFormForTokens(ctx, openAICodexTokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     openAICodexClientID,
	})
}

func buildOpenAICodexProviderState(tokens map[string]any, source string) map[string]any {
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)
	expiresAt := ""
	if exp, ok := jwtExpiry(accessToken); ok {
		expiresAt = exp.Format(time.RFC3339)
	}
	claims := jwtClaims(accessToken)
	account := map[string]any{}
	for _, key := range []string{"sub", "email"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			if key == "sub" {
				account["id"] = v
			} else {
				account[key] = v
			}
		}
	}
	return map[string]any{
		"auth": map[string]any{
			"type":     providerAuthTypeDeviceCode,
			"provider": openAICodexAuthProvider,
			"mode":     "chatgpt",
			"source":   source,
		},
		"credentials": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"expires_at":    expiresAt,
			"last_refresh":  time.Now().UTC().Format(time.RFC3339),
		},
		"account": account,
		"runtime": map[string]any{"base_url": openAICodexBackendAPIBaseURL},
	}
}

func extractOpenAICodexOutputText(body []byte) string {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return extractOpenAICodexSSEText(body)
	}
	if text, _ := data["output_text"].(string); strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	output, _ := data["output"].([]any)
	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, _ := obj["content"].([]any)
		for _, part := range content {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := partObj["text"].(string); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func extractOpenAICodexSSEText(body []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		if delta, _ := event["delta"].(string); delta != "" {
			b.WriteString(delta)
			continue
		}
		if text, _ := event["text"].(string); text != "" {
			b.WriteString(text)
			continue
		}
		if response, ok := event["response"].(map[string]any); ok {
			if text := extractOpenAICodexTextFromMap(response); text != "" {
				return text
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func extractOpenAICodexTextFromMap(data map[string]any) string {
	if text, _ := data["output_text"].(string); strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	output, _ := data["output"].([]any)
	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, _ := obj["content"].([]any)
		for _, part := range content {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := partObj["text"].(string); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func postProviderAuthJSON(ctx context.Context, url string, in any, out any) error {
	status, body, err := postProviderAuthJSONStatus(ctx, url, in, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("request failed with status %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

func postProviderAuthJSONStatus(ctx context.Context, url string, in any, out any) (int, []byte, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, body, err
		}
	}
	return resp.StatusCode, body, nil
}

func postFormForTokens(ctx context.Context, url string, fields map[string]string) (map[string]any, error) {
	form := neturl.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	accessToken, _ := payload["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return payload, nil
}

func stateMap(state map[string]any, key string) map[string]any {
	v, ok := state[key].(map[string]any)
	if ok {
		return v
	}
	return map[string]any{}
}

func stringFromNested(state map[string]any, outer, inner string) string {
	if v, ok := stateMap(state, outer)[inner].(string); ok {
		return v
	}
	return ""
}

func expiryFromState(state map[string]any) (time.Time, bool) {
	raw := stringFromNested(state, "credentials", "expires_at")
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

func jwtExpiry(token string) (time.Time, bool) {
	claims := jwtClaims(token)
	switch exp := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0), true
	case int64:
		return time.Unix(exp, 0), true
	case json.Number:
		n, err := exp.Int64()
		return time.Unix(n, 0), err == nil
	default:
		return time.Time{}, false
	}
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return map[string]any{}
	}
	return claims
}
