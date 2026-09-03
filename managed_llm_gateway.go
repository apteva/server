package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type managedLLMInFlight struct {
	global    int
	workspace map[string]int
}

var managedLLMRequests = struct {
	sync.Mutex
	byServer map[*Server]*managedLLMInFlight
}{byServer: map[*Server]*managedLLMInFlight{}}

type managedLLMReservation struct {
	workspaceKey string
	inputTokens  int64
	outputTokens int64
}

func estimateManagedLLMInputTokens(body []byte) int64 {
	// Reservation only; actual provider usage replaces it after the response.
	// One token per request byte is intentionally an upper bound rather than a
	// billing estimate, so multilingual or code-heavy prompts cannot slip past
	// a hard daily limit before the provider returns its tokenizer's count.
	n := int64(len(body))
	if n < 1 {
		return 1
	}
	return n
}

func managedLLMWorkspaceKey(userID int64, projectID string) string {
	return fmt.Sprintf("%d:%s", userID, projectID)
}

func (s *Server) reserveManagedLLMRequest(userID int64, projectID string, body map[string]any, raw []byte, policy AccessPolicy) (managedLLMReservation, error) {
	key := managedLLMWorkspaceKey(userID, projectID)
	managedLLMRequests.Lock()
	defer managedLLMRequests.Unlock()
	state := managedLLMRequests.byServer[s]
	if state == nil {
		state = &managedLLMInFlight{workspace: map[string]int{}}
		managedLLMRequests.byServer[s] = state
	}
	if limit := policy.Limits.GlobalConcurrentLLMCalls; limit > 0 && state.global >= limit {
		return managedLLMReservation{}, errors.New("global_capacity")
	}
	if limit := policy.Limits.ConcurrentLLMRequests; limit > 0 && state.workspace[key] >= limit {
		return managedLLMReservation{}, errors.New("workspace_capacity")
	}
	usage, err := s.store.ManagedLLMUsageForDay(userID, projectID, time.Now().UTC())
	if err != nil {
		return managedLLMReservation{}, err
	}
	if limit := int64(policy.Limits.DailyModelCalls); limit > 0 && usage.Calls >= limit {
		return managedLLMReservation{}, errors.New("daily_calls")
	}
	input := estimateManagedLLMInputTokens(raw)
	reserveOutput := int64(4096)
	if value, ok := jsonNumberInt64(body["max_tokens"]); ok && value > 0 {
		reserveOutput = value
	}
	if value, ok := jsonNumberInt64(body["max_completion_tokens"]); ok && value > 0 && value < reserveOutput {
		reserveOutput = value
	}
	if limit := int64(policy.Limits.DailyTokens); limit > 0 {
		remaining := limit - usage.InputTokens - usage.OutputTokens
		if remaining <= input {
			return managedLLMReservation{}, errors.New("daily_tokens")
		}
		if reserveOutput > remaining-input {
			reserveOutput = remaining - input
		}
		if reserveOutput < 1 {
			return managedLLMReservation{}, errors.New("daily_tokens")
		}
		if _, usesCompletionLimit := body["max_completion_tokens"]; usesCompletionLimit {
			body["max_completion_tokens"] = reserveOutput
		} else {
			body["max_tokens"] = reserveOutput
		}
	}
	if err := s.store.RecordManagedLLMUsage(userID, projectID, 1, input, reserveOutput, time.Now().UTC()); err != nil {
		return managedLLMReservation{}, err
	}
	state.global++
	state.workspace[key]++
	return managedLLMReservation{workspaceKey: key, inputTokens: input, outputTokens: reserveOutput}, nil
}

func jsonNumberInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), true
	case json.Number:
		v, err := number.Int64()
		return v, err == nil
	default:
		return 0, false
	}
}

func (s *Server) releaseManagedLLMRequest(reservation managedLLMReservation) {
	managedLLMRequests.Lock()
	defer managedLLMRequests.Unlock()
	state := managedLLMRequests.byServer[s]
	if state == nil {
		return
	}
	if state.global > 0 {
		state.global--
	}
	if state.workspace[reservation.workspaceKey] > 1 {
		state.workspace[reservation.workspaceKey]--
	} else {
		delete(state.workspace, reservation.workspaceKey)
	}
}

func (s *Server) refundManagedLLMReservation(userID int64, projectID string, reservation managedLLMReservation) {
	_ = s.store.RecordManagedLLMUsage(userID, projectID, -1,
		-reservation.inputTokens, -reservation.outputTokens, time.Now().UTC())
}

type tailCapture struct {
	limit int
	data  []byte
}

type managedLLMStreamWriter struct {
	destination http.ResponseWriter
	capture     io.Writer
}

