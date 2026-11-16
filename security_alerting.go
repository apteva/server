package main

// security_alerting.go — deterministic alert rules over the security trail
// and delivery of alerts to an independent webhook.
//
// No model calls: each rule is a pure function of the event (plus, for
// "new source", a lookup of the user's previous successful logins).
// Alerts are stored in security_alerts keyed by event+rule, so a replayed
// event (host collector re-delivery, restart) can never alert twice.
// Delivery is an outbox: APTEVA_SECURITY_ALERT_WEBHOOK receives a JSON POST
// signed with HMAC-SHA256 (APTEVA_SECURITY_ALERT_SECRET) in
// X-Apteva-Signature, with Idempotency-Key = alert key.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type SecurityAlert struct {
	ID             int64  `json:"id"`
	AlertKey       string `json:"alert_key"`
	EventID        string `json:"event_id"`
	Rule           string `json:"rule"`
	Severity       string `json:"severity"`
	Message        string `json:"message"`
	MachineID      string `json:"machine_id,omitempty"`
	CreatedAt      string `json:"created_at"`
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
	AcknowledgedBy int64  `json:"acknowledged_by,omitempty"`
	DeliveredAt    string `json:"delivered_at,omitempty"`
	DeliveryError  string `json:"delivery_error,omitempty"`
}

type securityAlertRule struct {
	Rule, Severity, Message string
}

func (l *securityAuditLog) migrateAlerts() {
	for _, ddl := range []string{
		`ALTER TABLE security_alerts ADD COLUMN delivered_at TEXT`,
		`ALTER TABLE security_alerts ADD COLUMN delivery_attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE security_alerts ADD COLUMN delivery_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE security_alerts ADD COLUMN event_json TEXT NOT NULL DEFAULT '{}'`,
	} {
		_, _ = l.db.Exec(ddl) // duplicate-column errors are expected after the first boot
	}
}

// evaluateSecurityRules returns the alerts an event triggers.
func (l *securityAuditLog) evaluateSecurityRules(ev SecurityEvent) []securityAlertRule {
	var out []securityAlertRule
	switch {
	case ev.Action == "audit.events_dropped":
		out = append(out, securityAlertRule{"audit_events_dropped", securitySeverityCritical, "Security audit events were dropped (queue full)."})
	case ev.Action == "auth.login" && ev.Result == securityResultSuccess && ev.PrincipalType == "user":
		if ev.SourceIP != "" && !l.seenLoginSource(ev) {
			out = append(out, securityAlertRule{"login_new_source", securitySeverityWarning,
				fmt.Sprintf("User %s signed in from a new source address %s.", ev.TargetID, ev.SourceIP)})
		}
	case ev.Action == "auth.login" && ev.Result == securityResultFailure:
		if l.recentFailures(ev.SourceIP, 10*time.Minute) >= 10 {
			out = append(out, securityAlertRule{"login_failure_burst", securitySeverityWarning,
				fmt.Sprintf("Repeated failed sign-ins from %s.", ev.SourceIP)})
		}
	case ev.Action == "credential.export" && ev.Result == securityResultSuccess:
		out = append(out, securityAlertRule{"credential_exported", securitySeverityWarning,
			fmt.Sprintf("Stored credentials for %s were revealed by %s %s.", ev.TargetID, ev.PrincipalType, ev.PrincipalID)})
	case ev.Action == "permission.platform_role" && ev.Result == securityResultSuccess:
		out = append(out, securityAlertRule{"platform_role_changed", securitySeverityWarning,
			fmt.Sprintf("Platform role of user %s changed.", ev.TargetID)})
	case ev.Action == "api_key.create" && ev.PrincipalType != "user":
		out = append(out, securityAlertRule{"api_key_created_by_non_user", securitySeverityWarning,
			fmt.Sprintf("An API key was created by %s %s.", ev.PrincipalType, ev.PrincipalID)})
	case ev.Action == "auth.register" && ev.Result == securityResultSuccess:
		if first, _ := ev.Details["first_user"].(bool); first {
			out = append(out, securityAlertRule{"admin_bootstrap", securitySeverityCritical, "The first (admin) account was registered."})
		}
	case ev.Category == "privileged" && ev.Result == securityResultDenied:
		out = append(out, securityAlertRule{"privileged_denied", securitySeverityNotice,
			fmt.Sprintf("A privileged operation (%s) was refused for %s %s.", ev.Action, ev.PrincipalType, ev.PrincipalID)})
	case ev.Category == "containment":
		sev := securitySeverityWarning
		if ev.Result != securityResultSuccess {
			sev = securitySeverityCritical
		}
		out = append(out, securityAlertRule{"containment_action", sev, fmt.Sprintf("Containment action %s (%s).", ev.Action, ev.Result)})
	case ev.Category == "ssh":
		// Host collector events carry their own deterministic verdicts.
		for _, raw := range anySlice(ev.Details["alerts"]) {
			m, _ := raw.(map[string]any)
			rule, _ := m["rule"].(string)
			if rule == "" {
				continue
			}
			sev, _ := m["severity"].(string)
			msg, _ := m["message"].(string)
			if sev == "" {
				sev = securitySeverityWarning
			}
			out = append(out, securityAlertRule{"ssh_" + rule, sev, msg})
		}
	}
	return out
}

