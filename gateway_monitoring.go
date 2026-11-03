package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Read-only monitoring tools on the Apteva Server gateway. They read the same
// telemetry projections as the dashboard charts, and every reply states the
// window it covers, so history removed by retention is never read as a quiet
// period.

var gatewayMonitoringTools = map[string]bool{
	"agents_health": true, "agents_usage": true, "agents_tool_usage": true, "agents_errors": true,
	"agents_threads": true,
}

var monitorPeriods = map[string]time.Duration{
	"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
}

const (
	// monitorErrorScanCap bounds the error events agents_errors groups.
	monitorErrorScanCap = 5000
	// monitorHealthPageSize bounds the agents whose Core is asked for live
	// status in one agents_health call.
	monitorHealthPageSize = 25
	monitorThreadCap      = 50
	monitorLiveTimeout    = 5 * time.Second
	// monitorThreadAgentCap bounds the agents whose thread lists one
	// agents_threads call fetches.
	monitorThreadAgentCap = 50
)

func handleGatewayMonitoringTool(name string, args map[string]any, projectID string, serverAPI gatewayAPIClient, store *Store) (any, error) {
	if store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	now := time.Now().UTC()
	switch name {
	case "agents_usage":
		return gatewayAgentsUsage(store, serverAPI.userID, args, projectID, now)
	case "agents_tool_usage":
		return gatewayAgentsToolUsage(store, serverAPI.userID, args, projectID, now)
	case "agents_errors":
		return gatewayAgentsErrors(store, serverAPI.userID, args, projectID, now)
	case "agents_health":
		return gatewayAgentsHealth(store, serverAPI, args, projectID, now)
	case "agents_threads":
		return gatewayAgentsThreads(store, serverAPI, args, projectID, now)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

type monitorScope struct {
	ProjectID string
	Agents    []Agent
	Range     UsageRange
	Since     time.Time
	Until     time.Time // never after Now
	Now       time.Time
}

// gatewayMonitorScope resolves the project, agents and window a monitoring
// call may read. Project members see every agent in the project, as on the
// dashboard; without a project the user's own agents are read.
func gatewayMonitorScope(store *Store, userID int64, args map[string]any, defaultProjectID string, now time.Time, defaultPeriod string) (*monitorScope, error) {
	projectID := gatewayProjectIDArg(args, defaultProjectID)
	if projectID != "" && store.GetPlatformRole(userID) != PlatformAdmin {
		role, err := store.GetProjectRole(projectID, userID)
		if err != nil || role.Rank() < ProjectViewer.Rank() {
			return nil, fmt.Errorf("project %q is not visible to this user", projectID)
		}
	}
	agents, err := store.listAgentsForTelemetry(userID, projectID)
	if err != nil {
		return nil, err
	}
	if raw, ok := args["agent_id"]; ok && raw != nil {
		id, err := parseIntArg(raw)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("agent_id must be a positive integer")
		}
		var match []Agent
		for _, a := range agents {
			if a.ID == id {
				match = append(match, a)
			}
		}
		if len(match) == 0 {
			return nil, fmt.Errorf("agent %d is not visible in this scope", id)
		}
		agents = match
	}
	rng, err := gatewayMonitorRange(args, now, defaultPeriod)
	if err != nil {
		return nil, err
	}
	until := rng.End.UTC()
	if until.After(now) {
		until = now
	}
	return &monitorScope{ProjectID: projectID, Agents: agents, Range: rng, Since: rng.Start.UTC(), Until: until, Now: now}, nil
}

func gatewayMonitorRange(args map[string]any, now time.Time, defaultPeriod string) (UsageRange, error) {
	arg := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	period, from, to, tz := arg("period"), arg("from"), arg("to"), arg("tz")
	if from != "" || to != "" {
		if period != "" {
			return UsageRange{}, fmt.Errorf("pass period or from/to, not both")
		}
		if from == "" {
			return UsageRange{}, fmt.Errorf("to requires from")
		}
		return ResolveUsageRange(url.Values{"from": {from}, "to": {to}, "tz": {tz}}, now)
	}
	if period == "" {
		period = defaultPeriod
	}
	length, ok := monitorPeriods[period]
	if !ok {
		return UsageRange{}, fmt.Errorf("period must be 1h, 24h, 7d, 30d or 90d")
	}
	loc := time.UTC
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return UsageRange{}, fmt.Errorf("unknown tz %q", tz)
		}
	}
	return UsageRange{Start: now.Add(-length), End: now, Location: loc, Bucket: usageResolution(length), Period: period}, nil
}