func (w managedLLMStreamWriter) Write(p []byte) (int, error) {
	n, err := w.destination.Write(p)
	if n > 0 {
		_, _ = w.capture.Write(p[:n])
	}
	if flusher, ok := w.destination.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (c *tailCapture) Write(p []byte) (int, error) {
	c.data = append(c.data, p...)
	if len(c.data) > c.limit {
		c.data = append([]byte(nil), c.data[len(c.data)-c.limit:]...)
	}
	return len(p), nil
}

func managedLLMUsageFromResponse(raw []byte) (input, output int64) {
	parse := func(payload []byte) {
		var event struct {
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &event) == nil {
			if event.Usage.PromptTokens != 0 || event.Usage.CompletionTokens != 0 {
				input, output = event.Usage.PromptTokens, event.Usage.CompletionTokens
			} else if event.Usage.InputTokens != 0 || event.Usage.OutputTokens != 0 {
				input, output = event.Usage.InputTokens, event.Usage.OutputTokens
			}
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		parse(trimmed)
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			parse(payload)
		}
	}
	return input, output
}

func (s *Server) managedLLMUpstream(policy ManagedLLMPolicy) (*runtimeConnection, *AppTemplate, map[string]string, string, error) {
	conn, encrypted, err := s.store.GetConnectionAny(policy.ConnectionID)
	if err != nil {
		return nil, nil, nil, "", err
	}
	if conn.Status != "active" || conn.ProjectID != "" || conn.CredentialManagement != "platform" || conn.CredentialExportPolicy != "never" {
		return nil, nil, nil, "", errors.New("managed LLM connection is not protected")
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Runtime == nil || !strings.EqualFold(app.Runtime.Role, "llm") {
		return nil, nil, nil, "", errors.New("managed LLM integration unavailable")
	}
	plaintext, err := Decrypt(s.secret, encrypted)
	if err != nil {
		return nil, nil, nil, "", err
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &credentials); err != nil {
		return nil, nil, nil, "", err
	}
	baseURL := resolveTemplate(app.BaseURL, credentials)
	var runtimeConfig map[string]any
	if raw := s.store.GetConnectionRuntimeConfigAny(conn.ID); raw != "" {
		_ = json.Unmarshal([]byte(raw), &runtimeConfig)
	}
	if configured, _ := runtimeConfig["base_url"].(string); strings.TrimSpace(configured) != "" {
		baseURL = resolveTemplate(strings.TrimSpace(configured), credentials)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, nil, nil, "", errors.New("managed LLM integration requires an HTTPS base_url")
	}
	upstream := strings.TrimRight(baseURL, "/") + policy.Path
	if query := buildAuthQuery(app.Auth.QueryParams, credentials); query != "" {
		upstream += "?" + query
	}
	runtimeConn := &runtimeConnection{ID: conn.ID, AppSlug: conn.AppSlug, ProjectID: conn.ProjectID}
	return runtimeConn, app, credentials, upstream, nil
}

// handleManagedLLMChat is intentionally authenticated with a core key rather
// than the general API middleware. Core keys are accepted nowhere else except
// their narrow runtime-token callback, keeping the gateway credential scoped
// to one agent process.
func (s *Server) handleManagedLLMChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !strings.HasPrefix(token, "core_") {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "invalid_agent_credential"})
		return
	}
	agent, err := s.store.GetAgentByCoreAPIKey(token)
	if err != nil || agent.Status != "running" {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "invalid_agent_credential"})
		return
	}
	policy, err := s.loadAccessPolicy()
	if err != nil || policy.ManagedLLM.ConnectionID <= 0 {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "managed_llm_unavailable"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	model, _ := body["model"].(string)
	allowedModels := policy.ManagedLLM.Models
	if len(allowedModels) == 0 {
		allowedModels = policy.Capabilities.AllowedModels
	}
	if len(allowedModels) > 0 && !containsExact(allowedModels, model) {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "model_not_allowed", "model": model})
		return
	}
	if s.store.GetPlatformRole(agent.UserID) == PlatformAdmin {
		// Operators can test and use the managed connection without consuming a
		// hosted-user allowance. Global concurrency still protects the provider.
		policy.Limits.ConcurrentLLMRequests = 0
		policy.Limits.DailyModelCalls = 0
		policy.Limits.DailyTokens = 0
	}
	// Ensure compatible streaming providers return the terminal usage event.
	if stream, _ := body["stream"].(bool); stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	reservation, err := s.reserveManagedLLMRequest(agent.UserID, agent.ProjectID, body, raw, policy)
	if err != nil {
		status := http.StatusTooManyRequests
		if err.Error() == "global_capacity" || err.Error() == "workspace_capacity" {
			status = http.StatusServiceUnavailable
		}
		writeJSONStatus(w, status, map[string]any{"error": err.Error()})
		return
	}
	defer s.releaseManagedLLMRequest(reservation)
	forwardBody, _ := json.Marshal(body)
	_, app, credentials, upstream, err := s.managedLLMUpstream(policy.ManagedLLM)
	if err != nil {
		s.refundManagedLLMReservation(agent.UserID, agent.ProjectID, reservation)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "managed_llm_unavailable"})
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(forwardBody))
	if err != nil {
		s.refundManagedLLMReservation(agent.UserID, agent.ProjectID, reservation)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "upstream_request_failed"})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range buildHeaders(app.Auth.Headers, credentials) {
		request.Header.Set(key, value)
	}
	client := &http.Client{
		Transport:     s.managedLLMTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		s.refundManagedLLMReservation(agent.UserID, agent.ProjectID, reservation)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "upstream_unavailable"})
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	capture := &tailCapture{limit: 256 << 10}
	_, _ = io.Copy(managedLLMStreamWriter{destination: w, capture: capture}, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.refundManagedLLMReservation(agent.UserID, agent.ProjectID, reservation)
		return
	}
	actualInput, actualOutput := managedLLMUsageFromResponse(capture.data)
	if actualInput > 0 || actualOutput > 0 {
		_ = s.store.RecordManagedLLMUsage(agent.UserID, agent.ProjectID, 0,
			actualInput-reservation.inputTokens, actualOutput-reservation.outputTokens, time.Now().UTC())
	}
}

func (s *Store) GetConnectionRuntimeConfigAny(connID int64) string {
	var raw string
	_ = s.db.QueryRow(`SELECT COALESCE(runtime_config,'{}') FROM connections WHERE id=?`, connID).Scan(&raw)
	return raw
}

func (s *Store) GetAgentByCoreAPIKey(key string) (*Agent, error) {
	var agent Agent
	err := s.db.QueryRow(`SELECT id,user_id,COALESCE(project_id,''),COALESCE(status,'stopped')
		FROM agents WHERE core_api_key=?`, key).Scan(&agent.ID, &agent.UserID, &agent.ProjectID, &agent.Status)
	return &agent, err
}
