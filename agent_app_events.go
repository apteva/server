package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// An agent attached to an app that publishes events gets one extra tool on
// that app's MCP: app_events_subscribe. The server answers it; the app never
// sees the call. It creates an ordinary app-event subscription (kind "agent")
// with the same delivery options as create_subscription, but short-term by
// default: the next matching event, within an hour, in the calling thread.

const (
	appEventsSubscribeTool     = "app_events_subscribe"
	agentSubscriptionKind      = "agent"
	agentSubscriptionTTL       = time.Hour
	maxAgentSubscriptions      = 50
	maxListedAppEvents         = 60
	maxListedAppEventDescLen   = 160
	maxListedAppEventFieldsLen = 160
)

// appEventsSubscribeAvailable reports whether the app gets the tool: it
// declares events and does not already have a tool of that name.
func appEventsSubscribeAvailable(entry *InstalledApp) bool {
	if entry == nil || entry.StaticDir != "" || len(entry.Manifest.Provides.Publishes) == 0 {
		return false
	}
	for _, tool := range entry.Manifest.Provides.MCPTools {
		if tool.Name == appEventsSubscribeTool {
			return false
		}
	}
	return true
}

func appEventsDisplayName(entry *InstalledApp) string {
	if name := strings.TrimSpace(entry.Manifest.DisplayName); name != "" {
		return name
	}
	return entry.AppName
}

// appEventsSubscribeToolDef is the tool as listed to the agent. The event
// list is the app's declared publishes, so the agent never has to guess.
func appEventsSubscribeToolDef(entry *InstalledApp) map[string]any {
	app := entry.AppName
	var lines []string
	names := make([]any, 0, len(entry.Manifest.Provides.Publishes))
	dynamic := false
	for i, ev := range entry.Manifest.Provides.Publishes {
		name := strings.TrimSpace(ev.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
		if ev.Dynamic || strings.HasSuffix(name, ".*") {
			dynamic = true
		}
		if i >= maxListedAppEvents {
			continue
		}
		line := "- " + name
		if desc := strings.TrimSpace(ev.Description); desc != "" {
			line += ": " + truncate(desc, maxListedAppEventDescLen)
		}
		if len(ev.Payload) > 0 {
			fields := make([]string, 0, len(ev.Payload))
			for field := range ev.Payload {
				fields = append(fields, field)
			}
			sort.Strings(fields)
			line += " (fields: " + truncate(strings.Join(fields, ", "), maxListedAppEventFieldsLen) + ")"
		}
		lines = append(lines, line)
	}
	if extra := len(names) - maxListedAppEvents; extra > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more", extra))
	}
	description := fmt.Sprintf("Get %s events in a thread. By default you get only the next matching event, within 1 hour, in the thread that calls this; "+
		"pass once=false and a longer expires_in_seconds to keep receiving them. Subscribe before you start what you are waiting for, and use filters to pick one item. "+
		"Each event arrives in the thread as a message: [app:%s:<event>] <event data as JSON>. Each call adds one subscription and returns its id; "+
		"the same call again returns the same subscription. Pass cancel with an id to end one early.\n\nEvents:\n%s",
		appEventsDisplayName(entry), app, strings.Join(lines, "\n"))
	eventItems := map[string]any{"type": "string"}
	eventsDesc := "Event names from the list in this tool's description."
	if dynamic {
		eventsDesc += " A name ending in .* matches any event under that prefix; you can also name one event under it."
	} else {
		eventItems["enum"] = names
	}
	return map[string]any{
		"name":        appEventsSubscribeTool,
		"description": description,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"events": map[string]any{"type": "array", "items": eventItems, "description": eventsDesc},
				"filters": map[string]any{"type": "object", "description": "Deliver only events whose data fields all match. Keys are field names or dotted paths. " +
					"A value matches exactly; an array field matches if any element does. Or use one operator: {\"prefix\":...}, {\"suffix\":...} or {\"contains\":...} (text, ignoring case), or {\"in\":[...]}"},
				"thread_id":          map[string]any{"type": "string", "description": "Existing thread that receives the events; defaults to the thread that calls this"},
				"once":               map[string]any{"type": "boolean", "description": "End the subscription after its first matching event. Default true"},
				"expires_in_seconds": map[string]any{"type": "integer", "description": "End the subscription after this many seconds. Default 3600, at most a year"},
				"expires_at":         map[string]any{"type": "string", "description": "End the subscription at this RFC3339 time, instead of expires_in_seconds"},
				"cancel":             map[string]any{"type": "string", "description": "A subscription id returned earlier: end that subscription now. Other arguments are ignored"},
			},
		},
	}
}