// monitorCoverage says whether telemetry for [from, to] is still retained.
func monitorCoverage(from, to, now time.Time) map[string]any {
	out := map[string]any{"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339)}
	retention := telemetryRetentionFromEnv()
	if retention <= 0 {
		out["retention"] = "disabled"
		out["complete"] = true
		return out
	}
	horizon := now.Add(-retention).UTC()
	out["retention_days"] = math.Round(retention.Hours()/24*10) / 10
	out["retained_from"] = horizon.Format(time.RFC3339)
	out["complete"] = !from.Before(horizon)
	if from.Before(horizon) {
		out["note"] = "Telemetry before retained_from has been deleted; figures for that part of the window are missing, not zero."
	}
	return out
}

func (m *monitorScope) coverage() map[string]any {
	out := monitorCoverage(m.Since, m.Until, m.Now)
	out["period"] = m.Range.Period
	out["timezone"] = m.Range.Location.String()
	out["agents"] = len(m.Agents)
	if m.ProjectID != "" {
		out["project_id"] = m.ProjectID
	}
	return out
}

func (m *monitorScope) agentNames() map[int64]string {
	names := map[int64]string{}
	for _, a := range m.Agents {
		names[a.ID] = a.Name
	}
	return names
}

func roundTo(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}

func ratio(part, whole int) any {
	if whole <= 0 {
		return nil
	}
	return roundTo(float64(part)/float64(whole), 4)
}

// --- agents_usage ---

var usageDefinitions = map[string]string{
	"calls":        "Completed model calls: llm.done and realtime.usage events.",
	"cache_rate":   "tokens_cached / tokens_in. Core counts cached prompt tokens inside tokens_in, so this is the share of prompt tokens read from cache. null when there were no prompt tokens.",
	"billed_usd":   "Estimated, not a provider invoice: apteva-server prices llm.done at ingest from its list-price table for the reported model (uncached input, cached input at the cached rate, output), and Core prices realtime usage from its realtime table. complete: every metered call was priced. partial: some were not, so billed_usd is a lower bound. unknown: none were, so billed_usd is null. none: no metered calls.",
	"included_usd": "List-price value of calls on subscription or flat-rate providers, which are not billed per token. included_status follows the same complete, partial, unknown and none rules.",
	"errors":       "llm.error, realtime.error, tool.error and tool results that report failure. Provider attempt failures that a retry or fallback may recover are not errors; agents_errors lists them.",
	"previous":     "The window of equal length immediately before this one.",
}

// pricedAmount reports a summed price and how much of it is known.
func pricedAmount(calls, unpriced int, amount float64) (any, string) {
	switch {
	case calls == 0:
		return 0.0, "none"
	case unpriced == 0:
		return roundTo(amount, 6), "complete"
	case unpriced >= calls:
		return nil, "unknown"
	}
	return roundTo(amount, 6), "partial"
}

func usageCostView(calls, metered, unpriced, unpricedIncluded int, billed, included float64) map[string]any {
	out := map[string]any{"metered_calls": metered, "unpriced_calls": unpriced, "included_calls": calls - metered, "unpriced_included_calls": unpricedIncluded}
	out["billed_usd"], out["status"] = pricedAmount(metered, unpriced, billed)
	out["included_usd"], out["included_status"] = pricedAmount(calls-metered, unpricedIncluded, included)
	return out
}

func usageTotalsView(t UsageTotals) map[string]any {
	return map[string]any{
		"calls": t.Calls, "tokens_in": t.TokensIn, "tokens_out": t.TokensOut, "tokens_cached": t.TokensCached,
		"cache_rate": ratio(t.TokensCached, t.TokensIn), "errors": t.Errors, "active_agents": t.ActiveAgents,
		"cost": usageCostView(t.Calls, t.MeteredCalls, t.UnpricedCalls, t.UnpricedIncluded, t.Cost, t.IncludedCost),
	}
}

func gatewayAgentsUsage(store *Store, userID int64, args map[string]any, projectID string, now time.Time) (any, error) {
	scope, err := gatewayMonitorScope(store, userID, args, projectID, now, "24h")
	if err != nil {
		return nil, err
	}
	groupBy, _ := args["group_by"].(string)
	groupBy = strings.TrimSpace(groupBy)
	if groupBy == "" {
		groupBy = "agent"
	}
	if !usageGroupings[groupBy] {
		return nil, fmt.Errorf("group_by must be agent, thread, model, provider or project")
	}
	includeTimeline, _, err := optionalBoolArg(args["include_timeline"])
	if err != nil {
		return nil, fmt.Errorf("include_timeline must be true or false")
	}
	report, err := store.TelemetryUsageReportForAgents(context.Background(), userID, scope.Agents, scope.Range, groupBy, now)
	if err != nil {
		return nil, err
	}
	groups := []any{}
	for _, g := range report.Groups {
		row := map[string]any{
			"key": g.Key, "label": g.Label, "billing": g.Billing, "calls": g.Calls,
			"tokens_in": g.TokensIn, "tokens_out": g.TokensOut, "tokens_cached": g.TokensCached,
			"cache_rate": ratio(g.TokensCached, g.TokensIn), "errors": g.Errors,
			"cost": usageCostView(g.Calls, g.MeteredCalls, g.UnpricedCalls, g.UnpricedIncluded, g.Cost, g.IncludedCost),
		}
		if g.AgentID != 0 {
			row["agent_id"] = g.AgentID
		}
		if groupBy == "thread" {
			row["thread_id"] = g.ThreadID
		}
		if g.ProjectID != "" {
			row["project_id"] = g.ProjectID
		}
		if g.Status != "" {
			row["status"] = g.Status
		}
		groups = append(groups, row)
	}
	result, err := gatewayPage(groups, args, "groups")
	if err != nil {
		return nil, err
	}
	previousFrom := scope.Since.Add(-scope.Range.End.Sub(scope.Range.Start))
	previous := usageTotalsView(report.Previous)
	previous["coverage"] = monitorCoverage(previousFrom, scope.Since, now)
	result["coverage"] = scope.coverage()
	result["group_by"] = groupBy
	result["totals"] = usageTotalsView(report.Totals)
	result["previous"] = previous
	result["daily_run_rate_usd"] = roundTo(report.DailyRunRate, 6)
	result["run_rate_days"] = roundTo(report.RunRateDays, 2)
	if includeTimeline {
		result["bucket"] = report.Bucket
		result["timeline"] = report.Timeline
	}
	result["definitions"] = usageDefinitions
	return result, nil
}

// --- agents_tool_usage ---

type toolUsageRow struct {
	Tool          string
	AgentID       int64
	Calls         int
	Results       int
	Failures      int
	durationSum   float64
	durationCount int
	DurationMax   float64
	LastCalled    string
	LastFailure   string
	agents        map[int64]bool
	evidence      []map[string]any
}

func gatewayAgentsToolUsage(store *Store, userID int64, args map[string]any, projectID string, now time.Time) (any, error) {
	scope, err := gatewayMonitorScope(store, userID, args, projectID, now, "24h")
	if err != nil {
		return nil, err
	}
	groupBy, _ := args["group_by"].(string)
	if groupBy = strings.TrimSpace(groupBy); groupBy == "" {
		groupBy = "tool"
	}
	if groupBy != "tool" && groupBy != "agent" {
		return nil, fmt.Errorf("group_by must be tool or agent")
	}
	sortBy, _ := args["sort"].(string)
	if sortBy = strings.TrimSpace(sortBy); sortBy == "" {
		sortBy = "calls"
	}
	if sortBy != "calls" && sortBy != "failures" && sortBy != "duration" {
		return nil, fmt.Errorf("sort must be calls, failures or duration")
	}
	query, _ := args["tool"].(string)
	names := scope.agentNames()

	rows := map[string]*toolUsageRow{}
	keyOf := func(agentID int64, tool string) string {
		if groupBy == "agent" {
			return fmt.Sprintf("%d/%s", agentID, tool)
		}
		return tool
	}
	if len(scope.Agents) > 0 {
		ids, idArgs := metricIDs(scope.Agents)
		params := append(idArgs, scope.Since.Format(metricTimeLayout), scope.Until.Format(metricTimeLayout))
		result, err := store.db.Query(`SELECT agent_id,tool_name,type,COUNT(*),SUM(is_error),COALESCE(SUM(duration),0),COUNT(duration),COALESCE(MAX(duration),0),MAX(time),COALESCE(MAX(CASE WHEN is_error=1 THEN time END),'')
 FROM telemetry_metrics WHERE agent_id IN (`+ids+`) AND type IN ('tool.call','tool.result') AND time>=? AND time<=?
 GROUP BY agent_id,tool_name,type`, params...)
		if err != nil {
			return nil, err
		}
		for result.Next() {
			var agentID int64
			var tool, kind, last, lastFailure string
			var count, failures, durationCount int
			var durationSum, durationMax float64
			if err := result.Scan(&agentID, &tool, &kind, &count, &failures, &durationSum, &durationCount, &durationMax, &last, &lastFailure); err != nil {
				result.Close()
				return nil, err
			}
			if !gatewayQueryMatch(query, tool) {
				continue
			}
			key := keyOf(agentID, tool)
			row := rows[key]
			if row == nil {
				row = &toolUsageRow{Tool: tool, AgentID: agentID, agents: map[int64]bool{}}
				rows[key] = row
			}
			row.agents[agentID] = true
			if kind == "tool.call" {
				row.Calls += count
				if last > row.LastCalled {
					row.LastCalled = last
				}
				continue
			}
			row.Results += count
			row.Failures += failures
			row.durationSum += durationSum
			row.durationCount += durationCount
			row.DurationMax = math.Max(row.DurationMax, durationMax)
			if lastFailure > row.LastFailure {
				row.LastFailure = lastFailure
			}
		}
		result.Close()
		if err := result.Err(); err != nil {
			return nil, err
		}
		evidence, err := store.db.Query(`SELECT agent_id,tool_name,thread_id,id,time FROM telemetry_metrics
 WHERE agent_id IN (`+ids+`) AND type='tool.result' AND is_error=1 AND time>=? AND time<=? ORDER BY time DESC LIMIT 1000`, params...)
		if err != nil {
			return nil, err
		}
		for evidence.Next() {
			var agentID int64
			var tool, threadID, id, at string
			if err := evidence.Scan(&agentID, &tool, &threadID, &id, &at); err != nil {
				evidence.Close()
				return nil, err
			}
			if row := rows[keyOf(agentID, tool)]; row != nil && len(row.evidence) < 3 {
				row.evidence = append(row.evidence, map[string]any{"event_id": id, "agent_id": agentID, "thread_id": threadID, "time": at})
			}
		}
		evidence.Close()
		if err := evidence.Err(); err != nil {
			return nil, err
		}
	}

	list := make([]*toolUsageRow, 0, len(rows))
	totals := map[string]int{"calls": 0, "results": 0, "failures": 0, "tools": 0}
	tools := map[string]bool{}
	for _, row := range rows {
		list = append(list, row)
		totals["calls"] += row.Calls
		totals["results"] += row.Results
		totals["failures"] += row.Failures
		tools[row.Tool] = true
	}
	totals["tools"] = len(tools)
	avg := func(r *toolUsageRow) float64 {
		if r.durationCount == 0 {
			return -1
		}
		return r.durationSum / float64(r.durationCount)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch sortBy {
		case "failures":
			if a.Failures != b.Failures {
				return a.Failures > b.Failures
			}
		case "duration":
			if avg(a) != avg(b) {
				return avg(a) > avg(b)
			}
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.AgentID < b.AgentID
	})
	items := make([]any, 0, len(list))
	for _, r := range list {
		tool := r.Tool
		if tool == "" {
			tool = "(unnamed)"
		}
		item := map[string]any{
			"tool": tool, "calls": r.Calls, "results": r.Results, "failures": r.Failures,
			"failure_rate": ratio(r.Failures, r.Results), "avg_duration_ms": nil, "max_duration_ms": nil,
			"last_called_at": nullIfEmpty(r.LastCalled), "last_failure_at": nullIfEmpty(r.LastFailure),
			"failure_evidence": r.evidence,
		}
		if r.durationCount > 0 {
			item["avg_duration_ms"] = roundTo(r.durationSum/float64(r.durationCount), 1)
			item["max_duration_ms"] = roundTo(r.DurationMax, 1)
		}
		if groupBy == "agent" {
			item["agent_id"], item["agent_name"] = r.AgentID, names[r.AgentID]
		} else {
			item["agents"] = len(r.agents)
		}
		if r.evidence == nil {
			item["failure_evidence"] = []any{}
		}
		items = append(items, item)
	}
	result, err := gatewayPage(items, args, "tools")
	if err != nil {
		return nil, err
	}
	result["coverage"] = scope.coverage()
	result["group_by"] = groupBy
	result["sort"] = sortBy
	result["totals"] = totals
	result["definitions"] = map[string]string{
		"calls":        "tool.call events: calls the agent started in the window.",
		"results":      "tool.result events in the window; a call started just before the window can finish inside it.",
		"failures":     "Tool results that report failure. failure_rate is failures / results.",
		"duration_ms":  "Execution time reported on the tool result.",
		"evidence_ids": "Telemetry event ids of the latest failures; agent_list_activity shows the events around them.",
	}
	return result, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// --- agents_errors ---

var errorCategoryTypes = map[string][]string{
	"model":    {"llm.error"},
	"provider": {"llm.provider_error"},
	"tool":     {"tool.error", "tool.result"},
	"realtime": {"realtime.error"},
	"runtime":  {"event.error"},
}

func errorCategory(kind string) (category string, counted bool) {
	switch kind {
	case "llm.error":
		return "model", true
	case "llm.provider_error":
		return "provider", false
	case "tool.error", "tool.result":
		return "tool", true
	case "realtime.error":
		return "realtime", true
	}
	return "runtime", false
}

var (
	errorSigURL    = regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s"']+`)
	errorSigQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	errorSigToken  = regexp.MustCompile(`[a-z0-9_-]{8,}`)
	errorSigNumber = regexp.MustCompile(`[0-9]+`)
	errorSigSpace  = regexp.MustCompile(`\s+`)
)

// errorSignature reduces a message to its shape so repeats of one failure
// group together: identifiers, numbers, URLs and quoted values are masked,
// which also keeps payload values out of the default reply.
func errorSignature(message string) string {
	line := strings.TrimSpace(message)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.ToLower(line)
	line = errorSigURL.ReplaceAllString(line, "<url>")
	line = errorSigQuoted.ReplaceAllString(line, "<str>")
	line = errorSigToken.ReplaceAllStringFunc(line, func(token string) string {
		if strings.ContainsAny(token, "0123456789") && strings.ContainsAny(token, "abcdefghijklmnopqrstuvwxyz") {
			return "<id>"
		}
		return token
	})
	line = errorSigNumber.ReplaceAllString(line, "#")
	line = strings.TrimSpace(errorSigSpace.ReplaceAllString(line, " "))
	if line == "" {
		return "(no message)"
	}
	return gatewayCompactText(line, 120)
}

// errorText finds the human-readable message in an error event payload.
func errorText(kind string, data map[string]any) string {
	pick := func(value any) string {
		switch v := value.(type) {
		case string:
			return v
		case map[string]any:
			for _, key := range []string{"message", "error", "detail"} {
				if s, ok := v[key].(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	switch kind {
	case "tool.result":
		result, _ := data["result"].(string)
		var doc map[string]any
		if strings.HasPrefix(strings.TrimSpace(result), "{") && json.Unmarshal([]byte(result), &doc) == nil {
			if text := pick(doc["error"]); text != "" {
				return text
			}
		}
		return result
	case "event.error":
		reason, _ := data["reason"].(string)
		return reason
	}
	if text := pick(data["error"]); text != "" {
		return text
	}
	return pick(data["message"])
}

func errorSubject(category, tool string, data map[string]any) string {
	text := func(key string) string {
		value, _ := data[key].(string)
		return value
	}
	switch category {
	case "tool":
		return tool
	case "model", "provider":
		if provider, model := text("provider"), text("model"); provider != "" || model != "" {
			return strings.Trim(provider+"/"+model, "/")
		}
	case "realtime":
		if op := text("operation"); op != "" {
			return op
		}
		return text("error_class")
	case "runtime":
		return text("source_app")
	}
	return ""
}

type errorGroup struct {
	Category  string
	Type      string
	Subject   string
	Signature string
	Counted   bool
	Count     int
	FirstSeen string
	LastSeen  string
	agents    map[int64]int
	threads   map[string]bool
	evidence  []map[string]any
	samples   []string
}

func gatewayAgentsErrors(store *Store, userID int64, args map[string]any, projectID string, now time.Time) (any, error) {
	scope, err := gatewayMonitorScope(store, userID, args, projectID, now, "24h")
	if err != nil {
		return nil, err
	}
	category, _ := args["category"].(string)
	if category = strings.TrimSpace(category); category == "" {
		category = "all"
	}
	var types []string
	if category == "all" {
		for _, c := range []string{"model", "provider", "tool", "realtime", "runtime"} {
			types = append(types, errorCategoryTypes[c]...)
		}
	} else if types = errorCategoryTypes[category]; types == nil {
		return nil, fmt.Errorf("category must be all, model, provider, tool, realtime or runtime")
	}
	includeDetails, _, err := optionalBoolArg(args["include_details"])
	if err != nil {
		return nil, fmt.Errorf("include_details must be true or false")
	}
	names := scope.agentNames()

	groups := map[string]*errorGroup{}
	byType := map[string]int{}
	scanned := 0
	if len(scope.Agents) > 0 {
		ids, idArgs := metricIDs(scope.Agents)
		slots := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
		params := append([]any{}, idArgs...)
		for _, t := range types {
			params = append(params, t)
		}
		params = append(params, scope.Since.Format(metricTimeLayout), scope.Until.Format(metricTimeLayout))
		where := `m.agent_id IN (` + ids + `) AND m.type IN (` + slots + `) AND m.time>=? AND m.time<=? AND (m.type!='tool.result' OR m.is_error=1)`

		counts, err := store.db.Query(`SELECT m.type,COUNT(*) FROM telemetry_metrics m WHERE `+where+` GROUP BY m.type`, params...)
		if err != nil {
			return nil, err
		}
		for counts.Next() {
			var kind string
			var n int
			if err := counts.Scan(&kind, &n); err != nil {
				counts.Close()
				return nil, err
			}
			byType[kind] = n
		}
		counts.Close()
		if err := counts.Err(); err != nil {
			return nil, err
		}

		rows, err := store.db.Query(`SELECT m.id,m.agent_id,m.thread_id,m.type,m.time,m.tool_name,COALESCE(t.data,'')
 FROM telemetry_metrics m LEFT JOIN telemetry t ON t.id=m.id WHERE `+where+` ORDER BY m.time DESC LIMIT ?`, append(params, monitorErrorScanCap)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, threadID, kind, at, tool, raw string
			var agentID int64
			if err := rows.Scan(&id, &agentID, &threadID, &kind, &at, &tool, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			scanned++
			var data map[string]any
			_ = json.Unmarshal([]byte(raw), &data)
			cat, counted := errorCategory(kind)
			message := errorText(kind, data)
			subject := errorSubject(cat, tool, data)
			signature := errorSignature(message)
			key := cat + "\x00" + kind + "\x00" + subject + "\x00" + signature
			g := groups[key]
			if g == nil {
				g = &errorGroup{Category: cat, Type: kind, Subject: subject, Signature: signature, Counted: counted, LastSeen: at, agents: map[int64]int{}, threads: map[string]bool{}}
				groups[key] = g
			}
			g.Count++
			g.FirstSeen = at // rows arrive newest first
			g.agents[agentID]++
			g.threads[threadID] = true
			if len(g.evidence) < 3 {
				g.evidence = append(g.evidence, map[string]any{"event_id": id, "agent_id": agentID, "thread_id": threadID, "time": at})
			}
			if includeDetails && len(g.samples) < 3 && message != "" {
				g.samples = append(g.samples, gatewayCompactText(message, 300))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	list := make([]*errorGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].LastSeen > list[j].LastSeen
	})
	items := make([]any, 0, len(list))
	for _, g := range list {
		agents := make([]map[string]any, 0, len(g.agents))
		for id, n := range g.agents {
			agents = append(agents, map[string]any{"agent_id": id, "name": names[id], "count": n})
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i]["count"].(int) > agents[j]["count"].(int) })
		if len(agents) > 5 {
			agents = agents[:5]
		}
		item := map[string]any{
			"category": g.Category, "type": g.Type, "subject": g.Subject, "signature": g.Signature,
			"counts_as_error": g.Counted, "count": g.Count, "first_seen": g.FirstSeen, "last_seen": g.LastSeen,
			"affected_agents": len(g.agents), "affected_threads": len(g.threads), "agents": agents, "evidence": g.evidence,
		}
		if includeDetails {
			item["samples"] = g.samples
		}
		items = append(items, item)
	}
	result, err := gatewayPage(items, args, "groups")
	if err != nil {
		return nil, err
	}
	totals := map[string]any{"errors": 0, "provider_attempt_failures": byType["llm.provider_error"], "execution_failures": byType["event.error"]}
	byCategory := map[string]int{}
	errorsTotal := 0
	for kind, n := range byType {
		cat, counted := errorCategory(kind)
		byCategory[cat] += n
		if counted {
			errorsTotal += n
		}
	}
	totals["errors"] = errorsTotal
	totals["by_category"] = byCategory
	coverage := scope.coverage()
	coverage["events_grouped"] = scanned
	if total := sumCounts(byType); scanned < total {
		coverage["grouping_truncated"] = true
		coverage["grouping_note"] = fmt.Sprintf("Groups cover the newest %d of %d events; totals cover all of them, and first_seen is the oldest grouped event.", scanned, total)
	}
	result["coverage"] = coverage
	result["category"] = category
	result["totals"] = totals
	result["definitions"] = map[string]string{
		"model":           "llm.error: the model call for a turn failed after any retry or fallback. Counts as an error.",
		"provider":        "llm.provider_error: one provider attempt failed; a retry or fallback may have recovered the turn. Not counted as an error.",
		"tool":            "Tool results reporting failure, and tool.error. Counts as an error.",
		"realtime":        "realtime.error: a realtime session operation failed. Counts as an error.",
		"runtime":         "event.error: a tracked execution ended in error, usually after one of the failures above. Not counted again.",
		"signature":       "The first line of the message with ids, numbers, URLs and quoted values masked. Pass include_details=true for raw samples.",
		"evidence":        "Telemetry event ids of the latest occurrences.",
		"errors (totals)": "Sum of the categories that count as errors; the same definition agents_usage and the dashboard use.",
	}
	return result, nil
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// --- agents_health ---

type agentLive struct {
	Status  map[string]any
	Threads []map[string]any
	Err     string
}

type agentWork struct {
	Queued, Claimed, Active int
	Items                   []map[string]any
}

var healthDefinitions = map[string]string{
	"state":       "stopped: not running. unknown: running, but Core's live status could not be read. failing: Core reports consecutive inference failures or a blocked thread. working: a model call is in flight or a tracked execution is active. waiting: paused, or tracked events are queued or claimed but not yet active. idle: running with nothing in flight. A quiet agent is idle, never stalled.",
	"telemetry":   "last_* timestamps search all retained telemetry; null means none was retained. Window counts cover coverage.from to coverage.to.",
	"active_work": "Receipts for app-delivered events with lifecycle tracking that have not settled. Untracked work has no receipt and is not listed.",
}

func gatewayAgentsHealth(store *Store, serverAPI gatewayAPIClient, args map[string]any, projectID string, now time.Time) (any, error) {
	scope, err := gatewayMonitorScope(store, serverAPI.userID, args, projectID, now, "1h")
	if err != nil {
		return nil, err
	}
	includeThreads, set, err := optionalBoolArg(args["include_threads"])
	if err != nil {
		return nil, fmt.Errorf("include_threads must be true or false")
	}
	if !set {
		_, includeThreads = args["agent_id"]
	}
	agents := append([]Agent(nil), scope.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	statusCounts := map[string]int{}
	for _, a := range agents {
		statusCounts[a.Status]++
	}
	offset, limit, err := gatewayPageArgs(args, len(agents))
	if err != nil {
		return nil, err
	}
	if limit > monitorHealthPageSize {
		limit = monitorHealthPageSize
	}
	end := offset + limit
	if end > len(agents) {
		end = len(agents)
	}
	page := agents[offset:end]

	window, err := healthWindowCounts(store, page, scope.Since)
	if err != nil {
		return nil, err
	}
	work, err := healthActiveWork(store, page)
	if err != nil {
		return nil, err
	}
	live := healthLive(serverAPI, page)

	rows := make([]any, 0, len(page))
	states := map[string]int{}
	for _, a := range page {
		last, err := healthLastSeen(store, a.ID)
		if err != nil {
			return nil, err
		}
		telemetry := map[string]any{"window": window[a.ID]}
		for key, value := range last {
			telemetry[key] = value
		}
		row := map[string]any{"agent_id": a.ID, "name": a.Name, "project_id": a.ProjectID, "status": a.Status, "telemetry": telemetry}
		w := work[a.ID]
		row["active_work"] = map[string]any{"tracked": w.Queued + w.Claimed + w.Active, "queued": w.Queued, "claimed": w.Claimed, "active": w.Active, "items": nonNilItems(w.Items)}
		l, running := live[a.ID]
		if running {
			row["live"] = healthLiveView(l, includeThreads)
		}
		state, reasons := healthState(a, l, running, w)
		row["state"], row["reasons"] = state, reasons
		states[state]++
		rows = append(rows, row)
	}
	result := map[string]any{
		"agents": rows, "total": len(agents), "offset": offset, "limit": limit, "has_more": end < len(agents),
		"states": states, "status_counts": statusCounts, "coverage": scope.coverage(), "definitions": healthDefinitions,
	}
	if end < len(agents) {
		result["next_offset"] = end
	}
	return result, nil
}

func nonNilItems(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}

func healthWindowCounts(store *Store, agents []Agent, since time.Time) (map[int64]map[string]int, error) {
	out := map[int64]map[string]int{}
	for _, a := range agents {
		out[a.ID] = map[string]int{"calls": 0, "errors": 0, "provider_attempt_failures": 0, "tool_results": 0, "tool_failures": 0}
	}
	if len(agents) == 0 {
		return out, nil
	}
	ids, args := metricIDs(agents)
	args = append(args, since.UTC().Format(metricTimeLayout))
	rows, err := store.db.Query(`SELECT agent_id,
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN samples ELSE 0 END),0),COALESCE(SUM(`+windowErrors+`),0),
 COALESCE(SUM(CASE WHEN type='llm.provider_error' THEN samples ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type='tool.result' THEN samples ELSE 0 END),0),COALESCE(SUM(CASE WHEN type='tool.result' THEN errors ELSE 0 END),0)
 FROM `+metricsWindow(since)+` WHERE agent_id IN (`+ids+`) AND time>=? GROUP BY agent_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var calls, errs, provider, results, failures int
		if err := rows.Scan(&id, &calls, &errs, &provider, &results, &failures); err != nil {
			return nil, err
		}
		out[id] = map[string]int{"calls": calls, "errors": errs, "provider_attempt_failures": provider, "tool_results": results, "tool_failures": failures}
	}
	return out, rows.Err()
}

// healthLastSeen reads each timestamp with its own indexed MAX lookup.
func healthLastSeen(store *Store, agentID int64) (map[string]any, error) {
	queries := map[string]string{
		"last_event_at":         `SELECT MAX(time) FROM telemetry_metrics WHERE agent_id=?`,
		"last_call_at":          `SELECT MAX(t) FROM (SELECT MAX(time) AS t FROM telemetry_metrics WHERE agent_id=? AND type='llm.done' UNION ALL SELECT MAX(time) FROM telemetry_metrics WHERE agent_id=?1 AND type='realtime.usage')`,
		"last_model_failure_at": `SELECT MAX(time) FROM telemetry_metrics WHERE agent_id=? AND type='llm.error'`,
		"last_error_at":         `SELECT MAX(t) FROM (SELECT MAX(time) AS t FROM telemetry_metrics WHERE agent_id=? AND type IN (` + errorEventTypes + `) UNION ALL SELECT MAX(time) FROM telemetry_metrics WHERE agent_id=?1 AND type='tool.result' AND is_error=1)`,
	}
	out := map[string]any{}
	for key, q := range queries {
		var value sql.NullString
		if err := store.db.QueryRow(q, agentID).Scan(&value); err != nil {
			return nil, err
		}
		out[key] = nil
		if value.Valid {
			out[key] = value.String
		}
	}
	return out, nil
}

func healthActiveWork(store *Store, agents []Agent) (map[int64]agentWork, error) {
	out := map[int64]agentWork{}
	if len(agents) == 0 {
		return out, nil
	}
	ids, args := metricIDs(agents)
	rows, err := store.db.Query(`SELECT agent_id,id,core_execution_id,source_app,thread_id,state,COALESCE(created_at,''),COALESCE(updated_at,'')
 FROM agent_event_executions WHERE agent_id IN (`+ids+`) AND state IN ('queued','event.claimed','event.active') ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agentID, id int64
		var executionID, sourceApp, threadID, state, created, updated string
		if err := rows.Scan(&agentID, &id, &executionID, &sourceApp, &threadID, &state, &created, &updated); err != nil {
			return nil, err
		}
		w := out[agentID]
		switch state {
		case "queued":
			w.Queued++
		case "event.claimed":
			w.Claimed++
		default:
			w.Active++
		}
		if len(w.Items) < 5 {
			w.Items = append(w.Items, map[string]any{
				"receipt_id": id, "execution_id": nullIfEmpty(executionID), "source_app": sourceApp, "thread_id": threadID,
				"state": state, "created_at": sqliteTimeRFC3339(created), "updated_at": sqliteTimeRFC3339(updated),
			})
		}
		out[agentID] = w
	}
	return out, rows.Err()
}

// sqliteTimeRFC3339 turns a CURRENT_TIMESTAMP value (UTC) into RFC 3339.
func sqliteTimeRFC3339(value string) any {
	if value == "" {
		return nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", value); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return value
}

// healthLive asks each running agent's Core for its status and threads,
// a few agents at a time and with a short timeout.
func healthLive(api gatewayAPIClient, agents []Agent) map[int64]agentLive {
	out := map[int64]agentLive{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 6)
	for _, a := range agents {
		if a.Status != "running" {
			continue
		}
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			var l agentLive
			if err := api.doWithTimeout(http.MethodGet, fmt.Sprintf("/agents/%d/status", id), nil, &l.Status, monitorLiveTimeout); err != nil {
				l.Err = liveError(err)
			} else if err := api.doWithTimeout(http.MethodGet, fmt.Sprintf("/agents/%d/threads", id), nil, &l.Threads, monitorLiveTimeout); err != nil {
				l.Err = liveError(err)
			}
			mu.Lock()
			out[id] = l
			mu.Unlock()
		}(a.ID)
	}
	wg.Wait()
	return out
}

func liveError(err error) string {
	if apiErr, ok := err.(*gatewayAPIError); ok {
		return fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, gatewayCompactText(apiErr.Body, 160))
	}
	return gatewayCompactText(err.Error(), 200)
}

func healthInference(value any) (failures int, blocked string, lastSuccess any) {
	inference, _ := value.(map[string]any)
	failures = gatewayIntFromAny(inference["consecutive_failures"])
	blocked, _ = inference["blocked_reason"].(string)
	lastSuccess = inference["last_successful_inference"]
	return
}

func healthLiveView(l agentLive, includeThreads bool) map[string]any {
	if l.Err != "" {
		return map[string]any{"available": false, "error": l.Err}
	}
	failures, blocked, lastSuccess := healthInference(l.Status["inference"])
	view := map[string]any{"available": true}
	for _, key := range []string{"paused", "llm_active", "iteration", "uptime_seconds", "next_wake_at", "model", "workflow_healthy", "core_version"} {
		if value, ok := l.Status[key]; ok {
			view[key] = value
		}
	}
	view["inference"] = map[string]any{"consecutive_failures": failures, "blocked_reason": nullIfEmpty(blocked), "last_successful_inference": lastSuccess}
	failing := []string{}
	threads := []any{}
	for _, t := range l.Threads {
		id, _ := t["id"].(string)
		failures, blocked, lastSuccess := healthInference(t["inference"])
		if (failures > 0 || blocked != "") && id != "main" {
			failing = append(failing, id)
		}
		if !includeThreads || len(threads) >= monitorThreadCap {
			continue
		}
		row := map[string]any{"id": id, "consecutive_failures": failures, "blocked_reason": nullIfEmpty(blocked), "last_successful_inference": lastSuccess}
		for _, key := range []string{"name", "parent_id", "depth", "iteration", "rate", "next_wake_at", "model", "provider", "realtime", "age"} {
			if value, ok := t[key]; ok {
				row[key] = value
			}
		}
		threads = append(threads, row)
	}
	view["thread_count"] = len(l.Threads)
	view["failing_threads"] = failing
	if includeThreads {
		view["threads"] = threads
		if len(l.Threads) > monitorThreadCap {
			view["threads_truncated"] = true
		}
	}
	return view
}

func healthState(a Agent, l agentLive, fetched bool, w agentWork) (string, []string) {
	if a.Status != "running" {
		return "stopped", []string{"agent status is " + a.Status}
	}
	if !fetched || l.Err != "" {
		reason := "live status was not read"
		if l.Err != "" {
			reason = "live status unavailable: " + l.Err
		}
		return "unknown", []string{reason}
	}
	var reasons []string
	check := func(thread string, value any) {
		failures, blocked, _ := healthInference(value)
		if failures > 0 {
			reasons = append(reasons, fmt.Sprintf("%s: %d consecutive inference failures", thread, failures))
		}
		if blocked != "" {
			reasons = append(reasons, thread+": blocked ("+blocked+")")
		}
	}
	check("main", l.Status["inference"])
	for _, t := range l.Threads {
		if id, _ := t["id"].(string); id != "main" {
			check(id, t["inference"])
		}
	}
	if len(reasons) > 0 {
		return "failing", reasons
	}
	if active, _ := l.Status["llm_active"].(bool); active {
		reasons = append(reasons, "model call in flight")
	}
	if w.Active > 0 {
		reasons = append(reasons, fmt.Sprintf("%d tracked execution(s) active", w.Active))
	}
	if len(reasons) > 0 {
		return "working", reasons
	}
	if paused, _ := l.Status["paused"].(bool); paused {
		reasons = append(reasons, "paused")
	}
	if w.Queued+w.Claimed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d tracked event(s) queued or claimed", w.Queued+w.Claimed))
	}
	if len(reasons) > 0 {
		return "waiting", reasons
	}
	return "idle", []string{"nothing in flight"}
}

// --- agents_threads ---

var threadDefinitions = map[string]string{
	"state":            "live: listed by the running agent's Core. failing: live, and Core reports consecutive inference failures or a block. stopped: a saved thread of a stopped agent. ended: seen in telemetry during the window but no longer listed. unknown: the agent's thread list could not be read. A quiet thread keeps its state; read last_activity_at for how long it has been quiet.",
	"window":           "Counts for coverage.from to coverage.to; errors use the same definition as agents_usage and agents_errors.",
	"last_activity_at": "Start of the latest minute with any event on the thread, across retained telemetry. last_error_at is the same for errors. null means none was retained.",
}

type threadListing struct {
	Threads []map[string]any
	Err     string
}

type threadKey struct {
	agent  int64
	thread string
}

type threadRow struct {
	agent        Agent
	id           string
	info         map[string]any
	state        string
	reasons      []string
	window       map[string]int
	lastActivity string
	lastError    string
}

func gatewayAgentsThreads(store *Store, serverAPI gatewayAPIClient, args map[string]any, projectID string, now time.Time) (any, error) {
	scope, err := gatewayMonitorScope(store, serverAPI.userID, args, projectID, now, "24h")
	if err != nil {
		return nil, err
	}
	stateFilter, _ := args["state"].(string)
	stateFilter = strings.TrimSpace(stateFilter)
	if stateFilter != "" && stateFilter != "failing" && stateFilter != "live" && stateFilter != "stopped" && stateFilter != "ended" && stateFilter != "unknown" {
		return nil, fmt.Errorf("state must be failing, live, stopped, ended or unknown")
	}
	sortBy, _ := args["sort"].(string)
	if sortBy = strings.TrimSpace(sortBy); sortBy == "" {
		sortBy = "activity"
	}
	if sortBy != "activity" && sortBy != "errors" && sortBy != "calls" {
		return nil, fmt.Errorf("sort must be activity, errors or calls")
	}
	query, _ := args["query"].(string)

	agents := append([]Agent(nil), scope.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	fetched := agents
	if len(fetched) > monitorThreadAgentCap {
		fetched = fetched[:monitorThreadAgentCap]
	}
	listings := threadListings(serverAPI, fetched)
	window, last, err := threadTelemetry(store, agents, scope.Since, scope.Until)
	if err != nil {
		return nil, err
	}

	rows := map[threadKey]*threadRow{}
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
		listing, ok := listings[a.ID]
		if !ok || listing.Err != "" {
			continue
		}
		for _, t := range listing.Threads {
			id, _ := t["id"].(string)
			if id == "" {
				continue
			}
			row := &threadRow{agent: a, id: id, info: t, state: "live"}
			if t["sleep_state"] == "stopped" {
				row.state, row.reasons = "stopped", []string{"saved thread of a stopped agent"}
			} else if failures, blocked, _ := healthInference(t["inference"]); failures > 0 || blocked != "" {
				row.state = "failing"
				if failures > 0 {
					row.reasons = append(row.reasons, fmt.Sprintf("%d consecutive inference failures", failures))
				}
				if blocked != "" {
					row.reasons = append(row.reasons, "blocked ("+blocked+")")
				}
			}
			rows[threadKey{a.ID, id}] = row
		}
	}
	since := scope.Since.Format(metricTimeLayout)
	for k, seen := range last {
		if rows[k] != nil || (window[k] == nil && seen[0] < since) {
			continue
		}
		row := &threadRow{agent: byID[k.agent], id: k.thread, state: "ended", reasons: []string{"active in the window but no longer listed"}}
		if listing, ok := listings[k.agent]; !ok {
			row.state, row.reasons = "unknown", []string{fmt.Sprintf("thread lists are read for the first %d agents; pass agent_id", monitorThreadAgentCap)}
		} else if listing.Err != "" {
			row.state, row.reasons = "unknown", []string{"thread list unavailable: " + listing.Err}
		}
		rows[k] = row
	}

	stateCounts := map[string]int{}
	list := make([]*threadRow, 0, len(rows))
	for k, row := range rows {
		row.window = window[k]
		if row.window == nil {
			row.window = map[string]int{"calls": 0, "errors": 0, "provider_attempt_failures": 0, "tool_calls": 0, "tool_failures": 0}
		}
		if seen, ok := last[k]; ok {
			row.lastActivity, row.lastError = seen[0], seen[1]
		}
		stateCounts[row.state]++
		name, _ := row.info["name"].(string)
		if (stateFilter == "" || row.state == stateFilter) && gatewayQueryMatch(query, row.id, name, row.agent.Name) {
			list = append(list, row)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch sortBy {
		case "errors":
			if a.window["errors"] != b.window["errors"] {
				return a.window["errors"] > b.window["errors"]
			}
		case "calls":
			if a.window["calls"] != b.window["calls"] {
				return a.window["calls"] > b.window["calls"]
			}
		}
		if a.lastActivity != b.lastActivity {
			return a.lastActivity > b.lastActivity
		}
		if a.agent.ID != b.agent.ID {
			return a.agent.ID < b.agent.ID
		}
		return a.id < b.id
	})
	items := make([]any, 0, len(list))
	for _, row := range list {
		items = append(items, row.view())
	}
	result, err := gatewayPage(items, args, "threads")
	if err != nil {
		return nil, err
	}
	coverage := scope.coverage()
	coverage["thread_lists_read"] = len(fetched)
	if len(fetched) < len(agents) {
		coverage["thread_lists_truncated"] = true
	}
	result["coverage"] = coverage
	result["state_counts"] = stateCounts
	result["sort"] = sortBy
	result["definitions"] = threadDefinitions
	return result, nil
}

func (row *threadRow) view() map[string]any {
	out := map[string]any{
		"agent_id": row.agent.ID, "agent_name": row.agent.Name, "agent_status": row.agent.Status,
		"thread_id": row.id, "state": row.state, "reasons": row.reasons, "window": row.window,
		"last_activity_at": nullIfEmpty(row.lastActivity), "last_error_at": nullIfEmpty(row.lastError),
	}
	if row.reasons == nil {
		out["reasons"] = []string{}
	}
	for _, key := range []string{"name", "parent_id", "depth", "model", "provider", "realtime", "iteration", "rate", "next_wake_at", "age"} {
		if value, ok := row.info[key]; ok && value != nil && value != "" {
			out[key] = value
		}
	}
	if row.state == "live" || row.state == "failing" {
		failures, blocked, lastSuccess := healthInference(row.info["inference"])
		out["inference"] = map[string]any{"consecutive_failures": failures, "blocked_reason": nullIfEmpty(blocked), "last_successful_inference": lastSuccess}
	}
	return out
}

// threadListings reads each agent's thread list. The server answers for a
// stopped agent from its saved configuration.
func threadListings(api gatewayAPIClient, agents []Agent) map[int64]threadListing {
	out := map[int64]threadListing{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 6)
	for _, a := range agents {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			var l threadListing
			if err := api.doWithTimeout(http.MethodGet, fmt.Sprintf("/agents/%d/threads", id), nil, &l.Threads, monitorLiveTimeout); err != nil {
				l.Err = liveError(err)
			}
			mu.Lock()
			out[id] = l
			mu.Unlock()
		}(a.ID)
	}
	wg.Wait()
	return out
}

// threadTelemetry returns window counts and the latest active and error
// minutes per thread.
func threadTelemetry(store *Store, agents []Agent, since, until time.Time) (map[threadKey]map[string]int, map[threadKey][2]string, error) {
	window := map[threadKey]map[string]int{}
	last := map[threadKey][2]string{}
	if len(agents) == 0 {
		return window, last, nil
	}
	ids, args := metricIDs(agents)
	rows, err := store.db.Query(`SELECT agent_id,thread_id,
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN samples ELSE 0 END),0),COALESCE(SUM(`+windowErrors+`),0),
 COALESCE(SUM(CASE WHEN type='llm.provider_error' THEN samples ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type='tool.call' THEN samples ELSE 0 END),0),COALESCE(SUM(CASE WHEN type='tool.result' THEN errors ELSE 0 END),0)
 FROM `+metricsWindow(since)+` WHERE agent_id IN (`+ids+`) AND thread_id!='' AND time>=? AND time<=? GROUP BY agent_id,thread_id`,
		append(append([]any{}, args...), since.UTC().Format(metricTimeLayout), until.UTC().Format(metricTimeLayout))...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var agentID int64
		var threadID string
		var calls, errs, provider, tools, failures int
		if err := rows.Scan(&agentID, &threadID, &calls, &errs, &provider, &tools, &failures); err != nil {
			rows.Close()
			return nil, nil, err
		}
		window[threadKey{agentID, threadID}] = map[string]int{"calls": calls, "errors": errs, "provider_attempt_failures": provider, "tool_calls": tools, "tool_failures": failures}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	minute := func(bucket string) string {
		return "strftime('%Y-%m-%dT%H:%M:%S.000000000Z'," + bucket + ",'unixepoch')"
	}
	rows, err = store.db.Query(`SELECT agent_id,thread_id,`+minute("MAX(bucket)")+`,COALESCE(`+minute("MAX(CASE WHEN errors>0 OR type IN ("+errorEventTypes+") THEN bucket END)")+`,'')
 FROM telemetry_rollups WHERE agent_id IN (`+ids+`) AND thread_id!='' GROUP BY agent_id,thread_id`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agentID int64
		var threadID, active, failed string
		if err := rows.Scan(&agentID, &threadID, &active, &failed); err != nil {
			return nil, nil, err
		}
		last[threadKey{agentID, threadID}] = [2]string{active, failed}
	}
	return window, last, rows.Err()
}
