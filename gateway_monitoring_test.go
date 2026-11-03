package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type monitoringFixture struct {
	s       *Server
	userID  int64
	project *Project
	writer  *Agent
	support *Agent
	now     time.Time
}

// seedMonitoring gives "writer" two failed tool results, one successful one,
// a provider attempt failure, a final LLM failure and an unrelated control
// error, and "support" one unpriced call on a worker thread.
func seedMonitoring(t *testing.T) monitoringFixture {
	t.Helper()
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	project, err := s.store.CreateProject(userID, "Ops", "", "")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := s.store.CreateAgent(userID, "writer", "", "cautious", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	support, err := s.store.CreateAgent(userID, "support", "", "cautious", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ev := func(agentID int64, thread, kind string, ago time.Duration, data map[string]any) TelemetryEvent {
		e := usageEvent(t, agentID, kind, now.Add(-ago), data)
		e.ThreadID = thread
		return e
	}
	refused := func(port int) string {
		return fmt.Sprintf(`error: http: Post "http://127.0.0.1:%d": dial tcp 127.0.0.1:%d: connect: connection refused`, port, port)
	}
	events := []TelemetryEvent{
		ev(writer.ID, "main", "llm.done", 50*time.Minute, map[string]any{"model": "claude-sonnet", "provider": "anthropic", "tokens_in": 1000, "tokens_cached": 250, "tokens_out": 100, "cost_usd": 0.5}),
		ev(writer.ID, "main", "tool.call", 40*time.Minute, map[string]any{"name": "crm_lookup"}),
		ev(writer.ID, "main", "tool.result", 40*time.Minute, map[string]any{"name": "crm_lookup", "success": false, "duration_ms": 120, "result": refused(53736)}),
		ev(writer.ID, "main", "tool.call", 30*time.Minute, map[string]any{"name": "crm_lookup"}),
		ev(writer.ID, "main", "tool.result", 30*time.Minute, map[string]any{"name": "crm_lookup", "success": false, "duration_ms": 80, "result": refused(41000)}),
		ev(writer.ID, "main", "tool.call", 20*time.Minute, map[string]any{"name": "crm_lookup"}),
		ev(writer.ID, "main", "tool.result", 20*time.Minute, map[string]any{"name": "crm_lookup", "success": true, "duration_ms": 40, "result": "ok"}),
		ev(writer.ID, "main", "llm.provider_error", 10*time.Minute, map[string]any{"model": "gpt-6", "provider": "openai", "role": "primary", "error": "OpenAI stream error (server_is_overloaded): try again"}),
		ev(writer.ID, "main", "llm.error", 9*time.Minute, map[string]any{"model": "gpt-6", "provider": "openai", "error": "OpenAI stream error (server_is_overloaded): try again"}),
		ev(writer.ID, "main", "proxy.websocket.control.error", 8*time.Minute, map[string]any{"error": "closed"}),
		ev(support.ID, "worker-1", "llm.done", 5*time.Minute, map[string]any{"model": "gpt-6", "provider": "openai", "tokens_in": 200, "tokens_out": 20}),
	}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	return monitoringFixture{s, userID, project, writer, support, now}
}

func asMap(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestErrorCountsShareOneDefinition(t *testing.T) {
	f := seedMonitoring(t)
	since := f.now.Add(-time.Hour)
	// Two failed tool results and one llm.error; the provider attempt and the
	// websocket control error are not errors.
	const want = 3

	stats, err := f.s.store.TelemetryStats(f.writer.ID, since)
	if err != nil || stats.Errors != want {
		t.Fatalf("stats errors=%v err=%v", stats, err)
	}
	timeline, err := f.s.store.TelemetryTimeline(f.writer.ID, since, 60)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, b := range timeline {
		sum += b.Errors
	}
	if sum != want {
		t.Fatalf("timeline errors=%d", sum)
	}
	byAgent, err := f.s.store.TelemetryStatsByProject(f.userID, f.project.ID, since)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range byAgent {
		if a.AgentID == f.writer.ID && a.Errors != want {
			t.Fatalf("project stats errors=%d", a.Errors)
		}
	}
	projectTimeline, err := f.s.store.TelemetryTimelineByProject(f.userID, f.project.ID, since, 60)
	if err != nil {
		t.Fatal(err)
	}
	sum = 0
	for _, b := range projectTimeline {
		sum += b.Errors
	}
	if sum != want {
		t.Fatalf("project timeline errors=%d", sum)
	}
	report, err := f.s.store.TelemetryUsageReport(context.Background(), f.userID, f.project.ID, rolling(t, "24h", f.now), "model", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Errors != want {
		t.Fatalf("usage errors=%d", report.Totals.Errors)
	}
	for _, g := range report.Groups {
		if g.Key == "unknown" {
			t.Fatalf("tool failures created a model group: %+v", g)
		}
	}
}

func TestTelemetryErrorMigrationBackfillsExistingRows(t *testing.T) {
	f := seedMonitoring(t)
	db := f.s.store.db
	// Return the database to its state before migration 8.
	insert, deletion := telemetryRollupTriggers(false)
	for _, q := range []string{
		`DROP TRIGGER telemetry_rollup_insert`, `DROP TRIGGER telemetry_rollup_delete`,
		`ALTER TABLE telemetry_rollups DROP COLUMN errors`, insert, deletion,
		`UPDATE telemetry_metrics SET is_error=0`, `DELETE FROM server_schema_migrations WHERE version=8`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := f.s.store.migrateTelemetryErrors(); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(q, err)
		}
		return n
	}
	if n := count(`SELECT SUM(is_error) FROM telemetry_metrics WHERE type='tool.result'`); n != 2 {
		t.Fatalf("backfilled tool failures=%d", n)
	}
	if n := count(`SELECT SUM(errors) FROM telemetry_rollups`); n != 2 {
		t.Fatalf("backfilled rollup errors=%d", n)
	}
	// The recreated triggers keep both tables current.
	failure := usageEvent(t, f.writer.ID, "tool.result", f.now.Add(-time.Minute), map[string]any{"name": "crm_lookup", "success": 0})
	failure.ID = "late-failure"
	if err := f.s.store.InsertTelemetry([]TelemetryEvent{failure}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT SUM(errors) FROM telemetry_rollups`); n != 3 {
		t.Fatalf("rollup errors after insert=%d", n)
	}
	if _, err := db.Exec(`DELETE FROM telemetry WHERE id='late-failure'`); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT SUM(errors) FROM telemetry_rollups`); n != 2 {
		t.Fatalf("rollup errors after delete=%d", n)
	}
}

func TestGatewayAgentsUsage(t *testing.T) {
	f := seedMonitoring(t)
	// A flat-rate call with no list price leaves the included value unknown.
	flat := usageEvent(t, f.support.ID, "llm.done", f.now.Add(-4*time.Minute), map[string]any{"model": "glm-5", "provider": "opencode-go", "tokens_in": 100, "tokens_out": 10})
	flat.ThreadID = "worker-1"
	if err := f.s.store.InsertTelemetry([]TelemetryEvent{flat}); err != nil {
		t.Fatal(err)
	}
	out, err := gatewayAgentsUsage(f.s.store, f.userID, map[string]any{"group_by": "thread"}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	result := asMap(t, out)
	totals := result["totals"].(map[string]any)
	if totals["calls"] != 3.0 || totals["errors"] != 3.0 {
		t.Fatalf("totals=%v", totals)
	}
	if rate := totals["cache_rate"].(float64); rate != 0.1923 {
		t.Fatalf("cache_rate=%v", rate)
	}
	cost := totals["cost"].(map[string]any)
	if cost["status"] != "partial" || cost["billed_usd"] != 0.5 || cost["unpriced_calls"] != 1.0 {
		t.Fatalf("cost=%v", cost)
	}
	if cost["included_status"] != "unknown" || cost["included_usd"] != nil || cost["included_calls"] != 1.0 {
		t.Fatalf("included cost=%v", cost)
	}
	coverage := result["coverage"].(map[string]any)
	if coverage["complete"] != true || coverage["period"] != "24h" {
		t.Fatalf("coverage=%v", coverage)
	}
	groups := result["groups"].([]any)
	byKey := map[string]map[string]any{}
	for _, g := range groups {
		row := g.(map[string]any)
		byKey[row["key"].(string)] = row
	}
	worker := byKey[fmt.Sprintf("%d/worker-1", f.support.ID)]
	if worker == nil || worker["thread_id"] != "worker-1" {
		t.Fatalf("groups=%v", groups)
	}
	if c := worker["cost"].(map[string]any); c["status"] != "unknown" || c["billed_usd"] != nil {
		t.Fatalf("unpriced group cost=%v", c)
	}

	one, err := gatewayAgentsUsage(f.s.store, f.userID, map[string]any{"agent_id": float64(f.support.ID)}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if calls := asMap(t, one)["totals"].(map[string]any)["calls"]; calls != 2.0 {
		t.Fatalf("agent_id scope calls=%v", calls)
	}
	for _, args := range []map[string]any{
		{"period": "1y"},
		{"period": "24h", "from": "2026-11-01"},
		{"group_by": "tool"},
	} {
		if _, err := gatewayAgentsUsage(f.s.store, f.userID, args, f.project.ID, f.now); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestGatewayAgentsToolUsage(t *testing.T) {
	f := seedMonitoring(t)
	out, err := gatewayAgentsToolUsage(f.s.store, f.userID, map[string]any{"period": "1h"}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	result := asMap(t, out)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	crm := tools[0].(map[string]any)
	if crm["tool"] != "crm_lookup" || crm["calls"] != 3.0 || crm["results"] != 3.0 || crm["failures"] != 2.0 {
		t.Fatalf("crm=%v", crm)
	}
	if crm["failure_rate"] != 0.6667 || crm["avg_duration_ms"] != 80.0 || crm["max_duration_ms"] != 120.0 {
		t.Fatalf("rates=%v", crm)
	}
	if evidence := crm["failure_evidence"].([]any); len(evidence) != 2 || evidence[0].(map[string]any)["event_id"] == "" {
		t.Fatalf("evidence=%v", evidence)
	}
	perAgent, err := gatewayAgentsToolUsage(f.s.store, f.userID, map[string]any{"group_by": "agent", "tool": "crm"}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	row := asMap(t, perAgent)["tools"].([]any)[0].(map[string]any)
	if row["agent_name"] != "writer" {
		t.Fatalf("per agent=%v", row)
	}
}

func TestGatewayAgentsErrors(t *testing.T) {
	f := seedMonitoring(t)
	out, err := gatewayAgentsErrors(f.s.store, f.userID, map[string]any{}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	result := asMap(t, out)
	totals := result["totals"].(map[string]any)
	if totals["errors"] != 3.0 || totals["provider_attempt_failures"] != 1.0 {
		t.Fatalf("totals=%v", totals)
	}
	groups := result["groups"].([]any)
	if len(groups) != 3 {
		t.Fatalf("groups=%v", groups)
	}
	tool := groups[0].(map[string]any)
	if tool["category"] != "tool" || tool["subject"] != "crm_lookup" || tool["count"] != 2.0 || tool["counts_as_error"] != true {
		t.Fatalf("tool group=%v", tool)
	}
	signature := tool["signature"].(string)
	if strings.Contains(signature, "127") || strings.Contains(signature, "53736") || !strings.Contains(signature, "connection refused") {
		t.Fatalf("signature=%q", signature)
	}
	if _, ok := tool["samples"]; ok {
		t.Fatal("raw samples returned without include_details")
	}
	for _, g := range groups[1:] {
		row := g.(map[string]any)
		if row["category"] == "provider" && row["counts_as_error"] != false {
			t.Fatalf("provider group=%v", row)
		}
	}

	detailed, err := gatewayAgentsErrors(f.s.store, f.userID, map[string]any{"category": "tool", "include_details": true}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	only := asMap(t, detailed)["groups"].([]any)
	if len(only) != 1 {
		t.Fatalf("tool category groups=%v", only)
	}
	if samples := only[0].(map[string]any)["samples"].([]any); len(samples) != 2 || !strings.Contains(samples[0].(string), "127.0.0.1") {
		t.Fatalf("samples=%v", samples)
	}
	if _, err := gatewayAgentsErrors(f.s.store, f.userID, map[string]any{"category": "network"}, f.project.ID, f.now); err == nil {
		t.Fatal("unknown category accepted")
	}
}

func TestGatewayAgentsHealth(t *testing.T) {
	f := seedMonitoring(t)
	if _, err := f.s.store.CreateAgent(f.userID, "idle", "", "cautious", "{}", f.project.ID); err != nil {
		t.Fatal(err)
	}
	ghost, err := f.s.store.CreateAgent(f.userID, "ghost", "", "cautious", "{}", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.db.Exec(`UPDATE agents SET status='running' WHERE id IN (?,?,?)`, f.writer.ID, f.support.ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"queued", "event.active", "event.settled"} {
		if _, err := f.s.store.db.Exec(`INSERT INTO agent_event_executions (agent_id,project_id,source_app,source_install_id,source_event_id,core_event_id,thread_id,payload_hash,state)
 VALUES (?,?,?,?,?,?,?,?,?)`, f.support.ID, f.project.ID, "tasks", 7, fmt.Sprint("src-", i), fmt.Sprint("core-", i), "main", "h", state); err != nil {
			t.Fatal(err)
		}
	}
	core := map[string][2]string{
		fmt.Sprint(f.writer.ID): {`{"paused":false,"llm_active":false,"iteration":5,"inference":{"consecutive_failures":2}}`,
			`[{"id":"main","inference":{"consecutive_failures":2}},{"id":"worker-1","directive":"private directive","inference":{"consecutive_failures":0}}]`},
		fmt.Sprint(f.support.ID): {`{"paused":false,"llm_active":true,"inference":{"consecutive_failures":0}}`, `[{"id":"main","inference":{}}]`},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
		replies, ok := core[parts[0]]
		if !ok || len(parts) != 2 {
			http.Error(w, "instance not running", http.StatusServiceUnavailable)
			return
		}
		if parts[1] == "status" {
			fmt.Fprint(w, replies[0])
		} else {
			fmt.Fprint(w, replies[1])
		}
	}))
	defer ts.Close()
	api := gatewayAPIClient{baseURL: ts.URL + "/api", userID: f.userID, instanceSecret: "test-secret"}

	out, err := gatewayAgentsHealth(f.s.store, api, map[string]any{}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	result := asMap(t, out)
	rows := map[string]map[string]any{}
	for _, raw := range result["agents"].([]any) {
		row := raw.(map[string]any)
		rows[row["name"].(string)] = row
	}
	for name, want := range map[string]string{"writer": "failing", "support": "working", "idle": "stopped", "ghost": "unknown"} {
		if got := rows[name]["state"]; got != want {
			t.Fatalf("%s state=%v reasons=%v", name, got, rows[name]["reasons"])
		}
	}
	writer := rows["writer"]
	if window := writer["telemetry"].(map[string]any)["window"].(map[string]any); window["errors"] != 3.0 || window["provider_attempt_failures"] != 1.0 || window["tool_failures"] != 2.0 {
		t.Fatalf("writer window=%v", window)
	}
	if writer["telemetry"].(map[string]any)["last_model_failure_at"] == nil {
		t.Fatal("missing last_model_failure_at")
	}
	if _, listed := writer["live"].(map[string]any)["threads"]; listed {
		t.Fatal("threads listed without agent_id or include_threads")
	}
	work := rows["support"]["active_work"].(map[string]any)
	if work["tracked"] != 2.0 || work["queued"] != 1.0 || work["active"] != 1.0 {
		t.Fatalf("active work=%v", work)
	}
	if rows["ghost"]["live"].(map[string]any)["available"] != false {
		t.Fatalf("ghost live=%v", rows["ghost"]["live"])
	}

	one, err := gatewayAgentsHealth(f.s.store, api, map[string]any{"agent_id": float64(f.writer.ID)}, f.project.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(one)
	if strings.Contains(string(raw), "private directive") {
		t.Fatal("thread directive leaked")
	}
	threads := asMap(t, one)["agents"].([]any)[0].(map[string]any)["live"].(map[string]any)["threads"].([]any)
	if len(threads) != 2 {
		t.Fatalf("threads=%v", threads)
	}
}

func TestGatewayMonitoringRequiresProjectAccess(t *testing.T) {
	f := seedMonitoring(t)
	outsider, err := f.s.store.CreateUser("outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayAgentsErrors(f.s.store, outsider.ID, map[string]any{}, f.project.ID, f.now); err == nil {
		t.Fatal("outsider read project errors")
	}
	if err := f.s.store.AddProjectMember(f.project.ID, outsider.ID, ProjectViewer, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayAgentsErrors(f.s.store, outsider.ID, map[string]any{}, f.project.ID, f.now); err != nil {
		t.Fatalf("viewer refused: %v", err)
	}
	other, err := f.s.store.CreateProject(f.userID, "Other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayAgentsUsage(f.s.store, f.userID, map[string]any{"agent_id": float64(f.writer.ID)}, other.ID, f.now); err == nil {
		t.Fatal("agent from another project accepted")
	}
}

func TestProjectConversationScopesMonitoringAgent(t *testing.T) {
	f := seedMonitoring(t)
	other, err := f.s.store.CreateProject(f.userID, "Other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := f.s.store.CreateAgent(f.userID, "stranger", "", "cautious", "{}", other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for tool := range gatewayMonitoringTools {
		call := func(agentID int64) ([]byte, error) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"agent_id":%d,"project_id":"%s"}}}`, tool, agentID, other.ID)
			return f.s.scopeProjectGatewayRequest([]byte(body), f.project.ID)
		}
		scoped, err := call(f.writer.ID)
		if err != nil {
			t.Fatal(tool, err)
		}
		if !strings.Contains(string(scoped), `"project_id":"`+f.project.ID+`"`) {
			t.Fatalf("%s: project not forced: %s", tool, scoped)
		}
		if _, err := call(stranger.ID); err == nil {
			t.Fatalf("%s: agent outside the trusted project accepted", tool)
		}
	}
}
