package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

const (
	connectionAuthTypeDeviceCode = "oauth_device_code"

	integrationOpenAICodexSlug              = "openai-codex"
	integrationOpenAICodexIssuer            = "https://auth.openai.com"
	integrationOpenAICodexClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	integrationOpenAICodexTokenURL          = "https://auth.openai.com/oauth/token"
	integrationOpenAICodexBackendAPIBaseURL = "https://chatgpt.com/backend-api/codex"
)

type connectionDeviceAuthSession struct {
	ID           string
	UserID       int64
	ConnectionID int64
	AppSlug      string
	DeviceAuthID string
	UserCode     string
	ExpiresAt    time.Time
	Interval     int
	CreatedAt    time.Time
}

type connectionDeviceAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*connectionDeviceAuthSession
}

var globalConnectionDeviceAuthSessions = &connectionDeviceAuthSessionStore{sessions: map[string]*connectionDeviceAuthSession{}}

func (s *connectionDeviceAuthSessionStore) put(session *connectionDeviceAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
}

func (s *connectionDeviceAuthSessionStore) get(id string) (*connectionDeviceAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	return session, ok
}

func (s *connectionDeviceAuthSessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

type connectionDeviceLooseInt int

func (v *connectionDeviceLooseInt) UnmarshalJSON(raw []byte) error {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		*v = connectionDeviceLooseInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	var parsed int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &parsed); err != nil {
		return err
	}
	*v = connectionDeviceLooseInt(parsed)
	return nil
}

func supportsConnectionDeviceAuth(app *AppTemplate) bool {
	return app != nil && app.Slug == integrationOpenAICodexSlug && containsString(app.Auth.Types, connectionAuthTypeDeviceCode)
}

func (s *Server) startConnectionDeviceAuth(ctx context.Context, userID int64, app *AppTemplate, conn *Connection) (map[string]any, error) {
	if !supportsConnectionDeviceAuth(app) {
		return nil, fmt.Errorf("%s does not support device-code auth", app.Slug)
	}
	var payload struct {
		UserCode     string                   `json:"user_code"`
		DeviceAuthID string                   `json:"device_auth_id"`
		ExpiresIn    connectionDeviceLooseInt `json:"expires_in"`
		Interval     connectionDeviceLooseInt `json:"interval"`
	}
	if err := postConnectionDeviceJSON(ctx, integrationOpenAICodexIssuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": integrationOpenAICodexClientID}, &payload); err != nil {
		return nil, err
	}
	if payload.UserCode == "" || payload.DeviceAuthID == "" {
		return nil, fmt.Errorf("OpenAI Codex device auth response was incomplete")
	}
	expiresIn := int(payload.ExpiresIn)
	interval := int(payload.Interval)
	if expiresIn <= 0 {
		expiresIn = 15 * 60
	}
	if interval <= 0 {
		interval = 5
	}
	session := &connectionDeviceAuthSession{
		ID:           "cauth_" + generateToken(18),
		UserID:       userID,
		ConnectionID: conn.ID,
		AppSlug:      app.Slug,
		DeviceAuthID: payload.DeviceAuthID,
		UserCode:     payload.UserCode,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
		Interval:     interval,
		CreatedAt:    time.Now(),
	}
	globalConnectionDeviceAuthSessions.put(session)
	return map[string]any{
		"session_id":       session.ID,
		"provider":         integrationOpenAICodexSlug,
		"method":           connectionAuthTypeDeviceCode,
		"verification_uri": integrationOpenAICodexIssuer + "/codex/device",
		"user_code":        payload.UserCode,
		"expires_at":       session.ExpiresAt.Format(time.RFC3339),
		"interval_seconds": interval,
	}, nil
}

func (s *Server) handlePollConnectionDeviceAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/connections/auth/")
	sessionID = strings.Trim(sessionID, "/")
	if sessionID == "" || sessionID == "start" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	session, ok := globalConnectionDeviceAuthSessions.get(sessionID)
	if !ok || session.UserID != getUserID(r) {
		http.Error(w, "auth session not found", http.StatusNotFound)
		return
	}
	if time.Now().After(session.ExpiresAt) {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		writeJSON(w, map[string]any{"status": "expired"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	reqBody := map[string]string{
		"device_auth_id": session.DeviceAuthID,
		"user_code":      session.UserCode,
	}
	var codeResp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	status, body, err := postConnectionDeviceJSONStatus(ctx, integrationOpenAICodexIssuer+"/api/accounts/deviceauth/token", reqBody, &codeResp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		writeJSON(w, map[string]any{
			"status":            "pending",
			"next_poll_seconds": session.Interval,
		})
		return
	}
	if status < 200 || status >= 300 {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		writeJSON(w, map[string]any{
			"status": "failed",
			"error":  strings.TrimSpace(string(body)),
		})
		return
	}
	if codeResp.AuthorizationCode == "" || codeResp.CodeVerifier == "" {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		writeJSON(w, map[string]any{
			"status": "failed",
			"error":  "OpenAI Codex device auth response was incomplete",
		})
		return
	}

	tokens, err := exchangeConnectionOpenAICodexCode(ctx, codeResp.AuthorizationCode, codeResp.CodeVerifier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	credentials := buildConnectionOpenAICodexCredentials(tokens)
	raw, _ := json.Marshal(credentials)
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateConnectionCredentials(session.ConnectionID, encrypted); err != nil {
		http.Error(w, "persist credentials failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateConnectionStatus(session.ConnectionID, "active"); err != nil {
		http.Error(w, "activate connection failed", http.StatusInternalServerError)
		return
	}
	conn, encCreds, err := s.store.GetConnection(session.UserID, session.ConnectionID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusInternalServerError)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app != nil {
		s.maybeAutoCreateMCPForConnection(session.UserID, conn, app, encCreds)
	}
	globalConnectionDeviceAuthSessions.delete(sessionID)
	s.recomputePendingOptions()
	writeJSON(w, map[string]any{
		"status":     "connected",
		"connection": conn,
		"account": map[string]any{
			"id":    credentials["account_id"],
			"email": credentials["account_email"],
		},
	})
}

func (s *Server) maybeAutoCreateMCPForConnection(userID int64, conn *Connection, app *AppTemplate, encCreds string) {
	if conn == nil || app == nil {
		return
	}
	var createdVia string
	_ = s.store.db.QueryRow(`SELECT COALESCE(created_via, 'integration') FROM connections WHERE id=?`, conn.ID).Scan(&createdVia)
	if createdVia == "app_install" || !connectionAutoMCPFlag(s, conn.ID) {
		return
	}
	if app.Kind == "remote_mcp" {
		_, _ = s.createRemoteMcpFromConnection(userID, conn, app, encCreds)
		return
	}
	_, _ = s.store.CreateMCPServerFromConnection(userID, conn, len(app.Tools))
}

func executeOpenAICodexIntegrationTool(app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any) (*ExecuteResult, error) {
	accessToken := strings.TrimSpace(credentials["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("OpenAI Codex connection is missing access_token")
	}
	timeout := 120 * time.Second
	if tool.TimeoutMS > 0 {
		timeout = time.Duration(tool.TimeoutMS) * time.Millisecond
	}
	payload := map[string]any{}
	normalizeChat := false
	normalizeImage := false
	switch tool.Name {
	case "responses_create":
		for k, v := range input {
			payload[k] = v
		}
		if _, ok := payload["instructions"]; !ok {
			payload["instructions"] = "You are a helpful assistant."
		}
	case "chat_completion", "vision_describe":
		normalizeChat = true
		payload = buildOpenAICodexResponsesPayload(input)
	case "generate_image":
		normalizeImage = true
		payload = buildOpenAICodexImagePayload(input)
	default:
		return nil, fmt.Errorf("unsupported OpenAI Codex tool %q", tool.Name)
	}
	if _, ok := payload["store"]; !ok {
		payload["store"] = false
	}
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}
	status, data, headers, err := callOpenAICodexResponses(context.Background(), accessToken, payload, timeout)
	if err != nil {
		return &ExecuteResult{Success: false, Status: status, Data: map[string]any{"error": err.Error()}, Headers: headers}, nil
	}
	if normalizeChat {
		data = normalizeOpenAICodexChatCompletion(data, input)
	} else if normalizeImage {
		data = normalizeOpenAICodexImageGeneration(data, input)
	}
	return &ExecuteResult{Success: status >= 200 && status < 300, Status: status, Data: data, Headers: headers}, nil
}

func buildOpenAICodexResponsesPayload(input map[string]any) map[string]any {
	model := strings.TrimSpace(fmt.Sprint(input["model"]))
	if model == "" || model == "<nil>" || model == "kimi-k2.6" {
		model = "gpt-5.5"
	}
	payload := map[string]any{
		"model": model,
	}
	if v, ok := input["temperature"]; ok {
		payload["temperature"] = v
	}
	if v, ok := input["response_format"]; ok {
		payload["text"] = map[string]any{"format": v}
	}
	messages, _ := input["messages"].([]any)
	var instructions []string
	var items []any
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(fmt.Sprint(msg["role"]))
		content := msg["content"]
		if role == "system" {
			if text := contentText(content); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if role == "" {
			role = "user"
		}
		item := map[string]any{
			"type":    "message",
			"role":    role,
			"content": responsesContentParts(content),
		}
		items = append(items, item)
	}
	if len(instructions) > 0 {
		payload["instructions"] = strings.Join(instructions, "\n\n")
	} else {
		payload["instructions"] = "You are a helpful assistant."
	}
	if len(items) == 0 {
		payload["input"] = fmt.Sprint(input["prompt"])
	} else {
		payload["input"] = items
	}
	return payload
}

func buildOpenAICodexImagePayload(input map[string]any) map[string]any {
	prompt := strings.TrimSpace(fmt.Sprint(input["prompt"]))
	if prompt == "" || prompt == "<nil>" {
		prompt = strings.TrimSpace(fmt.Sprint(input["input"]))
	}
	instructions := strings.TrimSpace(fmt.Sprint(input["instructions"]))
	if instructions == "" || instructions == "<nil>" {
		instructions = "Generate the requested image using the hosted image_generation tool. Return the completed image result."
	}
	tool := map[string]any{
		"type":   "image_generation",
		"action": "generate",
	}
	for _, key := range []string{"size", "quality", "output_format", "background", "output_compression"} {
		if v, ok := input[key]; ok && v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
			tool[key] = v
		}
	}
	payload := map[string]any{
		"model":        openAICodexResponsesModel(input["model"]),
		"instructions": instructions,
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": prompt,
			}},
		}},
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "image_generation"},
		"store":       false,
		// The ChatGPT-backed Codex runtime requires streaming responses.
		// callOpenAICodexResponses parses the SSE response and recovers the
		// completed response object for normalizers below.
		"stream": true,
	}
	return payload
}

