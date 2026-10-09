package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	integrationOpenAICodexBackendAPIBaseURL = "https://chatgpt.com/backend-api/codex"
)

// integrationOpenAICodexTokenURL is a var, not a const, so tests can
// point the connection refresh at a local server. Without this the only
// way to exercise refreshIntegrationOpenAICodexCredentials was to call
// auth.openai.com for real. The provider path has had the equivalent
// seam (openAICodexTokenEndpoint) since it was written.
var integrationOpenAICodexTokenURL = "https://auth.openai.com/oauth/token"

// Tests replace only the Responses network edge; production uses the Codex API.
var integrationOpenAICodexResponsesURL = integrationOpenAICodexBackendAPIBaseURL + "/responses"

// Device endpoints are variables so the complete connection lifecycle can be
// exercised against httptest servers. Production keeps the ChatGPT device-auth
// endpoints; tests replace only the network edge.
var (
	integrationOpenAICodexDeviceUserCodeURL = integrationOpenAICodexIssuer + "/api/accounts/deviceauth/usercode"
	integrationOpenAICodexDeviceTokenURL    = integrationOpenAICodexIssuer + "/api/accounts/deviceauth/token"
)

type connectionDeviceAuthSession struct {
	ID                      string
	UserID                  int64
	ConnectionID            int64
	AppSlug                 string
	DeviceAuthID            string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresAt               time.Time
	Interval                int
	Reauth                  bool
	CreatedAt               time.Time
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
	return app != nil && connectionSessionAuthDriverFor(app.Slug) != nil && containsString(app.Auth.Types, connectionAuthTypeDeviceCode)
}