// appendAppEventsSubscribeTool adds the tool to a JSON tools/list reply.
// Streaming replies and later pages are left alone.
func appendAppEventsSubscribeTool(entry *InstalledApp, resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") ||
		resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	raw, err := readResponseBody(resp)
	if err != nil {
		return err
	}
	var reply map[string]any
	if decodeMCPProxyRequest(raw, &reply) != nil {
		return nil
	}
	result, _ := reply["result"].(map[string]any)
	if result == nil {
		return nil
	}
	tools, _ := result["tools"].([]any)
	for _, tool := range tools {
		if m, _ := tool.(map[string]any); m != nil && m["name"] == appEventsSubscribeTool {
			return nil
		}
	}
	result["tools"] = append(tools, appEventsSubscribeToolDef(entry))
	encoded, err := json.Marshal(reply)
	if err != nil {
		return nil
	}
	setResponseBody(resp, encoded)
	return nil
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	setResponseBody(resp, raw)
	return raw, err
}

// appEventsMCPRequest is the part of an app MCP request this feature reads.
type appEventsMCPRequest struct {
	body   []byte
	method string
	cursor bool
	tool   string
	args   map[string]any
}

func peekAppEventsMCPRequest(r *http.Request) *appEventsMCPRequest {
	body, err := readRequestBody(r)
	if err != nil || len(body) == 0 {
		return nil
	}
	var rpc map[string]any
	if decodeMCPProxyRequest(body, &rpc) != nil {
		return nil
	}
	req := &appEventsMCPRequest{body: body}
	req.method, _ = rpc["method"].(string)
	params, _ := rpc["params"].(map[string]any)
	if params != nil {
		if c, _ := params["cursor"].(string); c != "" {
			req.cursor = true
		}
		req.tool, _ = params["name"].(string)
		req.args, _ = params["arguments"].(map[string]any)
	}
	if req.args == nil {
		req.args = map[string]any{}
	}
	return req
}

type agentAppEventsCall struct {
	entry        *InstalledApp
	agentID      int64
	projectID    string
	callerThread string
	args         map[string]any
}

// handleAppEventsSubscribe answers the tool call locally as MCP JSON.
func (s *Server) handleAppEventsSubscribe(w http.ResponseWriter, body []byte, call agentAppEventsCall) {
	result, err := s.runAppEventsSubscribe(call)
	if err != nil {
		writePlatformGatewayToolError(w, body, err)
		return
	}
	text, _ := json.Marshal(result)
	var rpc map[string]any
	_ = json.Unmarshal(body, &rpc)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      rpc["id"],
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
		},
	})
}