func openAICodexResponsesModel(raw any) string {
	model := strings.TrimSpace(fmt.Sprint(raw))
	if model == "" || model == "<nil>" || model == "kimi-k2.6" ||
		strings.HasPrefix(model, "gpt-image") || strings.HasPrefix(model, "dall-e") {
		return "gpt-5.5"
	}
	return model
}

func responsesContentParts(content any) any {
	switch c := content.(type) {
	case string:
		return []any{map[string]any{"type": "input_text", "text": c}}
	case []any:
		var out []any
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text", "input_text":
				out = append(out, map[string]any{"type": "input_text", "text": fmt.Sprint(part["text"])})
			case "image_url", "input_image":
				imageURL := ""
				if s, ok := part["image_url"].(string); ok {
					imageURL = s
				} else if m, ok := part["image_url"].(map[string]any); ok {
					imageURL = fmt.Sprint(m["url"])
				} else if s, ok := part["image"].(string); ok {
					imageURL = s
				}
				if imageURL != "" {
					image := map[string]any{"type": "input_image", "image_url": imageURL}
					if detail, ok := part["detail"]; ok {
						image["detail"] = detail
					}
					out = append(out, image)
				}
			}
		}
		return out
	default:
		return []any{map[string]any{"type": "input_text", "text": fmt.Sprint(c)}}
	}
}