func anySlice(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = t[i]
		}
		return out
	}
	return nil
}

func (l *securityAuditLog) seenLoginSource(ev SecurityEvent) bool {
	var n int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE action = 'auth.login' AND result = 'success'
		AND principal_type = 'user' AND principal_id = ? AND source_ip = ? AND event_id != ? AND occurred_at >= ?`,
		ev.PrincipalID, ev.SourceIP, ev.EventID, time.Now().UTC().Add(-90*24*time.Hour).Format(time.RFC3339Nano)).Scan(&n)
	if n > 0 {
		return true
	}
	// The very first login of an account has nothing to compare with.
	var any int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE action = 'auth.login' AND result = 'success'
		AND principal_type = 'user' AND principal_id = ? AND event_id != ?`, ev.PrincipalID, ev.EventID).Scan(&any)
	return any == 0
}

func (l *securityAuditLog) recentFailures(ip string, window time.Duration) int {
	if ip == "" {
		return 0
	}
	var n int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE action = 'auth.login' AND result = 'failure' AND source_ip = ? AND occurred_at >= ?`,
		ip, time.Now().UTC().Add(-window).Format(time.RFC3339Nano)).Scan(&n)
	return n
}

// raiseAlerts stores alerts for an inserted event. Called from the writer.
func (l *securityAuditLog) raiseAlerts(ev SecurityEvent) {
	rules := l.evaluateSecurityRules(ev)
	if len(rules) == 0 {
		return
	}
	payload, _ := json.Marshal(ev)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// SSH alerts already went out through apteva-host's own webhook (which
	// works while the server is down); list them without sending again.
	var deliveredAt any
	if ev.Category == "ssh" && ev.Details["host_delivers_alerts"] == true {
		deliveredAt = now
	}
	for _, rule := range rules {
		key := ev.EventID + ":" + rule.Rule
		res, err := l.db.Exec(`INSERT OR IGNORE INTO security_alerts(alert_key, event_id, rule, severity, message, machine_id, created_at, event_json, delivered_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, key, ev.EventID, rule.Rule, rule.Severity, rule.Message, ev.MachineID,
			now, string(payload), deliveredAt)
		if err != nil {
			l.noteError(err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			log.Printf("[SECURITY] ALERT %s (%s): %s", rule.Rule, rule.Severity, rule.Message)
		}
	}
	l.signalAlerts()
}

func (l *securityAuditLog) signalAlerts() {
	select {
	case l.alertWake <- struct{}{}:
	default:
	}
}

