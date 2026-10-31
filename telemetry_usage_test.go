package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func usageEvent(t *testing.T, agentID int64, kind string, at time.Time, data map[string]any) TelemetryEvent {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return TelemetryEvent{AgentID: agentID, Type: kind, Time: at, Data: raw}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// seedUsage creates two agents in one project: "writer" on a metered model
// and "coder" on the openai-codex subscription, plus one older event in the
// previous window and one unpriced call.
func seedUsage(t *testing.T) (*Server, int64, *Project, time.Time) {
	t.Helper()
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	project, err := s.store.CreateProject(userID, "Usage", "", "")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := s.store.CreateAgent(userID, "writer", "", "cautious", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	coder, err := s.store.CreateAgent(userID, "coder", "", "cautious", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []TelemetryEvent{
		usageEvent(t, writer.ID, "llm.done", now.Add(-2*time.Hour), map[string]any{"model": "claude-sonnet", "provider": "anthropic", "tokens_in": 1000, "tokens_cached": 400, "tokens_out": 200, "cost_usd": 0.5}),
		usageEvent(t, writer.ID, "llm.done", now.Add(-90*time.Minute), map[string]any{"model": "claude-sonnet", "provider": "anthropic", "tokens_in": 500, "tokens_out": 100, "cost_usd": 0.25}),
		usageEvent(t, writer.ID, "llm.done", now.Add(-time.Hour), map[string]any{"model": "mystery-model", "provider": "anthropic", "tokens_in": 10, "tokens_out": 5}),
		usageEvent(t, writer.ID, "llm.error", now.Add(-time.Hour), map[string]any{"model": "claude-sonnet", "provider": "anthropic"}),
		usageEvent(t, coder.ID, "llm.done", now.Add(-30*time.Minute), map[string]any{"model": "gpt-5.6", "provider": "openai-codex", "tokens_in": 2000, "tokens_out": 300, "cost_usd": 1.0}),
		usageEvent(t, writer.ID, "llm.done", now.Add(-30*time.Hour), map[string]any{"model": "claude-sonnet", "provider": "anthropic", "tokens_in": 100, "tokens_out": 10, "cost_usd": 2.0}),
	}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	return s, userID, project, now
}

func TestTelemetryMetricsCaptureModelAndProvider(t *testing.T) {
	s, _, _, _ := seedUsage(t)
	var model, provider string
	if err := s.store.db.QueryRow(`SELECT model,provider FROM telemetry_metrics WHERE type='llm.done' AND provider='openai-codex'`).Scan(&model, &provider); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.6" {
		t.Fatalf("model=%q", model)
	}
}

func TestUsageReportSplitsBilledAndSubscriptionSpend(t *testing.T) {
	s, userID, project, now := seedUsage(t)
	report, err := s.store.TelemetryUsageReport(context.Background(), userID, project.ID, "24h", "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	tot := report.Totals
	if !nearly(tot.Cost, 0.75) || !nearly(tot.IncludedCost, 1.0) {
		t.Fatalf("billed=%v included=%v, want 0.75 and 1.0", tot.Cost, tot.IncludedCost)
	}
	if tot.Calls != 4 || tot.Errors != 1 || tot.UnpricedCalls != 1 || tot.ActiveAgents != 2 {
		t.Fatalf("totals=%+v", tot)
	}
	if tot.TokensIn != 3510 || tot.TokensCached != 400 || tot.TokensOut != 605 {
		t.Fatalf("tokens=%+v", tot)
	}
	if report.Previous.Calls != 1 || !nearly(report.Previous.Cost, 2.0) {
		t.Fatalf("previous=%+v", report.Previous)
	}

	// The zero-filled timeline adds up to the totals.
	if len(report.Timeline) < 24 {
		t.Fatalf("timeline has %d buckets, want a full day", len(report.Timeline))
	}
	var cost, included float64
	calls, errors := 0, 0
	for _, b := range report.Timeline {
		cost += b.Cost
		included += b.IncludedCost
		calls += b.Calls
		errors += b.Errors
	}
	if !nearly(cost, tot.Cost) || !nearly(included, tot.IncludedCost) || calls != tot.Calls || errors != tot.Errors {
		t.Fatalf("timeline sums cost=%v included=%v calls=%d errors=%d differ from totals %+v", cost, included, calls, errors, tot)
	}

	if len(report.Groups) != 2 || report.Groups[0].Label != "coder" || report.Groups[0].Billing != "subscription" || report.Groups[1].Billing != "metered" {
		t.Fatalf("groups=%+v", report.Groups)
	}
	if report.DailyRunRate <= 0 {
		t.Fatalf("run rate=%v, want billed spend over the last 7 days", report.DailyRunRate)
	}
}

func TestUsageReportGroupsByModel(t *testing.T) {
	s, userID, project, now := seedUsage(t)
	report, err := s.store.TelemetryUsageReport(context.Background(), userID, project.ID, "24h", "model", now)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]UsageGroup{}
	for _, g := range report.Groups {
		byKey[g.Key] = g
	}
	if g := byKey["claude-sonnet"]; g.Calls != 2 || !nearly(g.Cost, 0.75) || g.Errors != 1 {
		t.Fatalf("claude-sonnet=%+v", g)
	}
	if g := byKey["mystery-model"]; g.UnpricedCalls != 1 || g.Cost != 0 {
		t.Fatalf("mystery-model=%+v", g)
	}
	if g := byKey["gpt-5.6"]; !nearly(g.IncludedCost, 1.0) || g.Cost != 0 {
		t.Fatalf("gpt-5.6=%+v", g)
	}
}

func TestUsageEndpointChecksProjectAccess(t *testing.T) {
	s, userID, project, _ := seedUsage(t)
	other, err := s.store.CreateUser("outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user int64
		want int
	}{{userID, http.StatusOK}, {other.ID, http.StatusForbidden}} {
		req := httptest.NewRequest(http.MethodGet, "/telemetry/usage?project_id="+project.ID+"&period=7d&group_by=provider", nil)
		req.Header.Set("X-User-ID", fmt.Sprint(tc.user))
		rec := httptest.NewRecorder()
		s.handleTelemetryUsage(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("user %d: status=%d body=%s", tc.user, rec.Code, rec.Body.String())
		}
		if tc.want == http.StatusOK {
			var report UsageReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Period != "7d" || report.GroupBy != "provider" || report.BucketMinutes != 360 {
				t.Fatalf("report header=%+v", report)
			}
		}
	}
}

// An existing database from before migration 7 gets the columns, a backfill
// of recent rows and triggers that keep populating them.
func TestTelemetryUsageMigrationUpgradesExistingFacts(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(userID, "legacy", "", "cautious", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	old := func(prefix string) string { return telemetryMetricsProjection(prefix, false) }
	for _, q := range []string{
		`DROP TRIGGER telemetry_metrics_insert`,
		`DROP TRIGGER telemetry_metrics_update`,
		`ALTER TABLE telemetry_metrics DROP COLUMN model`,
		`ALTER TABLE telemetry_metrics DROP COLUMN provider`,
		`CREATE TRIGGER telemetry_metrics_insert AFTER INSERT ON telemetry BEGIN INSERT OR REPLACE INTO telemetry_metrics ` + old("NEW.") + `; END`,
		`CREATE TRIGGER telemetry_metrics_update AFTER UPDATE ON telemetry BEGIN DELETE FROM telemetry_metrics WHERE id=OLD.id; INSERT OR REPLACE INTO telemetry_metrics ` + old("NEW.") + `; END`,
		`DELETE FROM server_schema_migrations WHERE version=7`,
	} {
		if _, err := s.store.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Now().UTC()
	if err := s.store.InsertTelemetry([]TelemetryEvent{usageEvent(t, agent.ID, "llm.done", now.Add(-time.Hour), map[string]any{"model": "before", "provider": "anthropic", "tokens_in": 1, "cost_usd": 0.1})}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.migrateTelemetryUsageColumns(); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertTelemetry([]TelemetryEvent{usageEvent(t, agent.ID, "llm.done", now, map[string]any{"model": "after", "provider": "openai", "tokens_in": 1, "cost_usd": 0.1})}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.db.Query(`SELECT model,provider FROM telemetry_metrics WHERE agent_id=? ORDER BY time`, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var model, provider string
		if err := rows.Scan(&model, &provider); err != nil {
			t.Fatal(err)
		}
		got = append(got, model+"/"+provider)
	}
	if len(got) != 2 || got[0] != "before/anthropic" || got[1] != "after/openai" {
		t.Fatalf("model/provider after upgrade = %v", got)
	}
	var rollupCalls int
	if err := s.store.db.QueryRow(`SELECT COALESCE(SUM(samples),0) FROM telemetry_rollups WHERE agent_id=? AND type='llm.done'`, agent.ID).Scan(&rollupCalls); err != nil || rollupCalls != 2 {
		t.Fatalf("rollups after upgrade = %d (%v), want 2", rollupCalls, err)
	}
}
