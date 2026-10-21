package main

// The existing telemetry API provides shared reads without runtime control.
// App callbacks and authenticated API clients use this same policy and query.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type telemetryPrincipal struct {
	installID, userID                   int64
	project                             string
	observer, content, runtimes, agents bool
	ownedRuntimes                       bool
	legacy                              bool // compatibility scope for the original live-only clients
}
type telemetryAgent struct {
	project, runtime string
	runtimeAgent     *EnvironmentAgent
	agent            *Agent
}

func (s *Server) telemetryServerID() string {
	secret := s.instanceSecret
	if secret == "" {
		secret = string(s.secret)
	}
	sum := sha256.Sum256([]byte("observation-server:" + secret))
	return "server_" + hex.EncodeToString(sum[:12])
}
func (s *Server) telemetryPrincipal(w http.ResponseWriter, r *http.Request, installID int64, legacy ...bool) (telemetryPrincipal, bool) {
	p := telemetryPrincipal{installID: installID, userID: getUserID(r), project: r.URL.Query().Get("project_id"), observer: installID == 0, content: installID == 0, runtimes: installID == 0, agents: installID == 0}
	p.legacy = len(legacy) > 0 && legacy[0]
	if installID != 0 {
		// Apps subscribe during OnMount before readiness promotes pending to running.
		// Disabled/error installations remain ineligible on initial and periodic checks.
		var project, status string
		if s.store.db.QueryRow(`SELECT installed_by,COALESCE(project_id,''),status FROM app_installs WHERE id=?`, installID).Scan(&p.userID, &project, &status) != nil || p.userID == 0 || (status != "running" && status != "pending") {
			http.Error(w, "active installation required", 401)
			return p, false
		}
		if project != "" {
			if p.project != "" && p.project != project {
				http.Error(w, "installation project scope mismatch", 403)
				return p, false
			}
			p.project = project
		}
		if p.project == "" && !p.legacy {
			http.Error(w, "project_id required for global app telemetry", 400)
			return p, false
		}
		p.observer = installHasPermission(s, installID, sdk.PermTelemetryObserve)
		p.content = installHasPermission(s, installID, sdk.PermTelemetryRead)
		p.runtimes = installHasPermission(s, installID, sdk.PermRuntimesObserve)
		p.ownedRuntimes = installHasPermission(s, installID, sdk.PermRuntimesRead) || installHasPermission(s, installID, sdk.PermRuntimesManage) || installHasPermission(s, installID, sdk.PermRuntimesCall)
		p.agents = installHasPermission(s, installID, sdk.PermInstancesRead)
	}
	if p.userID == 0 {
		http.Error(w, "unauthorized", 401)
		return p, false
	}
	if p.project != "" && s.effectiveRoleOnProject(p.userID, p.project).Rank() < ProjectViewer.Rank() {
		http.Error(w, "project telemetry denied", 403)
		return p, false
	}
	if principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal); ok && principal.userSession && principal.userID != p.userID {
		if p.project == "" || s.effectiveRoleOnProject(principal.userID, p.project).Rank() < ProjectViewer.Rank() {
			http.Error(w, "viewer project telemetry denied", 403)
			return p, false
		}
	}
	return p, true
}
func (s *Server) canObserveRuntime(p telemetryPrincipal, rt *Environment) bool {
	if p.project != "" && rt.ProjectID != p.project {
		return false
	}
	if s.effectiveRoleOnProject(p.userID, rt.ProjectID).Rank() < ProjectViewer.Rank() {
		return false
	}
	if p.installID == 0 || p.runtimes {
		return true
	}
	return rt.OwnerInstallID() == p.installID && p.ownedRuntimes
}
func (s *Server) telemetryAgents(p telemetryPrincipal, q sdk.TelemetryQuery) (map[int64]telemetryAgent, error) {
	out := map[int64]telemetryAgent{}
	if p.legacy {
		// Preserve the original owned-agent scope without per-agent DB reads.
		where := `user_id=? AND (?='' OR COALESCE(project_id,'')=?)`
		if p.installID == 0 {
			where = `user_id=? AND COALESCE(kind,'user') IN ('user','platform_helper') AND (?='' OR project_id=? OR kind='platform_helper')`
		}
		where += ` AND (COALESCE(project_id,'')='' OR ? OR EXISTS(SELECT 1 FROM project_members WHERE project_members.project_id=agents.project_id AND project_members.user_id=agents.user_id))`
		rows, err := s.store.db.Query(`SELECT id,user_id,name,COALESCE(project_id,''),status FROM agents WHERE `+where, p.userID, p.project, p.project, s.store.GetPlatformRole(p.userID) == PlatformAdmin)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			a := new(Agent)
			if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.ProjectID, &a.Status); err != nil {
				return nil, err
			}
			if q.AgentID != 0 && q.AgentID != a.ID {
				continue
			}
			out[a.ID] = telemetryAgent{project: a.ProjectID, agent: a}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if q.AgentID != 0 {
			if _, ok := out[q.AgentID]; !ok {
				return nil, fmt.Errorf("agent not found in telemetry scope")
			}
		}
		return out, nil
	}
	if q.RuntimeID == "" {
		visible, e := s.store.ListVisibleAgents(p.userID)
		if e != nil {
			return nil, e
		}
		for _, a := range visible {
			if p.project != "" && a.ProjectID != p.project {
				continue
			}
			if p.installID != 0 && (!p.observer || q.Projection == "content") && a.UserID != p.userID {
				continue
			}
			if q.AgentID == 0 || q.AgentID == a.ID {
				out[a.ID] = telemetryAgent{project: a.ProjectID, agent: &a}
			}
		}
	}
	found := q.RuntimeID == ""
	if s.environments != nil {
		for _, rt := range s.environments.List() {
			if q.RuntimeID != "" && rt.ID != q.RuntimeID {
				continue
			}
			if !s.canObserveRuntime(p, rt) {
				continue
			}
			if q.Projection == "content" && p.installID != 0 && rt.creatorUserID != p.userID {
				continue
			}
			found = true
			for _, a := range rt.Agents() {
				if q.AgentID == 0 || q.AgentID == a.AgentID {
					out[a.AgentID] = telemetryAgent{project: rt.ProjectID, runtime: rt.ID, runtimeAgent: a}
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("runtime not found in telemetry scope")
	}
	if q.AgentID != 0 {
		if _, ok := out[q.AgentID]; !ok {
			return nil, fmt.Errorf("agent not found in telemetry scope")
		}
	}
	return out, nil
}
func parseTelemetryQuery(v url.Values) (sdk.TelemetryQuery, error) {
	q := sdk.TelemetryQuery{ProjectID: v.Get("project_id"), RuntimeID: v.Get("runtime_id"), ThreadID: v.Get("thread_id"), ThreadPrefix: v.Get("thread_prefix"), Cursor: v.Get("cursor"), Projection: v.Get("projection"), Limit: 200}
	if q.Projection == "" {
		q.Projection = "metadata"
	}
	if q.Projection != "metadata" && q.Projection != "content" {
		return q, fmt.Errorf("projection must be metadata or content")
	}
	var e error
	if raw := v.Get("agent_id"); raw != "" {
		q.AgentID, e = strconv.ParseInt(raw, 10, 64)
		if e != nil || q.AgentID <= 0 {
			return q, fmt.Errorf("invalid agent_id")
		}
	}
	if raw := v.Get("limit"); raw != "" {
		q.Limit, e = strconv.Atoi(raw)
		if e != nil || q.Limit < 1 || q.Limit > 500 {
			return q, fmt.Errorf("limit must be between 1 and 500")
		}
	}
	if raw := v.Get("since"); raw != "" {
		q.Since, e = time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			return q, fmt.Errorf("invalid since")
		}
	}
	if raw := v.Get("until"); raw != "" {
		q.Until, e = time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			return q, fmt.Errorf("invalid until")
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Until.Before(q.Since) {
		return q, fmt.Errorf("until precedes since")
	}
	for _, event := range strings.Split(v.Get("events"), ",") {
		if event = strings.TrimSpace(event); event != "" {
			if len(event) > 128 {
				return q, fmt.Errorf("invalid event type")
			}
			q.Events = append(q.Events, event)
		}
	}
	sort.Strings(q.Events)
	if len(q.Events) > 32 || len(q.ThreadID) > 256 || len(q.ThreadPrefix) > 256 || len(q.RuntimeID) > 128 || len(q.Cursor) > 4096 {
		return q, fmt.Errorf("telemetry filter exceeds limit")
	}
	return q, nil
}
func telemetryFingerprint(q sdk.TelemetryQuery) string {
	q.Cursor = ""
	q.Limit = 0
	raw, _ := json.Marshal(q)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
func (p telemetryPrincipal) telemetryAllowed(q sdk.TelemetryQuery) bool {
	return (p.observer || p.content) && (q.Projection != "content" || p.content)
}

var errTelemetryCursor = errors.New("invalid history cursor or changed filters")

type telemetryHistoryCursor struct {
	Boundary int64  `json:"b"`
	Before   int64  `json:"n"`
	Query    string `json:"q"`
}

func (s *Server) queryTelemetryPage(r *http.Request, q sdk.TelemetryQuery, agents map[int64]telemetryAgent) (sdk.TelemetryPage, error) {
	out := sdk.TelemetryPage{ServerID: s.telemetryServerID(), Events: []sdk.TelemetryStreamEvent{}, Order: "newest_ingestion_first"}
	cursor := telemetryHistoryCursor{Query: telemetryFingerprint(q)}
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != telemetryFingerprint(q) || cursor.Boundary < 0 || cursor.Before < 1 || cursor.Before > cursor.Boundary+1 {
			return out, errTelemetryCursor
		}
	}
	if len(agents) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if q.Cursor == "" {
		if e := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM telemetry`).Scan(&cursor.Boundary); e != nil {
			return out, e
		}
		cursor.Before = cursor.Boundary + 1
	}
	ids := make([]int64, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	where, args := telemetryHistoryFilter(ids, q)
	where += ` AND rowid<=? AND rowid<?`
	args = append(args, cursor.Boundary, cursor.Before, q.Limit+1)
	rows, e := tx.QueryContext(ctx, `SELECT `+telemetryReadColumns+` FROM telemetry WHERE `+where+` ORDER BY rowid DESC LIMIT ?`, args...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	pageBytes := 0
	for rows.Next() {
		row, ev, e := scanTelemetryRow(rows)
		if e != nil {
			return out, e
		}
		if len(out.Events) == q.Limit {
			raw, _ := json.Marshal(cursor)
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		event := s.projectTelemetryEvent(ev, agents[ev.AgentID], q.Projection)
		encoded, _ := json.Marshal(event)
		if pageBytes+len(encoded) > 2<<20 {
			if len(out.Events) == 0 {
				return out, fmt.Errorf("event identity exceeds response limit")
			}
			raw, _ := json.Marshal(cursor)
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		pageBytes += len(encoded)
		out.Events = append(out.Events, event)
		cursor.Before = row
	}
	return out, rows.Err()
}

// Metadata is an allowlist, never a generic payload scrubber. Arbitrary strings,
// errors, labels, prompts and nested JSON are intentionally excluded.
func (s *Server) projectTelemetryEvent(ev TelemetryEvent, a telemetryAgent, projection string) sdk.TelemetryStreamEvent {
	out := sdk.TelemetryStreamEvent{ServerID: s.telemetryServerID(), ProjectID: a.project, RuntimeID: a.runtime, ID: ev.ID, AgentID: ev.AgentID, ThreadID: ev.ThreadID, Type: ev.Type, Time: ev.Time, Seq: ev.Seq}
	if projection == "content" {
		out.Data = publicTelemetryEvent(ev).Data
		if len(out.Data) > 256<<10 {
			out.Data, _ = json.Marshal(map[string]any{"content_omitted": true, "reason": "event_size_limit", "bytes": len(out.Data)})
		}
		return out
	}
	safe := map[string]any{}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(ev.Data))
	decoder.UseNumber()
	_ = decoder.Decode(&raw)
	for _, key := range []string{"duration_ms", "tokens_in", "tokens_out", "cost_usd"} {
		if n, ok := raw[key].(json.Number); ok {
			if _, err := n.Float64(); err == nil {
				safe[key] = n
			}
		}
	}
	if success, ok := raw["success"].(bool); ok {
		safe["success"] = success
	}
	if target, ok := raw["target_agent_id"].(json.Number); ok {
		if id, err := target.Int64(); err == nil && id > 0 {
			safe["target_agent_id"] = id
		}
	}
	for _, key := range []string{"parent_thread_id", "target_thread_id", "call_id", "tool_call_id", "proxy_connection_id", "trace_id", "execution_id", "parent_execution_id", "request_id", "tool_span_id"} {
		if id, ok := raw[key].(string); ok && telemetryIdentifier(id) {
			safe[key] = id
		}
	}

	if strings.HasPrefix(ev.Type, "tool.") {
		for _, key := range []string{"name", "tool", "tool_name"} {
			if name, ok := raw[key].(string); ok && telemetryIdentifier(name) {
				safe["tool_name"] = name
				break
			}
		}
	}
	if status, ok := raw["status"].(string); ok && runtimeContains([]string{"running", "queued", "waiting", "done", "succeeded", "failed", "stopped", "canceled", "cancelled", "error"}, status) {
		safe["status"] = status
	}
	out.Data, _ = json.Marshal(safe)
	return out
}
func telemetryIdentifier(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.:/", r)) {
			return false
		}
	}
	return true
}
func (s *Server) handleTelemetryRead(w http.ResponseWriter, r *http.Request, installID int64, suffix string) {
	if r.Method != "GET" {
		http.Error(w, "read-only telemetry: GET only", 405)
		return
	}
	stream := suffix == "/stream"
	// The original app subscription is live-only content. Modern resumable
	// reads always use the explicit project-scoped policy.
	legacy := stream && r.URL.Query().Get("format") != "delivery"
	if legacy && r.URL.Query().Get("projection") == "" {
		r = r.Clone(r.Context())
		u := *r.URL
		r.URL = &u
		v := r.URL.Query()
		v.Set("projection", "content")
		r.URL.RawQuery = v.Encode()
	}
	var sinceSeq uint64
	if stream {
		r = r.Clone(r.Context())
		u := *r.URL
		r.URL = &u
		v := r.URL.Query()
		if v.Get("agent_id") == "" && v.Get("instance_id") != "" {
			v.Set("agent_id", v.Get("instance_id"))
		}
		if v.Get("format") != "delivery" {
			raw := v.Get("since")
			if raw == "" {
				raw = r.Header.Get("Last-Event-ID")
			}
			var err error
			sinceSeq, err = strconv.ParseUint(raw, 10, 64)
			if raw != "" && err == nil {
				v.Del("since")
			}
		} else if v.Get("cursor") == "" {
			v.Set("cursor", r.Header.Get("Last-Event-ID"))
		}
		r.URL.RawQuery = v.Encode()
	}
	p, ok := s.telemetryPrincipal(w, r, installID, legacy)
	if !ok {
		return
	}
	switch suffix {
	case "/history", "/stream":
		q, e := parseTelemetryQuery(r.URL.Query())
		if e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		q.ProjectID = p.project
		if !p.telemetryAllowed(q) {
			http.Error(w, "missing telemetry observation/content permission", 403)
			return
		}
		agents, e := s.telemetryAgents(p, q)
		if e != nil {
			http.Error(w, e.Error(), 404)
			return
		}
		if stream {
			if sinceSeq > 0 && q.Cursor == "" && s.broadcaster != nil {
				q.Cursor = encodeTelemetryCursor(s.broadcaster.replayEpoch, sinceSeq, q)
			}
			s.streamTelemetry(w, r, p, q)
			return
		}
		out, e := s.queryTelemetryPage(r, q, agents)
		if e != nil {
			if errors.Is(e, errTelemetryCursor) {
				http.Error(w, e.Error(), 400)
			} else {
				http.Error(w, "telemetry query unavailable", 503)
			}
			return
		}
		writeJSON(w, out)
	case "/agents":
		if !p.agents {
			http.Error(w, "missing agent inspection permission", 403)
			return
		}
		scope, e := s.telemetryAgents(p, sdk.TelemetryQuery{ProjectID: p.project})
		if e != nil {
			http.Error(w, "agent inventory unavailable", 500)
			return
		}
		out := []sdk.ObservedAgent{}
		for id, a := range scope {
			entry := sdk.ObservedAgent{ServerID: s.telemetryServerID(), ID: id, ProjectID: a.project, RuntimeID: a.runtime}
			if a.runtimeAgent != nil {
				entry.Name = a.runtimeAgent.Alias
				entry.Status = s.publicRuntimeAgent(a.runtimeAgent).Status
			} else if a.agent != nil {
				entry.Name = a.agent.Name
				entry.Status = a.agent.Status
			}
			out = append(out, entry)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		writeJSON(w, out)
	case "/runtimes":
		if p.installID != 0 && !p.runtimes && !p.ownedRuntimes {
			http.Error(w, "missing runtime observation permission", 403)
			return
		}
		out := []sdk.ObservedRuntime{}
		if s.environments != nil {
			for _, rt := range s.environments.List() {
				if s.canObserveRuntime(p, rt) {
					out = append(out, s.observedRuntime(rt))
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		writeJSON(w, out)
	default:
		if strings.HasPrefix(suffix, "/runtimes/") {
			id := strings.TrimPrefix(suffix, "/runtimes/")
			if !validRuntimeID(id) || s.environments == nil {
				http.NotFound(w, r)
				return
			}
			rt, exists := s.environments.Get(id)
			if !exists || !s.canObserveRuntime(p, rt) {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, s.observedRuntime(rt))
			return
		}
		if strings.HasPrefix(suffix, "/agents/") && strings.HasSuffix(suffix, "/topology") {
			s.handleTelemetryTopology(w, r, p, suffix)
			return
		}
		http.NotFound(w, r)
	}
}
func (s *Server) observedRuntime(rt *Environment) sdk.ObservedRuntime {
	rt.mu.Lock()
	stopped := rt.stopped
	rt.mu.Unlock()
	status := "running"
	if stopped {
		status = "stopped"
	}
	return sdk.ObservedRuntime{ServerID: s.telemetryServerID(), ID: rt.ID, ProjectID: rt.ProjectID, Status: status, Agents: s.publicRuntimeAgents(rt), Apps: s.runtimeApps(rt), CreatedAt: rt.createdAt, ExpiresAt: rt.ExpiresAt()}
}

// Inventory and topology are read-only resources on the existing telemetry API.
func (s *Server) registerTelemetryReadRoutes(mux *http.ServeMux) {
	for _, path := range []string{"/telemetry/agents", "/telemetry/agents/", "/telemetry/runtimes", "/telemetry/runtimes/"} {
		mux.HandleFunc(path, s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			s.handleTelemetryRead(w, r, 0, strings.TrimPrefix(r.URL.Path, "/telemetry"))
		}))
	}
}