func (l *securityAuditLog) Alerts(openOnly bool, limit int) ([]SecurityAlert, error) {
	if l == nil {
		return nil, fmt.Errorf("security audit log unavailable")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, alert_key, event_id, rule, severity, message, machine_id, created_at, COALESCE(acknowledged_at,''),
		acknowledged_by, COALESCE(delivered_at,''), delivery_error FROM security_alerts`
	if openOnly {
		query += ` WHERE acknowledged_at IS NULL`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	rows, err := l.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecurityAlert
	for rows.Next() {
		var a SecurityAlert
		if err := rows.Scan(&a.ID, &a.AlertKey, &a.EventID, &a.Rule, &a.Severity, &a.Message, &a.MachineID, &a.CreatedAt,
			&a.AcknowledgedAt, &a.AcknowledgedBy, &a.DeliveredAt, &a.DeliveryError); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (l *securityAuditLog) AcknowledgeAlert(id, by int64) (bool, error) {
	res, err := l.db.Exec(`UPDATE security_alerts SET acknowledged_at = ?, acknowledged_by = ? WHERE id = ? AND acknowledged_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ─── delivery ─────────────────────────────────────────────────────────────

func (l *securityAuditLog) alertDeliverer(url, secret string) {
	defer l.wg.Done()
	client := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-l.alertWake:
		case <-tick.C:
		}
		l.deliverPendingAlerts(client, url, secret)
	}
}

func (l *securityAuditLog) deliverPendingAlerts(client *http.Client, url, secret string) {
	rows, err := l.db.Query(`SELECT id, alert_key, rule, severity, message, machine_id, created_at, event_json, delivery_attempts
		FROM security_alerts WHERE delivered_at IS NULL AND delivery_attempts < 50 ORDER BY id ASC LIMIT 50`)
	if err != nil {
		return
	}
	type pending struct {
		id                                                  int64
		key, rule, severity, message, machine, created, evt string
		attempts                                            int
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.key, &p.rule, &p.severity, &p.message, &p.machine, &p.created, &p.evt, &p.attempts) == nil {
			batch = append(batch, p)
		}
	}
	rows.Close()
	host, _ := os.Hostname()
	for _, p := range batch {
		// Exponential spacing between attempts of the same alert.
		var lastTry sql.NullString
		_ = l.db.QueryRow(`SELECT value FROM security_meta WHERE key = ?`, "alert_try:"+p.key).Scan(&lastTry)
		if lastTry.Valid && p.attempts > 0 {
			if t, err := time.Parse(time.RFC3339Nano, lastTry.String); err == nil {
				wait := time.Duration(1<<min(p.attempts, 10)) * time.Second
				if time.Since(t) < wait {
					continue
				}
			}
		}
		body, _ := json.Marshal(map[string]any{
			"source": "apteva-server", "host": host, "alert_key": p.key, "rule": p.rule, "severity": p.severity,
			"message": p.message, "machine_id": p.machine, "created_at": p.created, "event": json.RawMessage(p.evt),
		})
		err := postSignedAlert(client, url, secret, p.key, body)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = l.db.Exec(`INSERT INTO security_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, "alert_try:"+p.key, now)
		if err != nil {
			_, _ = l.db.Exec(`UPDATE security_alerts SET delivery_attempts = delivery_attempts + 1, delivery_error = ? WHERE id = ?`, truncateAuditString(err.Error(), 300), p.id)
			continue
		}
		_, _ = l.db.Exec(`UPDATE security_alerts SET delivered_at = ?, delivery_attempts = delivery_attempts + 1, delivery_error = '' WHERE id = ?`, now, p.id)
		_, _ = l.db.Exec(`DELETE FROM security_meta WHERE key = ?`, "alert_try:"+p.key)
	}
}

func postSignedAlert(client *http.Client, url, secret, key string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Apteva-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("alert webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// startAlerting wires rules into the writer and starts delivery when a
// webhook is configured.
func (l *securityAuditLog) startAlerting(url, secret string) {
	if l == nil {
		return
	}
	l.migrateAlerts()
	l.mu.Lock()
	l.alertHook = l.raiseAlerts
	l.mu.Unlock()
	if strings.TrimSpace(url) != "" {
		l.wg.Add(1)
		go l.alertDeliverer(strings.TrimSpace(url), secret)
	}
}

func (s *Server) startSecurityAlerting() {
	s.securityAudit.startAlerting(os.Getenv("APTEVA_SECURITY_ALERT_WEBHOOK"), os.Getenv("APTEVA_SECURITY_ALERT_SECRET"))
}
