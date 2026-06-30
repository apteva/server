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
	ToolName string
	Spec     *sdk.AsyncResultSpec
	AgentID  int64
	ThreadID string
}

func (s *Server) inspectAppMCPAsyncRequest(entry *InstalledApp, r *http.Request) *appMCPAsyncRequest {
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
	return &appMCPAsyncRequest{
		ToolName: toolName,
		Spec:     spec,
		AgentID:  agentID,
		ThreadID: strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Thread")),
	}
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
	if err := json.Unmarshal(body, &rpc); err != nil {
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
	if len(content) == 0 {
		return nil
	}
	first, _ := content[0].(map[string]any)
	if first == nil {
		return nil
	}
	text, _ := first["text"].(string)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var toolResult map[string]any
	if err := json.Unmarshal([]byte(text), &toolResult); err != nil {
		return nil
	}
	idField := strings.TrimSpace(req.Spec.IDField)
	if idField == "" {
		return nil
	}
	asyncID, ok := toolResult[idField]
	if !ok || fmt.Sprint(asyncID) == "" {
		return nil
	}
	created, events, err := s.createAsyncResultSubscriptions(entry, req, toolResult)
	if err != nil {
		log.Printf("[APP-MCP-ASYNC] create subscriptions app=%s install=%d tool=%s: %v", entry.AppName, entry.InstallID, req.ToolName, err)
		return nil
	}
	if !created {
		return nil
	}
	toolResult["_apteva_async"] = map[string]any{
		"will_notify": true,
		"id_field":    idField,
		"id":          asyncID,
		"events":      events,
		"instruction": "You will receive an event when this async result completes, fails, or is cancelled. Do not poll status unless the user explicitly asks for progress.",
	}
	nextText, err := json.Marshal(toolResult)
	if err != nil {
		return nil
	}
	first["text"] = string(nextText)
	nextBody, err = json.Marshal(rpc)
	if err != nil {
		return nil
	}
	return nil
}

func (s *Server) createAsyncResultSubscriptions(entry *InstalledApp, req *appMCPAsyncRequest, toolResult map[string]any) (bool, []string, error) {
	notify := req.Spec.Notify
	if strings.TrimSpace(notify.Target) != "" && notify.Target != "caller" {
		return false, nil, nil
	}
	if strings.TrimSpace(notify.Mode) != "" && notify.Mode != "once" {
		return false, nil, nil
	}
	if req.AgentID <= 0 || len(notify.Events) == 0 {
		return false, nil, nil
	}
	match := make(map[string]any, len(notify.Match))
	for eventField, expr := range notify.Match {
		value, ok := resolveAsyncResultExpression(expr, toolResult)
		if !ok {
			return false, nil, nil
		}
		match[eventField] = value
	}
	if len(match) == 0 && strings.TrimSpace(req.Spec.IDField) != "" {
		if v, ok := toolResult[req.Spec.IDField]; ok {
			match[req.Spec.IDField] = v
		}
	}
	if len(match) == 0 {
		return false, nil, nil
	}
	matchJSON, err := json.Marshal(match)
	if err != nil {
		return false, nil, err
	}
	agent, err := s.store.GetAgentByID(req.AgentID)
	if err != nil {
		return false, nil, err
	}
	expiresAt := time.Now().Add(24 * time.Hour)
	if d, ok := parseAsyncExpiresAfter(notify.ExpiresAfter); ok {
		expiresAt = time.Now().Add(d)
	}
	threadID := strings.TrimSpace(req.ThreadID)
	waitGroupID := "async-" + generateID()
	events := make([]string, 0, len(notify.Events))
	for _, event := range notify.Events {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		events = append(events, event)
	}
	events = compactSubscriptionEvents(events)
	if len(events) == 0 {
		return false, nil, nil
	}
	slug := entry.AppName + ":*"
	name := fmt.Sprintf("%s %s async result", entry.AppName, req.ToolName)
	if _, err := s.store.CreateEphemeralAppEventSubscription(
		agent.UserID, req.AgentID, name, slug,
		"Auto-created for async MCP result "+req.ToolName,
		threadID, entry.ProjectID, events, string(matchJSON), waitGroupID, expiresAt,
	); err != nil {
		_ = s.store.DeleteEphemeralSubscriptionWaitGroup(waitGroupID)
		return false, nil, err
	}
	if s.appEventDispatcher != nil {
		if err := s.appEventDispatcher.Reconcile(); err != nil {
			log.Printf("[APP-MCP-ASYNC] reconcile after create: %v", err)
		}
	}
	return true, events, nil
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
