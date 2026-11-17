package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// The parent HTTP gateway retains caller identity; the subprocess never gets
// to choose an agent or conversation notification destination.
func (s *Server) inspectPlatformInstallWait(agent *Agent, thread, project string, body []byte) *appMCPAsyncRequest {
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &rpc) != nil || rpc.Method != "tools/call" {
		return nil
	}
	name := rpc.Params.Name
	watch, _ := rpc.Params.Arguments["watch"].(bool)
	if name != "apps_install" && !(name == "apps_get" && watch) {
		return nil
	}
	req := &appMCPAsyncRequest{ToolName: name, AgentID: agent.ID, UserID: agent.UserID, ThreadID: thread, ProjectID: project, StartedAt: time.Now(), Spec: &sdk.AsyncResultSpec{IDField: "operation_id", Notify: &sdk.AsyncNotifySpec{Target: "caller", Mode: "once", Events: []string{"runtime.operation.succeeded", "runtime.operation.failed", "runtime.operation.cancelled"}, Match: map[string]string{"id": "$result.operation_id"}, ExpiresAfter: "1h"}}}
	if thread == "" && agent.Kind == "platform_helper" {
		req.SetupError = fmt.Errorf("originating Helper conversation is unavailable")
	} else {
		if req.ThreadID == "" {
			req.ThreadID = "main"
		}
		req.Cursor, req.ThreadEpoch, req.SetupError = s.store.asyncSubscriptionCursor(agent.ID, req.ThreadID)
	}
	return req
}

func (s *Server) augmentPlatformInstallWait(req *appMCPAsyncRequest, body []byte) ([]byte, error) {
	if req == nil {
		return body, nil
	}
	var rpc map[string]any
	if json.Unmarshal(body, &rpc) != nil {
		return body, nil
	}
	result, _ := rpc["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		return body, nil
	}
	blocks, _ := result["content"].([]any)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		value, _ := block["text"].(string)
		var receipt appReadiness
		if json.Unmarshal([]byte(value), &receipt) != nil || receipt.InstallID <= 0 {
			continue
		}
		if receipt.OperationID == "" || receipt.Ready || receipt.State == "failed" || receipt.State == "cancelled" || receipt.State == "needs_configuration" {
			var obj map[string]any
			json.Unmarshal([]byte(value), &obj)
			obj["_apteva_async"] = map[string]any{"will_notify": false, "instruction": "Inspect ready, state and missing_setup. This result does not require an automatic wait; do not repeat installation."}
			encoded, _ := json.Marshal(obj)
			block["text"] = string(encoded)
			return json.Marshal(rpc)
		}
		entry := &InstalledApp{InstallID: receipt.InstallID, AppName: receipt.Name, ProjectID: receipt.ProjectID}
		if receipt.ProjectID != "" && req.ProjectID != receipt.ProjectID {
			req.SetupError = fmt.Errorf("installation notifications require the installation's conversation scope")
		}
		// Concurrent retries in one conversation share the existing bounded wait.
		unlock := lockAsyncThreadDelivery(req.AgentID, req.ThreadID)
		defer unlock()
		match := runtimeJSON(map[string]any{"id": receipt.OperationID})
		var id string
		if s.store.dbQueryRow(`SELECT id FROM subscriptions WHERE kind='ephemeral' AND user_id=? AND agent_id=? AND thread_id=? AND source_install_id=? AND project_id=? AND match_json=? ORDER BY created_at DESC LIMIT 1`, req.UserID, req.AgentID, req.ThreadID, receipt.InstallID, req.ProjectID, match).Scan(&id) == nil {
			if sub, err := s.store.GetSubscription(req.UserID, id); err == nil && !subscriptionExpired(sub) {
				req.ExistingSubscription = sub
			}
		}
		response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
		if err := s.maybeAugmentAppMCPAsyncResponse(entry, req, response); err != nil {
			return nil, err
		}
		// A global operation can complete before its project-scoped waiter is
		// registered. Rechecking durable state closes that window; event keys
		// deduplicate this against publication in saveOperation.
		if receipt.ProjectID == "" && req.ProjectID != "" && req.SetupError == nil {
			if op, err := s.appRuntimeManager().operation(receipt.OperationID); err == nil && runtimeOperationTerminal(op.Status) {
				ev, key := runtimeOperationEvent(op, receipt.Name, req.ProjectID, "runtime.operation."+op.Status)
				if _, err = s.queueAppSubscriptionsWithID(ev, key); err != nil {
					return nil, err
				}
				if s.appEventDispatcher != nil {
					s.appEventDispatcher.wakeOutbox()
				}
			}
		}
		return io.ReadAll(response.Body)
	}
	return body, nil
}

func platformInstallIsReadiness(args map[string]any) bool {
	view, _ := args["view"].(string)
	watch, _ := args["watch"].(bool)
	return strings.TrimSpace(view) == "readiness" || watch
}