func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, raw := range c {
			if part, ok := raw.(map[string]any); ok && (part["type"] == "text" || part["type"] == "input_text") {
				if text := strings.TrimSpace(fmt.Sprint(part["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return strings.TrimSpace(fmt.Sprint(c))
	}
}

func normalizeOpenAICodexChatCompletion(data any, input map[string]any) any {
	text := extractOpenAICodexIntegrationText(data)
	model := openAICodexResponsesModel(input["model"])
	out := map[string]any{
		"id":      "",
		"object":  "chat.completion",
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
	}
	if m, ok := data.(map[string]any); ok {
		if id, _ := m["id"].(string); id != "" {
			out["id"] = id
		}
		if usage, ok := m["usage"]; ok {
			out["usage"] = usage
		}
	}
	return out
}

func normalizeOpenAICodexImageGeneration(data any, input map[string]any) any {
	out := map[string]any{
		"data":  []any{},
		"model": openAICodexResponsesModel(input["model"]),
	}
	m, ok := data.(map[string]any)
	if !ok {
		return out
	}
	if id, _ := m["id"].(string); id != "" {
		out["id"] = id
	}
	if model, _ := m["model"].(string); strings.TrimSpace(model) != "" {
		out["model"] = model
	}
	if usage, ok := m["usage"]; ok {
		out["usage"] = usage
	}
	output, _ := m["output"].([]any)
	images := make([]any, 0, len(output))
	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok || obj["type"] != "image_generation_call" {
			continue
		}
		result, _ := obj["result"].(string)
		if strings.TrimSpace(result) == "" {
			continue
		}
		image := map[string]any{"b64_json": result}
		if revised, _ := obj["revised_prompt"].(string); strings.TrimSpace(revised) != "" {
			image["revised_prompt"] = revised
		}
		images = append(images, image)
	}
	out["data"] = images
	return out
}

func extractOpenAICodexIntegrationText(data any) string {
	m, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	if text, _ := m["output_text"].(string); strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	output, _ := m["output"].([]any)
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

func callOpenAICodexResponses(ctx context.Context, accessToken string, payload map[string]any, timeout time.Duration) (int, any, map[string]string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, integrationOpenAICodexBackendAPIBaseURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	headers := pickForwardableHeaders(resp.Header)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10_000_000))
	var data any
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && isOpenAICodexStreamResponse(resp.Header, body) {
		data = parseOpenAICodexSSE(body)
	} else if err := json.Unmarshal(body, &data); err != nil {
		data = map[string]any{"raw": string(body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, data, headers, fmt.Errorf("HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}
	return resp.StatusCode, data, headers, nil
}

func isOpenAICodexStreamResponse(header http.Header, body []byte) bool {
	if strings.Contains(header.Get("Content-Type"), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("event: "))
}

func parseOpenAICodexSSE(body []byte) map[string]any {
	out := map[string]any{
		"object": "response",
	}
	var text strings.Builder
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
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
		switch event["type"] {
		case "response.output_text.delta":
			if delta, _ := event["delta"].(string); delta != "" {
				text.WriteString(delta)
			}
		case "response.output_text.done":
			if done, _ := event["text"].(string); done != "" {
				text.Reset()
				text.WriteString(done)
			}
		case "response.completed":
			if response, ok := event["response"].(map[string]any); ok {
				for k, v := range response {
					out[k] = v
				}
			}
		}
	}
	out["output_text"] = strings.TrimSpace(text.String())
	return out
}

func refreshIntegrationOpenAICodexCredentials(credentials map[string]string) error {
	refreshToken := strings.TrimSpace(credentials["refresh_token"])
	if refreshToken == "" {
		return fmt.Errorf("OpenAI Codex connection is missing refresh_token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tokens, err := postConnectionDeviceFormForTokens(ctx, integrationOpenAICodexTokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     integrationOpenAICodexClientID,
	})
	if err != nil {
		return err
	}
	next := buildConnectionOpenAICodexCredentials(tokens)
	if next["refresh_token"] == "" {
		next["refresh_token"] = refreshToken
	}
	for k, v := range next {
		credentials[k] = v
	}
	return nil
}

func connectionOpenAICodexNeedsRefresh(credentials map[string]string, skew time.Duration) bool {
	raw := strings.TrimSpace(credentials["token_expires_at"])
	if raw == "" {
		raw = strings.TrimSpace(credentials["expires_at"])
	}
	if raw == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, raw)
	return err == nil && time.Until(exp) <= skew
}

func exchangeConnectionOpenAICodexCode(ctx context.Context, code, verifier string) (map[string]any, error) {
	return postConnectionDeviceFormForTokens(ctx, integrationOpenAICodexTokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  integrationOpenAICodexIssuer + "/deviceauth/callback",
		"client_id":     integrationOpenAICodexClientID,
		"code_verifier": verifier,
	})
}

func buildConnectionOpenAICodexCredentials(tokens map[string]any) map[string]string {
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)
	expiresAt := ""
	if exp, ok := connectionJWTExpiry(accessToken); ok {
		expiresAt = exp.Format(time.RFC3339)
	}
	claims := connectionJWTClaims(accessToken)
	creds := map[string]string{
		"access_token":     accessToken,
		"token":            accessToken,
		"bearer_token":     accessToken,
		"refresh_token":    refreshToken,
		"token_expires_at": expiresAt,
		"expires_at":       expiresAt,
		"last_refresh":     time.Now().UTC().Format(time.RFC3339),
		"auth_provider":    integrationOpenAICodexSlug,
		"auth_type":        connectionAuthTypeDeviceCode,
		"runtime_base_url": integrationOpenAICodexBackendAPIBaseURL,
	}
	if sub, _ := claims["sub"].(string); strings.TrimSpace(sub) != "" {
		creds["account_id"] = sub
	}
	if email, _ := claims["email"].(string); strings.TrimSpace(email) != "" {
		creds["account_email"] = email
	}
	return creds
}

func postConnectionDeviceJSON(ctx context.Context, url string, in any, out any) error {
	status, body, err := postConnectionDeviceJSONStatus(ctx, url, in, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("request failed with status %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

func postConnectionDeviceJSONStatus(ctx context.Context, url string, in any, out any) (int, []byte, error) {
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

func postConnectionDeviceFormForTokens(ctx context.Context, url string, fields map[string]string) (map[string]any, error) {
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

func connectionJWTExpiry(token string) (time.Time, bool) {
	claims := connectionJWTClaims(token)
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

func connectionJWTClaims(token string) map[string]any {
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