func (s *Server) runAppEventsSubscribe(call agentAppEventsCall) (map[string]any, error) {
	entry := call.entry
	attached, err := s.store.agentAttachedToInstall(call.agentID, entry.InstallID)
	if err != nil {
		return nil, err
	}
	if !attached {
		return nil, fmt.Errorf("this agent is not attached to %s", entry.AppName)
	}
	agent, err := s.store.GetAgentByID(call.agentID)
	if err != nil || agent == nil {
		return nil, fmt.Errorf("agent %d not found", call.agentID)
	}
	if raw, ok := call.args["cancel"]; ok && raw != nil {
		id, _ := raw.(string)
		return s.cancelAgentAppEventSubscription(entry, agent, strings.TrimSpace(id))
	}

	events, err := agentAppEventNames(entry, call.args["events"])
	if err != nil {
		return nil, err
	}
	threadID, err := s.agentAppEventThread(agent, call)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expiresIn := call.args["expires_in_seconds"]
	if n, ok := expiresIn.(json.Number); ok {
		// The proxy decodes numbers exactly; the shared parser takes integers.
		v, err := n.Int64()
		if err != nil {
			return nil, errors.New("expires_in_seconds must be a positive integer")
		}
		expiresIn = v
	}
	options := map[string]any{"filters": call.args["filters"], "once": true,
		"expires_in_seconds": expiresIn, "expires_at": call.args["expires_at"]}
	if once, ok := call.args["once"]; ok && once != nil {
		options["once"] = once
	}
	expiresAt, _ := call.args["expires_at"].(string)
	if call.args["expires_in_seconds"] == nil && strings.TrimSpace(expiresAt) == "" {
		options["expires_in_seconds"] = int64(agentSubscriptionTTL / time.Second)
	}
	opts, err := parseSubscriptionDeliveryOptions(options, now)
	if err != nil {
		return nil, err
	}

	eventsJSON, _ := json.Marshal(compactSubscriptionEvents(events))
	var existingID, existingExpiry string
	err = s.store.dbQueryRow(`SELECT id, COALESCE(expires_at,'') FROM subscriptions
 WHERE kind=? AND agent_id=? AND source_install_id=? AND project_id=? AND thread_id=? AND events=?
   AND COALESCE(match_json,'')=? AND delete_on_match=? AND enabled=1
   AND (COALESCE(expires_at,'')='' OR expires_at > datetime('now'))
 ORDER BY created_at LIMIT 1`,
		agentSubscriptionKind, agent.ID, entry.InstallID, call.projectID, threadID, string(eventsJSON),
		opts.MatchJSON, boolToInt(opts.DeleteOnMatch)).Scan(&existingID, &existingExpiry)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existingID != "" {
		// The same call again keeps one subscription; a later expiry extends it.
		if want := opts.expiresAtValue(); want > existingExpiry {
			if _, err := s.store.dbExec(`UPDATE subscriptions SET expires_at=? WHERE id=?`, want, existingID); err != nil {
				return nil, err
			}
		}
		sub, err := s.store.GetSubscription(agent.UserID, existingID)
		if err != nil || sub == nil {
			return nil, fmt.Errorf("load subscription %s: %v", existingID, err)
		}
		out := agentAppEventSubscriptionResult(entry, sub)
		out["existing"] = true
		return out, nil
	}

	var active int
	if err := s.store.dbQueryRow(`SELECT count(*) FROM subscriptions WHERE kind=? AND agent_id=?
 AND (COALESCE(expires_at,'')='' OR expires_at > datetime('now'))`, agentSubscriptionKind, agent.ID).Scan(&active); err != nil {
		return nil, err
	}
	if active >= maxAgentSubscriptions {
		return nil, fmt.Errorf("this agent already has %d active app event subscriptions; cancel one first", active)
	}

	name := appEventsDisplayName(entry) + " events"
	sub, err := s.store.CreateAppEventSubscriptionWithOptions(appEventSubscriptionSpec{
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		SourceInstallID: entry.InstallID,
		Name:            name,
		Slug:            entry.AppName + ":*",
		Description:     fmt.Sprintf("Created by the agent for thread %s", threadID),
		ThreadID:        threadID,
		Kind:            agentSubscriptionKind,
		ProjectID:       call.projectID,
		Events:          events,
		// The agent made it, so a notice is only useful when a standing
		// subscription ends on its own.
		NotifyAgent: !opts.DeleteOnMatch,
		Options:     opts,
	})
	if err != nil {
		return nil, err
	}
	if s.appEventDispatcher != nil {
		if err := s.appEventDispatcher.Reconcile(); err != nil {
			log.Printf("[APP-SUB] reconcile after agent subscribe: %v", err)
		}
	}
	log.Printf("[APP-SUB] agent=%d subscribed sub=%s app=%s install=%d project=%s thread=%s events=%v once=%v expires=%q",
		agent.ID, sub.ID, entry.AppName, entry.InstallID, call.projectID, threadID, sub.Events, sub.DeleteOnMatch, sub.ExpiresAt)
	return agentAppEventSubscriptionResult(entry, sub), nil
}

func (s *Server) cancelAgentAppEventSubscription(entry *InstalledApp, agent *Agent, id string) (map[string]any, error) {
	if id == "" {
		return nil, errors.New("cancel needs a subscription id")
	}
	res, err := s.store.dbExec(`DELETE FROM subscriptions WHERE id=? AND kind=? AND agent_id=? AND source_install_id=?`,
		id, agentSubscriptionKind, agent.ID, entry.InstallID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("no subscription %s from this agent on %s; it may have ended already", id, entry.AppName)
	}
	if s.appEventDispatcher != nil {
		_ = s.appEventDispatcher.Reconcile()
	}
	log.Printf("[APP-SUB] agent=%d canceled sub=%s app=%s", agent.ID, id, entry.AppName)
	return map[string]any{"canceled": id}, nil
}

