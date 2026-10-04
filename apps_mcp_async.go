package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type appMCPAsyncRequest struct {
	ProjectID   string
	UserID      int64
	StartedAt   time.Time
	Cursor      int64
	ThreadEpoch int64
	SetupError  error
	ToolName    string
	Spec        *sdk.AsyncResultSpec
	AgentID     int64
	ThreadID    string
}

func (s *Server) inspectAppMCPAsyncRequest(entry *InstalledApp, r *http.Request, projectID string) *appMCPAsyncRequest {
	if entry == nil || r == nil || r.Method != http.MethodPost {
		return nil
	}
	agentID, _ := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Agent")), 10, 64)
	if agentID <= 0 {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || rpc.Method != "tools/call" {
		return nil
	}
	toolName := strings.TrimSpace(rpc.Params.Name)
	if toolName == "" {
		return nil
	}
	spec := asyncResultSpecForTool(entry, toolName)
	if spec == nil || spec.Notify == nil {
		return nil
	}
	threadID := strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Thread"))
	if threadID == "" {
		threadID = "main"
	}
	req := &appMCPAsyncRequest{ToolName: toolName, Spec: spec, AgentID: agentID, ThreadID: threadID,
		ProjectID: projectID, UserID: getUserID(r), StartedAt: time.Now()}
	req.SetupError = sdk.ValidateAsyncResultSpec(spec)
	if req.SetupError == nil {
		req.Cursor, req.ThreadEpoch, req.SetupError = s.store.asyncSubscriptionCursor(agentID, threadID)
	}
	return req
}

func asyncResultSpecForTool(entry *InstalledApp, toolName string) *sdk.AsyncResultSpec {
	if entry == nil {
		return nil
	}
	for i := range entry.Manifest.Provides.MCPTools {
		tool := &entry.Manifest.Provides.MCPTools[i]
		if tool.Name == toolName || entry.AppName+"_"+tool.Name == toolName {
			return tool.AsyncResult
		}
	}
	return nil
}

func (s *Server) maybeAugmentAppMCPAsyncResponse(entry *InstalledApp, req *appMCPAsyncRequest, resp *http.Response) error {
	if entry == nil || req == nil || req.Spec == nil || req.Spec.Notify == nil || resp == nil {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	nextBody := body
	defer func() {
		resp.Body = io.NopCloser(bytes.NewReader(nextBody))
		resp.ContentLength = int64(len(nextBody))
		resp.Header.Set("Content-Length", strconv.Itoa(len(nextBody)))
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var rpc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&rpc); err != nil {
		return nil
	}
	result, _ := rpc["result"].(map[string]any)
	if result == nil {
		return nil
	}
	if isErr, _ := result["isError"].(bool); isErr {
		return nil
	}
	content, _ := result["content"].([]any)
	toolResult, _ := result["structuredContent"].(map[string]any)
	var textBlocks []map[string]any
	idField := strings.TrimSpace(req.Spec.IDField)
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block["type"] != "text" {
			continue
		}
		value, _ := block["text"].(string)
		var decoded map[string]any
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&decoded) != nil || decoded == nil {
			continue
		}
		if _, ok := decoded[idField]; !ok {
			continue
		}
		if toolResult == nil {
			toolResult = decoded
		}
		textBlocks = append(textBlocks, block)
	}
	if toolResult == nil {
		return nil
	}
	asyncID, hasID := toolResult[idField]
	var sub *Subscription
	registrationErr := req.SetupError
	if registrationErr == nil && (!hasID || !validAsyncIdentifier(asyncID)) {
		registrationErr = fmt.Errorf("tool response is missing a valid async identifier %q", idField)
	}
	if registrationErr == nil {
		sub, registrationErr = s.createAsyncResultSubscriptions(entry, req, toolResult)
	}
	metadata := map[string]any{"will_notify": false, "id_field": idField, "id": asyncID}
	if registrationErr != nil {
		log.Printf("[APP-MCP-ASYNC] register app=%s install=%d tool=%s: %v", entry.AppName, entry.InstallID, req.ToolName, registrationErr)
		metadata["error"] = "Notification registration failed: " + registrationErr.Error()
		metadata["instruction"] = "The tool call already ran, but automatic notifications are unavailable. Do not repeat the tool call; use its returned identifier to check status or report the notification failure."
	} else if sub != nil {
		metadata["will_notify"] = true
		metadata["subscription_id"] = sub.ID
		metadata["mode"] = sub.AsyncMode
		metadata["events"] = sub.Events
		metadata["terminal_events"] = sub.TerminalEvents
		if expires, err := parseTime(sub.ExpiresAt); err == nil {
			metadata["expires_at"] = expires.UTC().Format(time.RFC3339Nano)
		}
		metadata["instruction"] = "You will receive an event when this async result completes, fails, or is cancelled. Do not poll status unless the user explicitly asks for progress."
		if sub.AsyncMode == "stream" {
			metadata["instruction"] = "You will receive matching progress and completion events in this thread. Only a declared terminal event closes the subscription; requests for input keep it open. Do not poll status unless the user explicitly asks."
		}
	}
	toolResult["_apteva_async"] = metadata
	if result["structuredContent"] != nil {
		result["structuredContent"] = toolResult
	}
	for _, block := range textBlocks {
		value, _ := block["text"].(string)
		var decoded map[string]any
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&decoded) != nil {
			continue
		}
		decoded["_apteva_async"] = metadata
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return err
		}
		block["text"] = string(encoded)
	}
	encoded, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	nextBody = encoded
	return nil
}