func (s *Server) startConnectionDeviceAuth(ctx context.Context, userID int64, app *AppTemplate, conn *Connection, reauth bool) (map[string]any, error) {
	if !supportsConnectionDeviceAuth(app) {
		return nil, fmt.Errorf("%s does not support device-code auth", app.Slug)
	}
	driver := connectionSessionAuthDriverFor(app.Slug)
	authorization, err := driver.Start(ctx)
	if err != nil {
		return nil, err
	}
	expiresIn := authorization.ExpiresIn
	interval := authorization.Interval
	if expiresIn <= 0 {
		expiresIn = 15 * 60
	}
	if interval <= 0 {
		interval = 5
	}
	session := &connectionDeviceAuthSession{
		ID:                      "cauth_" + generateToken(18),
		UserID:                  userID,
		ConnectionID:            conn.ID,
		AppSlug:                 app.Slug,
		DeviceAuthID:            authorization.DeviceCode,
		UserCode:                authorization.UserCode,
		VerificationURI:         authorization.VerificationURI,
		VerificationURIComplete: authorization.VerificationURIComplete,
		ExpiresAt:               time.Now().Add(time.Duration(expiresIn) * time.Second),
		Interval:                interval,
		Reauth:                  reauth,
		CreatedAt:               time.Now(),
	}
	globalConnectionDeviceAuthSessions.put(session)
	response := map[string]any{
		"session_id":       session.ID,
		"provider":         app.Slug,
		"method":           connectionAuthTypeDeviceCode,
		"verification_uri": authorization.VerificationURI,
		"user_code":        authorization.UserCode,
		"expires_at":       session.ExpiresAt.Format(time.RFC3339),
		"interval_seconds": interval,
	}
	if authorization.VerificationURIComplete != "" {
		response["verification_uri_complete"] = authorization.VerificationURIComplete
	}
	return response, nil
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
		if !session.Reauth {
			_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		}
		writeJSON(w, map[string]any{"status": "expired"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	driver := connectionSessionAuthDriverFor(session.AppSlug)
	if driver == nil {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		http.Error(w, "device auth provider is no longer available", http.StatusBadRequest)
		return
	}
	result, err := driver.Poll(ctx, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if result.NextPollSeconds > 0 {
		session.Interval = result.NextPollSeconds
	}
	if result.Status == "pending" {
		writeJSON(w, map[string]any{
			"status":            "pending",
			"next_poll_seconds": session.Interval,
		})
		return
	}
	if result.Status == "expired" {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		if !session.Reauth {
			_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		}
		writeJSON(w, map[string]any{
			"status": "expired",
		})
		return
	}
	if result.Status != "connected" || len(result.Credentials) == 0 {
		globalConnectionDeviceAuthSessions.delete(sessionID)
		if !session.Reauth {
			_ = s.store.UpdateConnectionStatus(session.ConnectionID, "failed")
		}
		errorMessage := strings.TrimSpace(result.Error)
		if errorMessage == "" {
			errorMessage = "device auth response was incomplete"
		}
		writeJSON(w, map[string]any{
			"status": "failed",
			"error":  errorMessage,
		})
		return
	}
	credentials := result.Credentials
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

func executeOpenAICodexIntegrationTool(app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any, parents ...context.Context) (*ExecuteResult, error) {
	accessToken := strings.TrimSpace(credentials["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("OpenAI Codex connection is missing access_token")
	}
	timeout := integrationTimeoutFromContext(integrationRequestContext(parents), tool, 240*time.Second)
	resolvedInput := make(map[string]any, len(input)+1)
	for key, value := range input {
		resolvedInput[key] = value
	}
	if model := strings.TrimSpace(fmt.Sprint(resolvedInput["model"])); model == "" || model == "<nil>" || model == "kimi-k2.6" {
		model = "gpt-6.1-sol"
		ctx, cancel := context.WithTimeout(integrationRequestContext(parents), 15*time.Second)
		if models, err := fetchCodexModelCatalog(ctx, accessToken, credentials["account_id"], false); err == nil {
			model = codexDefaultModel(models, "medium")
		}
		cancel()
		resolvedInput["model"] = model
	}
	payload := map[string]any{}
	normalizeChat := false
	normalizeImage := false
	switch tool.Name {
	case "responses_create":
		for k, v := range resolvedInput {
			payload[k] = v
		}
		if _, ok := payload["instructions"]; !ok {
			payload["instructions"] = "You are a helpful assistant."
		}
	case "chat_completion", "vision_describe":
		normalizeChat = true
		payload = buildOpenAICodexResponsesPayload(resolvedInput)
	case "generate_image":
		normalizeImage = true
		payload = buildOpenAICodexImagePayload(resolvedInput)
	default:
		return nil, fmt.Errorf("unsupported OpenAI Codex tool %q", tool.Name)
	}
	if _, ok := payload["store"]; !ok {
		payload["store"] = false
	}
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}
	status, data, headers, err := callOpenAICodexResponses(integrationRequestContext(parents), accessToken, credentials["account_id"], payload, timeout)
	if err != nil {
		return &ExecuteResult{Success: false, Status: status, Data: data, Headers: headers}, nil
	}
	object, _ := data.(map[string]any)
	timing := object["timing"]
	if normalizeChat {
		data = normalizeOpenAICodexChatCompletion(data, input)
	} else if normalizeImage {
		data = normalizeOpenAICodexImageGeneration(data, input)
	}
	if object, ok := data.(map[string]any); ok && timing != nil {
		object["timing"] = timing
	}
	return &ExecuteResult{Success: status >= 200 && status < 300, Status: status, Data: data, Headers: headers}, nil
}

func buildOpenAICodexResponsesPayload(input map[string]any) map[string]any {
	model := strings.TrimSpace(fmt.Sprint(input["model"]))
	if model == "" || model == "<nil>" || model == "kimi-k2.6" {
		model = "gpt-6.1-sol"
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
		return "gpt-6.1-sol"
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

func callOpenAICodexResponses(ctx context.Context, accessToken, accountID string, payload map[string]any, timeout time.Duration) (status int, data any, headers map[string]string, retErr error) {
	started := time.Now()
	var terminalAt time.Time
	defer func() {
		timing := map[string]any{"request_duration_ms": float64(time.Since(started)) / float64(time.Millisecond)}
		if !terminalAt.IsZero() {
			timing["terminal_event_ms"] = float64(terminalAt.Sub(started)) / float64(time.Millisecond)
		}
		if retErr != nil {
			failure := map[string]any{"error": retErr.Error(), "timing": timing}
			var streamErr *codexStreamError
			if errors.As(retErr, &streamErr) {
				details := streamErr.details
				for _, name := range []string{"X-Request-Id", "Request-Id"} {
					if _, exists := details["request_id"]; !exists && headers[name] != "" {
						details["request_id"] = headers[name]
					}
				}
				hints, _ := details["retry_headers"].(map[string]string)
				if hints == nil {
					hints = map[string]string{}
				}
				for _, name := range codexRetryHintHeaders {
					if headers[name] != "" {
						hints[name] = headers[name]
					}
				}
				if len(hints) > 0 {
					details["retry_headers"] = hints
				}
				failure["error_details"] = details
			}
			data = failure
		} else if object, ok := data.(map[string]any); ok {
			object["timing"] = timing
		}
	}()
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, integrationOpenAICodexResponsesURL, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if strings.TrimSpace(accountID) != "" {
		req.Header.Set("ChatGPT-Account-ID", strings.TrimSpace(accountID))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	headers = codexResponseHeaders(resp.Header)
	reader := bufio.NewReader(resp.Body)
	stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if !stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		prefix, _ := reader.Peek(24)
		stream = isOpenAICodexStreamResponse(resp.Header, prefix)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && stream {
		parsed, err := parseOpenAICodexSSEReader(reader, func() {
			if terminalAt.IsZero() {
				terminalAt = time.Now()
			}
		})
		return resp.StatusCode, parsed, headers, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, 10_000_001))
	if readErr != nil {
		return resp.StatusCode, nil, headers, fmt.Errorf("read Codex response: %w", readErr)
	}
	if len(body) > 10_000_000 {
		return resp.StatusCode, nil, headers, fmt.Errorf("Codex response exceeds 10 MB")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && isOpenAICodexStreamResponse(resp.Header, body) {
		parsed, err := parseOpenAICodexSSE(body)
		if err != nil {
			return resp.StatusCode, nil, headers, err
		}
		data = parsed
	} else if err := json.Unmarshal(body, &data); err != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, nil, headers, fmt.Errorf("decode Codex response: %w", err)
		}
		data = map[string]any{"raw": string(body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		object, _ := data.(map[string]any)
		return resp.StatusCode, data, headers, codexStreamFailure("http.error", object, fmt.Sprintf("HTTP %d: Codex request failed", resp.StatusCode))
	}
	if object, ok := data.(map[string]any); ok {
		if object["error"] != nil {
			return resp.StatusCode, nil, headers, codexStreamFailure("response.failed", map[string]any{"response": object}, "Codex response contains an error")
		}
		if status, _ := object["status"].(string); status != "" && status != "completed" {
			return resp.StatusCode, nil, headers, codexStreamFailure("response."+status, map[string]any{"response": object}, "Codex response "+status)
		}
	}
	return resp.StatusCode, data, headers, nil
}

func isOpenAICodexStreamResponse(header http.Header, body []byte) bool {
	if strings.Contains(header.Get("Content-Type"), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("event:")) || bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:"))
}

// codexStreamError carries only diagnostic fields, never the response's prompts or output.
type codexStreamError struct {
	details map[string]any
	message string
}

func (e *codexStreamError) Error() string { return e.message }

func codexStreamFailure(kind string, event map[string]any, fallback string) *codexStreamError {
	details := map[string]any{"event_type": kind}
	response, _ := event["response"].(map[string]any)
	if id, ok := response["id"].(string); ok {
		details["response_id"] = id
	}
	if status, ok := response["status"].(string); ok {
		details["response_status"] = status
	}
	// Prefer the most specific error, falling back to the event-level fields.
	eventError, _ := event["error"].(map[string]any)
	responseError, _ := response["error"].(map[string]any)
	for _, source := range []map[string]any{event, response, eventError, responseError} {
		for _, key := range []string{"code", "message", "param", "parameter", "request_id", "retry_after", "retry_after_ms", "retry_after_seconds", "retry_at", "reset_at", "reset_after", "reset_after_ms", "reset_after_seconds", "resets_at", "resets_in_seconds", "reset_seconds"} {
			if value, ok := source[key]; ok {
				if value == nil && (key == "param" || key == "parameter") {
					details[key] = nil
				}
				switch value.(type) {
				case string, float64, json.Number, bool:
					details[key] = value
				}
			}
		}
	}
	for _, source := range []map[string]any{event, response, eventError, responseError} {
		if rawHeaders, ok := source["headers"].(map[string]any); ok {
			hints := map[string]string{}
			for key, raw := range rawHeaders {
				if strings.EqualFold(key, "X-Request-Id") || strings.EqualFold(key, "Request-Id") {
					if value, ok := raw.(string); ok {
						details["request_id"] = value
					}
				}
				for _, allowed := range codexRetryHintHeaders {
					if strings.EqualFold(key, allowed) {
						if value, ok := raw.(string); ok {
							hints[allowed] = value
						}
					}
				}
			}
			if len(hints) > 0 {
				details["retry_headers"] = hints
			}
		}
	}
	for _, source := range []map[string]any{eventError, responseError} {
		if value, ok := source["type"].(string); ok {
			details["type"] = value
		}
	}
	if value, ok := event["type"].(string); ok && value != kind && !strings.HasPrefix(value, "response.") {
		details["type"] = value
	}
	for _, source := range []map[string]any{event, response} {
		if incomplete, ok := source["incomplete_details"].(map[string]any); ok {
			if reason, ok := incomplete["reason"].(string); ok {
				details["incomplete_reason"] = reason
			}
		}
	}
	message, _ := details["message"].(string)
	if message == "" {
		message = fallback
		details["message"] = message
	}
	return &codexStreamError{details: details, message: message}
}

var codexRetryHintHeaders = []string{
	"Retry-After", "Retry-After-Ms", "X-Retry-After-Ms",
	"X-RateLimit-Reset-Requests", "X-RateLimit-Reset-Tokens", "X-RateLimit-Reset", "RateLimit-Reset",
	"X-Codex-Primary-Reset-At", "X-Codex-Secondary-Reset-At",
}

func codexResponseHeaders(header http.Header) map[string]string {
	out := pickForwardableHeaders(header)
	if out == nil {
		out = map[string]string{}
	}
	for _, name := range codexRetryHintHeaders {
		if value := header.Get(name); value != "" {
			out[name] = value
		}
	}
	return out
}

func parseOpenAICodexSSE(body []byte) (map[string]any, error) {
	return parseOpenAICodexSSEReader(bytes.NewReader(body), nil)
}

// Parse frames while they arrive so terminal timing does not include the wait for EOF.
// Successful completion still validates the rest of the stream, including read failures.
func parseOpenAICodexSSEReader(reader io.Reader, onTerminal func()) (map[string]any, error) {
	out := map[string]any{"object": "response"}
	var text strings.Builder
	var output []any
	seenOutput := map[string]bool{}
	completed := false
	limited := &io.LimitedReader{R: reader, N: 10_000_001}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 10_000_001)
	var frame []string
	var label string
	terminal := func() {
		if onTerminal != nil {
			onTerminal()
		}
	}
	ids := map[string]any{}
	failure := func(kind string, event map[string]any, message string) error {
		err := codexStreamFailure(kind, event, message)
		for key, value := range ids {
			if _, exists := err.details[key]; !exists {
				err.details[key] = value
			}
		}
		return err
	}
	process := func() error {
		if len(frame) == 0 {
			label = ""
			return nil
		}
		raw := strings.Join(frame, "\n")
		frame = nil
		kind := label
		label = ""
		if strings.TrimSpace(raw) == "[DONE]" {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil || event == nil {
			if kind == "" {
				kind = "stream.invalid_event"
			}
			return failure(kind, nil, "decode Codex stream event: invalid JSON object")
		}
		if requestID, ok := event["request_id"].(string); ok {
			ids["request_id"] = requestID
		}
		if response, ok := event["response"].(map[string]any); ok {
			if responseID, ok := response["id"].(string); ok {
				ids["response_id"] = responseID
			}
			if requestID, ok := response["request_id"].(string); ok {
				ids["request_id"] = requestID
			}
		}
		payloadKind, _ := event["type"].(string)
		if kind == "" {
			kind = payloadKind
		}
		if kind == "" {
			return failure("stream.invalid_event", nil, "decode Codex stream event: missing event type")
		}
		if kind != payloadKind && payloadKind != "" && kind != "error" {
			return failure(kind, nil, "decode Codex stream event: conflicting event types")
		}
		collectOpenAICodexImageOutput(event, &output, seenOutput)
		if item, ok := event["item"].(map[string]any); ok {
			collectOpenAICodexImageOutput(item, &output, seenOutput)
		}
		switch kind {
		case "response.failed", "response.incomplete", "error":
			terminal()
			return failure(kind, event, "Codex stream ended with "+kind)
		case "response.output_text.delta":
			delta, ok := event["delta"].(string)
			if !ok {
				return failure(kind, nil, "decode Codex stream event: delta must be text")
			}
			text.WriteString(delta)
		case "response.output_text.done":
			done, ok := event["text"].(string)
			if !ok {
				return failure(kind, nil, "decode Codex stream event: done text must be text")
			}
			text.Reset()
			text.WriteString(done)
		case "response.completed":
			terminal()
			response, ok := event["response"].(map[string]any)
			if !ok {
				return failure(kind, nil, "Codex response.completed event has no response")
			}
			if status, exists := response["status"]; exists && status != "completed" {
				return failure(kind, event, "Codex response has non-completed status")
			}
			if response["error"] != nil {
				return failure(kind, event, "Codex completed response contains an error")
			}
			completed = true
			for k, v := range response {
				out[k] = v
			}
			if items, ok := response["output"].([]any); ok {
				for _, rawItem := range items {
					if item, ok := rawItem.(map[string]any); ok {
						collectOpenAICodexImageOutput(item, &output, seenOutput)
					}
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return nil, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			label = value
		case "data":
			frame = append(frame, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, failure("stream.read_error", nil, "read Codex response: "+err.Error())
	}
	if limited.N <= 0 {
		return nil, failure("stream.too_large", nil, "Codex response exceeds 10 MB")
	}
	if err := process(); err != nil {
		return nil, err
	}
	if !completed {
		return nil, failure("stream.missing_completion", nil, "Codex stream ended without response.completed")
	}
	out["output_text"] = strings.TrimSpace(text.String())
	if len(output) > 0 {
		out["output"] = output
	}
	return out, nil
}

func collectOpenAICodexImageOutput(obj map[string]any, output *[]any, seen map[string]bool) {
	if obj == nil || obj["type"] != "image_generation_call" {
		return
	}
	result, _ := obj["result"].(string)
	if strings.TrimSpace(result) == "" {
		return
	}
	key := strings.TrimSpace(fmt.Sprint(obj["id"]))
	if key == "" || key == "<nil>" {
		key = result
	}
	if seen[key] {
		return
	}
	seen[key] = true
	item := map[string]any{
		"type":   "image_generation_call",
		"result": result,
	}
	for _, k := range []string{"id", "status", "revised_prompt"} {
		if v, ok := obj[k]; ok && strings.TrimSpace(fmt.Sprint(v)) != "" {
			item[k] = v
		}
	}
	*output = append(*output, item)
}

func refreshIntegrationOpenAICodexCredentials(credentials map[string]string) error {
	return refreshIntegrationOpenAICodexCredentialsContext(context.Background(), credentials)
}
func refreshIntegrationOpenAICodexCredentialsContext(parent context.Context, credentials map[string]string) error {
	refreshToken := strings.TrimSpace(credentials["refresh_token"])
	if refreshToken == "" {
		return fmt.Errorf("OpenAI Codex connection is missing refresh_token")
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
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
	if idToken, _ := tokens["id_token"].(string); strings.TrimSpace(idToken) != "" {
		if idClaims := connectionJWTClaims(idToken); len(idClaims) > 0 {
			claims = idClaims
		}
	}
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
	if authClaims := stateMap(claims, "https://api.openai.com/auth"); len(authClaims) > 0 {
		if accountID, _ := authClaims["chatgpt_account_id"].(string); strings.TrimSpace(accountID) != "" {
			creds["account_id"] = accountID
		}
	}
	if accountID, _ := tokens["account_id"].(string); strings.TrimSpace(accountID) != "" {
		creds["account_id"] = accountID
	}
	if creds["account_id"] == "" {
		if sub, _ := claims["sub"].(string); strings.TrimSpace(sub) != "" {
			creds["account_id"] = sub
		}
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
