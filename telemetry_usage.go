package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Usage report for the dashboard's Usage page. One query over the compact
// telemetry facts produces every number on the page, so totals, the timeline
// and breakdowns always agree: a call is an llm.done or realtime.usage event,
// an error is an llm.error, tool.error or realtime.error event.
//
// Spend is split by how it is paid for. Providers whose type carries the
// "subscription" capability (e.g. openai-codex) are priced at list rates but
// not billed per token, so their cost is reported as included_cost.

const usageFactTypes = `'llm.done','realtime.usage','llm.error','tool.error','realtime.error'`

// usageBackfillWindow bounds the one-off model/provider backfill; the Usage
// page reads at most two 30-day windows.
const usageBackfillWindow = 90 * 24 * time.Hour

// migrateTelemetryUsageColumns adds model and provider to telemetry_metrics
// and recreates the ingest triggers that populate it.
func (s *Store) migrateTelemetryUsageColumns() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRow("SELECT count(*) FROM server_schema_migrations WHERE version=7").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return tx.Commit()
	}
	projection := func(prefix string) string { return telemetryMetricsProjection(prefix, true) }
	since := time.Now().Add(-usageBackfillWindow).UTC().Format(metricTimeLayout)
	statements := []string{
		`ALTER TABLE telemetry_metrics ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE telemetry_metrics ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`DROP TRIGGER IF EXISTS telemetry_metrics_insert`,
		`DROP TRIGGER IF EXISTS telemetry_metrics_update`,
		`CREATE TRIGGER telemetry_metrics_insert AFTER INSERT ON telemetry BEGIN INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`CREATE TRIGGER telemetry_metrics_update AFTER UPDATE ON telemetry BEGIN DELETE FROM telemetry_metrics WHERE id=OLD.id; INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`UPDATE telemetry_metrics SET
  model=CASE WHEN json_type(t.data,'$.model')='text' THEN json_extract(t.data,'$.model') ELSE '' END,
  provider=CASE WHEN json_type(t.data,'$.provider')='text' THEN json_extract(t.data,'$.provider') ELSE '' END
 FROM (SELECT id,data FROM telemetry WHERE type IN (` + usageFactTypes + `) AND json_valid(data)) AS t
 WHERE telemetry_metrics.id=t.id AND telemetry_metrics.type IN (` + usageFactTypes + `) AND telemetry_metrics.time>='` + since + `'`,
		`INSERT INTO server_schema_migrations(version) VALUES(7)`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("telemetry usage migration: %w", err)
		}
	}
	return tx.Commit()
}

type UsageTotals struct {
	Cost          float64 `json:"cost"`
	IncludedCost  float64 `json:"included_cost"`
	Calls         int     `json:"calls"`
	TokensIn      int     `json:"tokens_in"`
	TokensOut     int     `json:"tokens_out"`
	TokensCached  int     `json:"tokens_cached"`
	Errors        int     `json:"errors"`
	UnpricedCalls int     `json:"unpriced_calls"`
	ActiveAgents  int     `json:"active_agents"`
}

type UsageSeriesPoint struct {
	Cost         float64 `json:"cost"`
	IncludedCost float64 `json:"included_cost"`
	Calls        int     `json:"calls"`
	Tokens       int     `json:"tokens"`
}

type UsageBucket struct {
	Time         string                      `json:"time"`
	Cost         float64                     `json:"cost"`
	IncludedCost float64                     `json:"included_cost"`
	Calls        int                         `json:"calls"`
	TokensIn     int                         `json:"tokens_in"`
	TokensOut    int                         `json:"tokens_out"`
	Errors       int                         `json:"errors"`
	Series       map[string]UsageSeriesPoint `json:"series"`
}

type UsageGroup struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Billing       string  `json:"billing"` // metered, subscription or mixed
	AgentID       int64   `json:"agent_id,omitempty"`
	ProjectID     string  `json:"project_id,omitempty"`
	Status        string  `json:"status,omitempty"`
	Cost          float64 `json:"cost"`
	IncludedCost  float64 `json:"included_cost"`
	Calls         int     `json:"calls"`
	TokensIn      int     `json:"tokens_in"`
	TokensOut     int     `json:"tokens_out"`
	TokensCached  int     `json:"tokens_cached"`
	Errors        int     `json:"errors"`
	UnpricedCalls int     `json:"unpriced_calls"`
	metered       bool
	subscription  bool
}

type UsageReport struct {
	Period        string        `json:"period"`
	GroupBy       string        `json:"group_by"`
	Since         string        `json:"since"`
	Until         string        `json:"until"`
	BucketMinutes int           `json:"bucket_minutes"`
	Agents        int           `json:"agents"`
	Totals        UsageTotals   `json:"totals"`
	Previous      UsageTotals   `json:"previous"`
	DailyRunRate  float64       `json:"daily_run_rate"` // billed cost per day over the last 7 days
	RunRateDays   float64       `json:"run_rate_days"`
	Timeline      []UsageBucket `json:"timeline"`
	Groups        []UsageGroup  `json:"groups"`
}

type usagePeriod struct {
	length        time.Duration
	bucketMinutes int
}

var usagePeriods = map[string]usagePeriod{
	"24h": {24 * time.Hour, 60},
	"7d":  {7 * 24 * time.Hour, 360},
	"30d": {30 * 24 * time.Hour, 1440},
}

var usageGroupings = map[string]bool{"agent": true, "model": true, "provider": true, "project": true}

// flatRateProviders are covered by a flat monthly fee but not marked with the
// "subscription" capability in provider_types (see the OpenCode Go note in
// modelPricingTable).
var flatRateProviders = []string{"opencode-go"}

// subscriptionProviderKeys lists provider keys whose usage is covered by a
// subscription rather than billed per token.
func (s *Store) subscriptionProviderKeys() map[string]bool {
	out := map[string]bool{}
	for _, key := range flatRateProviders {
		out[key] = true
	}
	types, err := s.ListProviderTypes()
	if err != nil {
		return out
	}
	for _, pt := range types {
		for _, capability := range pt.Capabilities {
			if capability == "subscription" && pt.AuthProvider != "" {
				out[pt.AuthProvider] = true
			}
		}
	}
	return out
}

func (s *Store) TelemetryUsageReport(ctx context.Context, userID int64, project, period, groupBy string, now time.Time) (*UsageReport, error) {
	p, ok := usagePeriods[period]
	if !ok {
		period, p = "24h", usagePeriods["24h"]
	}
	if !usageGroupings[groupBy] {
		groupBy = "agent"
	}
	now = now.UTC()
	since := now.Add(-p.length)
	width := int64(p.bucketMinutes) * 60
	report := &UsageReport{
		Period: period, GroupBy: groupBy, BucketMinutes: p.bucketMinutes,
		Since: since.Format(time.RFC3339), Until: now.Format(time.RFC3339),
		Timeline: []UsageBucket{}, Groups: []UsageGroup{},
	}
	for start := since.Unix() / width * width; start <= now.Unix(); start += width {
		report.Timeline = append(report.Timeline, UsageBucket{Time: time.Unix(start, 0).UTC().Format(time.RFC3339), Series: map[string]UsageSeriesPoint{}})
	}

	agents, err := s.listAgentsForTelemetry(userID, project)
	if err != nil {
		return nil, err
	}
	report.Agents = len(agents)
	if len(agents) == 0 {
		return report, nil
	}
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	projectNames := map[string]string{}
	if groupBy == "project" {
		if projects, err := s.ListProjectsForUser(userID); err == nil {
			for _, pr := range projects {
				projectNames[pr.ID] = pr.Name
			}
		}
	}
	subscription := s.subscriptionProviderKeys()

	ids, args := metricIDs(agents)
	query := `SELECT agent_id,(unixepoch(substr(time,1,19)||'Z')/?)*? AS bucket,time>=? AS cur,type,model,provider,
 COUNT(*),SUM(tokens_in),SUM(tokens_out),SUM(tokens_cached),SUM(cost),SUM(cost_present)
 FROM telemetry_metrics WHERE agent_id IN (` + ids + `) AND type IN (` + usageFactTypes + `) AND time>=? AND time<=?
 GROUP BY agent_id,bucket,cur,type,model,provider`
	params := append([]any{width, width, since.Format(metricTimeLayout)}, args...)
	params = append(params, since.Add(-p.length).Format(metricTimeLayout), now.Format(metricTimeLayout))
	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	firstBucket := since.Unix() / width * width
	groups := map[string]*UsageGroup{}
	active := map[int64]bool{}
	for rows.Next() {
		var agentID, bucket int64
		var current bool
		var kind, model, provider string
		var count, in, out, cached, present int
		var cost float64
		if err := rows.Scan(&agentID, &bucket, &current, &kind, &model, &provider, &count, &in, &out, &cached, &cost, &present); err != nil {
			return nil, err
		}
		isCall := kind == "llm.done" || kind == "realtime.usage"
		included := subscription[provider]
		totals := &report.Previous
		if current {
			totals = &report.Totals
		}
		addUsage(totals, isCall, included, count, in, out, cached, cost, present)
		if !current {
			continue
		}
		if isCall && count > 0 {
			active[agentID] = true
		}

		agent := byID[agentID]
		key, label := usageGroupKey(groupBy, agent, model, provider, projectNames)
		g := groups[key]
		if g == nil {
			g = &UsageGroup{Key: key, Label: label}
			if groupBy == "agent" {
				g.AgentID, g.ProjectID, g.Status = agent.ID, agent.ProjectID, agent.Status
			} else if groupBy == "project" {
				g.ProjectID = agent.ProjectID
			}
			groups[key] = g
		}
		if isCall {
			g.Calls += count
			g.TokensIn += in
			g.TokensOut += out
			g.TokensCached += cached
			if included {
				g.IncludedCost += cost
				g.subscription = true
			} else {
				g.Cost += cost
				g.UnpricedCalls += count - present
				g.metered = true
			}
		} else {
			g.Errors += count
		}

		index := int((bucket - firstBucket) / width)
		if index < 0 || index >= len(report.Timeline) {
			continue
		}
		b := &report.Timeline[index]
		if !isCall {
			b.Errors += count
			continue
		}
		point := b.Series[key]
		point.Calls += count
		point.Tokens += in + out
		b.Calls += count
		b.TokensIn += in
		b.TokensOut += out
		if included {
			point.IncludedCost += cost
			b.IncludedCost += cost
		} else {
			point.Cost += cost
			b.Cost += cost
		}
		b.Series[key] = point
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	report.Totals.ActiveAgents = len(active)

	for _, g := range groups {
		switch {
		case g.metered && g.subscription:
			g.Billing = "mixed"
		case g.subscription:
			g.Billing = "subscription"
		default:
			g.Billing = "metered"
		}
		report.Groups = append(report.Groups, *g)
	}
	sort.Slice(report.Groups, func(i, j int) bool {
		a, b := report.Groups[i], report.Groups[j]
		if av, bv := a.Cost+a.IncludedCost, b.Cost+b.IncludedCost; av != bv {
			return av > bv
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return a.Label < b.Label
	})

	report.DailyRunRate, report.RunRateDays, err = s.usageRunRate(ctx, ids, args, subscription, now)
	return report, err
}

func addUsage(t *UsageTotals, isCall, included bool, count, in, out, cached int, cost float64, present int) {
	if !isCall {
		t.Errors += count
		return
	}
	t.Calls += count
	t.TokensIn += in
	t.TokensOut += out
	t.TokensCached += cached
	if included {
		t.IncludedCost += cost
	} else {
		t.Cost += cost
		t.UnpricedCalls += count - present
	}
}

func usageGroupKey(groupBy string, agent Agent, model, provider string, projectNames map[string]string) (string, string) {
	switch groupBy {
	case "model":
		if model == "" {
			return "unknown", "Unknown model"
		}
		return model, model
	case "provider":
		if provider == "" {
			return "unknown", "Unknown provider"
		}
		return provider, provider
	case "project":
		if agent.ProjectID == "" {
			return "none", "No project"
		}
		if name := projectNames[agent.ProjectID]; name != "" {
			return agent.ProjectID, name
		}
		return agent.ProjectID, agent.ProjectID
	}
	key := strconv.FormatInt(agent.ID, 10)
	if agent.Name == "" {
		return key, "Agent " + key
	}
	return key, agent.Name
}

// usageRunRate is billed spend per day over the last 7 days. Agents with less
// history are averaged over the days they actually have, at least one.
func (s *Store) usageRunRate(ctx context.Context, ids string, args []any, subscription map[string]bool, now time.Time) (float64, float64, error) {
	since := now.Add(-7 * 24 * time.Hour)
	params := append(append([]any{}, args...), since.Format(metricTimeLayout), now.Format(metricTimeLayout))
	rows, err := s.db.QueryContext(ctx, `SELECT provider,SUM(cost),MIN(time) FROM telemetry_metrics
 WHERE agent_id IN (`+ids+`) AND type IN ('llm.done','realtime.usage') AND time>=? AND time<=? GROUP BY provider`, params...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var billed float64
	earliest := now
	for rows.Next() {
		var provider, first string
		var cost float64
		if err := rows.Scan(&provider, &cost, &first); err != nil {
			return 0, 0, err
		}
		if subscription[provider] || cost <= 0 {
			continue
		}
		billed += cost
		if t, err := time.Parse(metricTimeLayout, first); err == nil && t.Before(earliest) {
			earliest = t
		}
	}
	if err := rows.Err(); err != nil || billed == 0 {
		return 0, 0, err
	}
	days := now.Sub(earliest).Hours() / 24
	if days < 1 {
		days = 1
	}
	return billed / days, days, nil
}

// GET /telemetry/usage?project_id=&period=24h|7d|30d&group_by=agent|model|provider|project
// Without project_id the report covers every agent the caller can see.
func (s *Server) handleTelemetryUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()
	projectID := strings.TrimSpace(q.Get("project_id"))
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	report, err := s.store.TelemetryUsageReport(r.Context(), userID, projectID, q.Get("period"), q.Get("group_by"), time.Now())
	if err != nil {
		http.Error(w, "usage report failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
}