func validAsyncIdentifier(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case json.Number:
		return v.String() != ""
	case float64, int64, int:
		return true
	default:
		return false
	}
}

func (s *Server) createAsyncResultSubscriptions(entry *InstalledApp, req *appMCPAsyncRequest, toolResult map[string]any) (*Subscription, error) {
	if err := sdk.ValidateAsyncResultSpec(req.Spec); err != nil {
		return nil, err
	}
	notify := req.Spec.Notify
	if notify == nil || req.AgentID <= 0 {
		return nil, fmt.Errorf("async caller and notification declaration required")
	}
	match := make(map[string]any, len(notify.Match))
	for eventField, expr := range notify.Match {
		value, ok := resolveAsyncResultExpression(expr, toolResult)
		if !ok || !validAsyncIdentifier(value) {
			return nil, fmt.Errorf("cannot resolve async match %q", eventField)
		}
		match[eventField] = value
	}
	if len(match) == 0 {
		match[req.Spec.IDField] = toolResult[req.Spec.IDField]
	}
	// A static filter alone could subscribe this caller to unrelated jobs.
	correlated := false
	if len(notify.Match) == 0 {
		correlated = true
	}
	for _, expr := range notify.Match {
		correlated = correlated || strings.TrimSpace(expr) == "$result."+req.Spec.IDField
	}
	if !correlated {
		return nil, fmt.Errorf("async match must include the returned %s identifier", req.Spec.IDField)
	}
	matchJSON, err := json.Marshal(match)
	if err != nil {
		return nil, err
	}
	agent, err := s.store.GetAgentByID(req.AgentID)
	if err != nil {
		return nil, err
	}
	if req.UserID != 0 && req.UserID != agent.UserID {
		return nil, fmt.Errorf("async caller does not own the agent")
	}
	projectID := req.ProjectID
	if req.StartedAt.IsZero() && projectID == "" {
		projectID = entry.ProjectID
	}
	if entry.ProjectID != "" && entry.ProjectID != projectID {
		return nil, fmt.Errorf("async source belongs to another project")
	}
	if agent.ProjectID != projectID {
		scope, err := s.store.AgentThreadProjectForUser(agent.UserID, agent.ID, req.ThreadID)
		if err != nil {
			return nil, err
		}
		if scope == "" || scope != projectID {
			return nil, fmt.Errorf("async caller thread is not scoped to this project")
		}
	}
	duration := 24 * time.Hour
	if parsed, ok := parseAsyncExpiresAfter(notify.ExpiresAfter); ok {
		duration = parsed
	}
	sub, err := s.store.CreateEphemeralAppEventSubscription(agent.UserID, req.AgentID,
		fmt.Sprintf("%s %s async result", entry.AppName, req.ToolName), entry.AppName+":*",
		"Auto-created for async MCP result "+req.ToolName, req.ThreadID, projectID, notify.Events, string(matchJSON),
		"async-"+generateID(), time.Now().Add(duration), asyncSubscriptionOptions{
			Mode: notify.Mode, TerminalEvents: notify.TerminalEvents, SourceInstallID: entry.InstallID,
			Cursor: req.Cursor, StartedAt: req.StartedAt, ThreadEpoch: req.ThreadEpoch})
	if err != nil {
		return nil, err
	}
	if s.appEventDispatcher != nil {
		if err := s.appEventDispatcher.Reconcile(); err != nil {
			log.Printf("[APP-MCP-ASYNC] reconcile: %v", err)
		}
		s.appEventDispatcher.wakeOutbox()
	}
	return sub, nil
}

func resolveAsyncResultExpression(expr string, result map[string]any) (any, bool) {
	expr = strings.TrimSpace(expr)
	const prefix = "$result."
	if !strings.HasPrefix(expr, prefix) {
		return expr, true
	}
	key := strings.TrimSpace(strings.TrimPrefix(expr, prefix))
	if key == "" {
		return nil, false
	}
	value, ok := result[key]
	return value, ok
}

func parseAsyncExpiresAfter(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
