package main

// Host SSH evidence in the platform audit trail.
//
// apteva-host runs the one collector per machine (internal/sshwatch): it
// reads sshd records and authorized_keys changes, stores them durably,
// evaluates its own alert rules and delivers alerts to its own webhook —
// all of which keeps working while apteva-server is down. The server only
// copies those events into its security log so the Security page, alert
// list, hash chain and independent forwarding cover SSH access too.
//
// Copying is idempotent: each host event keeps its host ID as EventID, so a
// restarted server, two server slots sharing the log, or a reset cursor
// re-reading old pages never produces duplicates.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apteva/server/internal/host"
	"github.com/apteva/server/internal/sshwatch"
)

const hostSecurityCursorKey = "host_ssh_cursor"

type hostSecurityIngest struct {
	mu         sync.Mutex
	lastRunAt  time.Time
	lastOKAt   time.Time
	lastErr    string
	ingested   int64
	cursorSeen int64
}

func hostSecuritySocket() string { return strings.TrimSpace(os.Getenv("APTEVA_HOST_SOCKET")) }

func hostSecurityPollInterval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("APTEVA_SECURITY_HOST_POLL")); err == nil && d >= time.Second {
		return d
	}
	return 15 * time.Second
}

// startHostSecurityIngest copies host SSH events on an interval until ctx is
// done. It is a no-op outside apteva-host or without an audit log.
func (s *Server) startHostSecurityIngest(ctx context.Context) {
	if s.securityAudit == nil || hostSecuritySocket() == "" {
		return
	}
	go func() {
		t := time.NewTicker(hostSecurityPollInterval())
		defer t.Stop()
		for {
			_, _ = s.syncHostSecurityEvents(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// syncHostSecurityEvents drains every host event after the stored cursor.
func (s *Server) syncHostSecurityEvents(ctx context.Context) (int, error) {
	socket := hostSecuritySocket()
	if s.securityAudit == nil || socket == "" {
		return 0, fmt.Errorf("not running under apteva-host")
	}
	client := host.UnixClient(socket)
	defer client.CloseIdleConnections()
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	total, err := s.ingestHostSecurity(callCtx, func(ctx context.Context, after int64) (sshwatch.EventsPage, error) {
		return host.SecurityEvents(ctx, client, after, 500)
	}, func(ctx context.Context) (sshwatch.Health, error) {
		return host.SecurityHealth(ctx, client)
	})
	st := &s.hostIngest
	st.mu.Lock()
	st.lastRunAt = time.Now().UTC()
	if err != nil {
		st.lastErr = err.Error()
	} else {
		st.lastErr = ""
		st.lastOKAt = st.lastRunAt
	}
	st.ingested += int64(total)
	st.cursorSeen = s.securityAudit.metaInt(hostSecurityCursorKey)
	st.mu.Unlock()
	return total, err
}

type hostEventsFetcher func(ctx context.Context, after int64) (sshwatch.EventsPage, error)
type hostHealthFetcher func(ctx context.Context) (sshwatch.Health, error)

func (s *Server) ingestHostSecurity(ctx context.Context, fetch hostEventsFetcher, healthOf hostHealthFetcher) (int, error) {
	health, err := healthOf(ctx)
	if err != nil {
		return 0, fmt.Errorf("host collector health: %w", err)
	}
	cursor := s.securityAudit.metaInt(hostSecurityCursorKey)
	if health.LastSeq < cursor {
		// The host store was replaced (new machine, wiped directory): start
		// over. Event IDs make the re-read harmless.
		cursor = 0
	}
	total := 0
	for page := 0; page < 200; page++ {
		res, err := fetch(ctx, cursor)
		if err != nil {
			return total, fmt.Errorf("host security events: %w", err)
		}
		if len(res.Events) == 0 {
			break
		}
		batch := make([]SecurityEvent, 0, len(res.Events))
		for _, ev := range res.Events {
			batch = append(batch, securityEventFromHost(ev, health.WebhookConfigured))
		}
		n, err := s.securityAudit.RecordBatch(ctx, batch)
		total += n
		if err != nil {
			return total, err
		}
		next := res.NextAfter
		if next <= cursor {
			next = res.Events[len(res.Events)-1].Seq
		}
		if next <= cursor {
			break
		}
		cursor = next
		if err := s.securityAudit.setMetaInt(hostSecurityCursorKey, cursor); err != nil {
			return total, err
		}
	}
	return total, nil
}

func hostSeverity(sev string) string {
	switch strings.ToLower(sev) {
	case sshwatch.SeverityCritical, sshwatch.SeverityHigh:
		return securitySeverityCritical
	case sshwatch.SeverityMedium:
		return securitySeverityWarning
	case sshwatch.SeverityLow:
		return securitySeverityNotice
	}
	return securitySeverityWarning
}

var securitySeverityRank = map[string]int{securitySeverityInfo: 0, securitySeverityNotice: 1, securitySeverityWarning: 2, securitySeverityCritical: 3}

// securityEventFromHost maps one collector event. Key fingerprints are
// public identifiers and are kept; key material never leaves the host.
func securityEventFromHost(ev sshwatch.Event, hostDeliversAlerts bool) SecurityEvent {
	out := SecurityEvent{
		EventID:       "host:" + ev.ID,
		OccurredAt:    ev.ObservedAt.UTC(),
		Category:      "ssh",
		Action:        "ssh." + strings.TrimPrefix(ev.Kind, "ssh_"),
		Result:        securityResultSuccess,
		Severity:      securitySeverityInfo,
		PrincipalType: "os_user",
		PrincipalID:   ev.User,
		SourceIP:      ev.SourceIP,
		MachineID:     ev.MachineID,
		TargetType:    "machine",
		TargetID:      firstNonEmpty(ev.Hostname, ev.MachineID),
	}
	if ev.User == "" {
		out.PrincipalType = "system"
	}
	if ev.Fingerprint != "" {
		out.CredentialID = "ssh_key:" + ev.Fingerprint
	}
	switch ev.Result {
	case sshwatch.ResultFailed, sshwatch.ResultInvalidUser:
		out.Result = securityResultFailure
	}
	if ev.Kind == sshwatch.KindCollectorGap {
		out.Result = securityResultError
		out.Severity = securitySeverityWarning
	}
	details := map[string]any{"host_seq": ev.Seq, "outcome": ev.Result}
	set := func(k string, v any) {
		switch t := v.(type) {
		case string:
			if t != "" {
				details[k] = t
			}
		case int:
			if t != 0 {
				details[k] = t
			}
		case time.Time:
			if !t.IsZero() {
				details[k] = t.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	set("hostname", ev.Hostname)
	set("method", ev.Method)
	set("key_type", ev.KeyType)
	set("fingerprint", ev.Fingerprint)
	set("source_port", ev.SourcePort)
	set("pid", ev.PID)
	set("log_source", ev.Source)
	set("message", ev.Message)
	set("path", ev.Path)
	set("key_count", ev.KeyCount)
	set("gap_from", ev.GapFrom)
	set("gap_to", ev.GapTo)
	if keys := hostKeyList(ev.KeysAdded); len(keys) > 0 {
		details["keys_added"] = keys
	}
	if keys := hostKeyList(ev.KeysRemoved); len(keys) > 0 {
		details["keys_removed"] = keys
	}
	if len(ev.Alerts) > 0 {
		alerts := make([]any, 0, len(ev.Alerts))
		for _, a := range ev.Alerts {
			sev := hostSeverity(a.Severity)
			if securitySeverityRank[sev] > securitySeverityRank[out.Severity] {
				out.Severity = sev
			}
			alerts = append(alerts, map[string]any{"rule": a.Rule, "severity": sev, "message": a.Message, "host_severity": a.Severity})
		}
		details["alerts"] = alerts
		// The host already delivers these to its own webhook (that path
		// works with the server down); the server lists them without
		// sending them a second time.
		details["host_delivers_alerts"] = hostDeliversAlerts
	}
	out.Details = details
	return out
}

func hostKeyList(keys []sshwatch.AuthorizedKey) []any {
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		m := map[string]any{"fingerprint": k.Fingerprint, "type": k.Type}
		if k.Comment != "" {
			m["comment"] = k.Comment
		}
		out = append(out, m)
	}
	return out
}

// securityHostStatus summarises the collector for the Security page.
func (s *Server) securityHostStatus(ctx context.Context) map[string]any {
	socket := hostSecuritySocket()
	if socket == "" {
		return map[string]any{"available": false, "reason": "apteva-server is not running under apteva-host, so no SSH collector is attached; SSH access to this machine is not monitored by Apteva."}
	}
	st := &s.hostIngest
	st.mu.Lock()
	ingest := map[string]any{"last_run_at": timeOrEmpty(st.lastRunAt), "last_ok_at": timeOrEmpty(st.lastOKAt), "last_error": st.lastErr,
		"ingested": st.ingested, "cursor": st.cursorSeen}
	st.mu.Unlock()
	client := host.UnixClient(socket)
	defer client.CloseIdleConnections()
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	health, err := host.SecurityHealth(callCtx, client)
	if err != nil {
		return map[string]any{"available": false, "reason": "apteva-host did not answer: " + err.Error(), "ingest": ingest}
	}
	var problems []string
	if health.Stale {
		problems = append(problems, "collector heartbeat is stale")
	}
	if health.SourceError != "" {
		problems = append(problems, "log source: "+health.SourceError)
	}
	if health.AuthorizedKeysError != "" {
		problems = append(problems, "authorized_keys watch: "+health.AuthorizedKeysError)
	}
	if health.StoreError != "" {
		problems = append(problems, "event store: "+health.StoreError)
	}
	if health.LowSpace {
		problems = append(problems, "collector disk space is low")
	}
	if !health.WebhookConfigured {
		problems = append(problems, "no host alert webhook: SSH alerts are not delivered while apteva-server is down")
	}
	if health.AlertLastError != "" {
		problems = append(problems, "host alert delivery: "+health.AlertLastError)
	}
	return map[string]any{"available": true, "healthy": len(problems) == 0, "problems": problems, "health": health, "ingest": ingest}
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleSecurityHost serves /security/host[/policy|/sync] (admin-only; the
// caller already checked).
func (s *Server) handleSecurityHost(w http.ResponseWriter, r *http.Request, rest string) {
	socket := hostSecuritySocket()
	switch {
	case (rest == "" || rest == "status") && r.Method == http.MethodGet:
		writeJSON(w, s.securityHostStatus(r.Context()))
		return
	case socket == "":
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "host_collector_unavailable", "message": "apteva-server is not running under apteva-host"})
		return
	}
	client := host.UnixClient(socket)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch {
	case rest == "policy" && r.Method == http.MethodGet:
		p, err := host.GetSecurityPolicy(ctx, client)
		if err != nil {
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, p)
	case rest == "policy" && r.Method == http.MethodPut:
		if !s.requirePrivilegedAuth(w, r, "ssh.policy_update") {
			return
		}
		var p sshwatch.Policy
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid policy: " + err.Error()})
			return
		}
		before, _ := host.GetSecurityPolicy(ctx, client)
		applied, err := host.PutSecurityPolicy(ctx, client, p)
		ev := s.securityEventFromRequest(r, "config", "ssh.policy_update", securityResultSuccess)
		ev.Severity = securitySeverityWarning
		ev.TargetType = "ssh_policy"
		ev.Details = map[string]any{"before": hostPolicySummary(before), "after": hostPolicySummary(applied)}
		if err != nil {
			ev.Result = securityResultFailure
			ev.Details["error"] = err.Error()
			s.recordSecurity(r, ev)
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		s.recordSecurity(r, ev)
		writeJSON(w, applied)
	case rest == "sync" && r.Method == http.MethodPost:
		n, err := s.syncHostSecurityEvents(r.Context())
		if err != nil {
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{"ingested": n, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ingested": n})
	default:
		http.NotFound(w, r)
	}
}

// hostPolicySummary is what the audit record keeps of a policy: the shape of
// the change, never the webhook secret.
func hostPolicySummary(p sshwatch.Policy) map[string]any {
	return map[string]any{
		"approved_sources": p.ApprovedSources, "approved_fingerprints": p.ApprovedFingerprints,
		"root_login_allowed": p.RootLoginAllowed, "root_approved_sources": p.RootApprovedSources,
		"failure_burst_threshold": p.FailureBurstThreshold, "failure_burst_window_seconds": p.FailureBurstWindowSeconds,
		"webhook_configured": p.AlertWebhookURL != "",
	}
}