// agentAppEventNames accepts only events the app declares. A dynamic
// declaration (name ending in .*) also accepts one event under its prefix.
func agentAppEventNames(entry *InstalledApp, raw any) ([]string, error) {
	var requested []string
	switch v := raw.(type) {
	case string:
		requested = []string{v}
	case []any:
		for _, item := range v {
			name, ok := item.(string)
			if !ok {
				return nil, errors.New("events must be a list of event names")
			}
			requested = append(requested, name)
		}
	case nil:
	default:
		return nil, errors.New("events must be a list of event names")
	}
	requested = compactSubscriptionEvents(requested)
	sort.Strings(requested)
	if len(requested) == 0 {
		return nil, errors.New("name at least one event to subscribe to")
	}
	declared := map[string]bool{}
	var prefixes, valid []string
	for _, ev := range entry.Manifest.Provides.Publishes {
		name := strings.TrimSpace(ev.Name)
		if name == "" {
			continue
		}
		declared[name] = true
		valid = append(valid, name)
		if prefix, ok := strings.CutSuffix(name, "*"); ok && strings.HasSuffix(prefix, ".") {
			prefixes = append(prefixes, prefix)
		}
	}
	for _, name := range requested {
		if declared[name] {
			continue
		}
		matched := false
		for _, prefix := range prefixes {
			if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && !strings.ContainsAny(rest, "*") {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("%s does not publish %q; its events are: %s", entry.AppName, name, strings.Join(valid, ", "))
		}
	}
	return requested, nil
}

// agentAppEventThread is the thread that receives the events: the one the
// agent names, or else the calling thread. A named thread must exist, so a
// delivery never creates an empty thread.
func (s *Server) agentAppEventThread(agent *Agent, call agentAppEventsCall) (string, error) {
	caller := strings.TrimSpace(call.callerThread)
	if caller == "" {
		caller = "main"
	}
	threadID := caller
	if raw, ok := call.args["thread_id"]; ok && raw != nil {
		named, isString := raw.(string)
		if !isString {
			return "", errors.New("thread_id must be a string")
		}
		if named = strings.TrimSpace(named); named != "" {
			threadID = named
		}
	}
	if !validDeviceThreadID(threadID) {
		return "", fmt.Errorf("invalid thread_id %q", threadID)
	}
	if threadID == caller || threadID == "main" {
		return threadID, nil
	}
	exists, err := s.asyncThreadExists(&Subscription{AgentID: agent.ID, ThreadID: threadID})
	if err != nil {
		return "", fmt.Errorf("check thread %s: %v", threadID, err)
	}
	if !exists {
		return "", fmt.Errorf("thread %s does not exist; spawn it first, or leave thread_id out to use this thread", threadID)
	}
	return threadID, nil
}

func agentAppEventSubscriptionResult(entry *InstalledApp, sub *Subscription) map[string]any {
	out := map[string]any{
		"subscription_id": sub.ID,
		"app":             entry.AppName,
		"events":          sub.Events,
		"thread_id":       sub.ThreadID,
		"once":            sub.DeleteOnMatch,
	}
	for k, v := range subscriptionDeliveryOptionsSummary(sub) {
		out[k] = v
	}
	expiresAt, _ := out["expires_at"].(string)
	format := fmt.Sprintf("[app:%s:<event>] <data>", entry.AppName)
	switch {
	case sub.DeleteOnMatch && expiresAt != "":
		out["delivery"] = fmt.Sprintf("The next matching event arrives in thread %s as %s; then this subscription ends. If none comes by %s, it ends then.", sub.ThreadID, format, expiresAt)
	case sub.DeleteOnMatch:
		out["delivery"] = fmt.Sprintf("The next matching event arrives in thread %s as %s; then this subscription ends.", sub.ThreadID, format)
	case expiresAt != "":
		out["delivery"] = fmt.Sprintf("Matching events arrive in thread %s as %s until %s.", sub.ThreadID, format, expiresAt)
	default:
		out["delivery"] = fmt.Sprintf("Matching events arrive in thread %s as %s.", sub.ThreadID, format)
	}
	return out
}

func (s *Store) agentAttachedToInstall(agentID, installID int64) (bool, error) {
	var enabled int
	err := s.dbQueryRow(`SELECT enabled FROM app_agent_bindings WHERE install_id=? AND agent_id=?`, installID, agentID).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return enabled == 1, err
}
