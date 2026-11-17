package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apteva/server/internal/sshwatch"
)

// fakeHostCollector serves pages from an in-memory host event store, the
// way apteva-host's /security/events does.
type fakeHostCollector struct {
	mu      sync.Mutex
	events  []sshwatch.Event
	webhook bool
	calls   int
}

func (f *fakeHostCollector) add(ev sshwatch.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev.Seq = int64(len(f.events) + 1)
	if ev.ID == "" {
		ev.ID = "evt-" + itoa(ev.Seq)
	}
	if ev.MachineID == "" {
		ev.MachineID = "machine-a"
	}
	if ev.ObservedAt.IsZero() {
		ev.ObservedAt = time.Now().UTC()
	}
	f.events = append(f.events, ev)
}

func (f *fakeHostCollector) fetch(_ context.Context, after int64) (sshwatch.EventsPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	page := sshwatch.EventsPage{NextAfter: after}
	for _, ev := range f.events {
		if ev.Seq > after && len(page.Events) < 2 { // tiny pages exercise paging
			page.Events = append(page.Events, ev)
			page.NextAfter = ev.Seq
		}
	}
	return page, nil
}

func (f *fakeHostCollector) health(context.Context) (sshwatch.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sshwatch.Health{Role: "collector", LastSeq: int64(len(f.events)), WebhookConfigured: f.webhook}, nil
}

func sshLogin(user, ip, fp string, alerts ...sshwatch.Alert) sshwatch.Event {
	return sshwatch.Event{Kind: sshwatch.KindSSHAuth, Result: sshwatch.ResultAccepted, User: user, SourceIP: ip, SourcePort: 52144,
		Method: "publickey", KeyType: "ED25519", Fingerprint: fp, Hostname: "prod-1", Alerts: alerts}
}

func TestHostSSHEventsReachAuditTrailOnceAndAlert(t *testing.T) {
	dir := t.TempDir()
	audit, err := openSecurityAuditLog(dir, securityAuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	audit.startAlerting("", "")
	s := &Server{securityAudit: audit}
	hostC := &fakeHostCollector{}
	hostC.add(sshLogin("deploy", "198.51.100.5", "SHA256:knownkey"))
	// The known key, from a source outside the approved list.
	hostC.add(sshLogin("root", "203.0.113.200", "SHA256:knownkey",
		sshwatch.Alert{Rule: sshwatch.RuleUnapprovedSource, Severity: sshwatch.SeverityHigh, Message: "SSH login from unapproved source 203.0.113.200"},
		sshwatch.Alert{Rule: sshwatch.RuleRootLogin, Severity: sshwatch.SeverityHigh, Message: "root login"}))
	hostC.add(sshwatch.Event{Kind: sshwatch.KindAuthorizedKeysChange, Result: sshwatch.ResultChanged, User: "root", Path: "/root/.ssh/authorized_keys",
		KeysAdded: []sshwatch.AuthorizedKey{{Fingerprint: "SHA256:newkey", Type: "ssh-ed25519", Comment: "laptop"}}, KeyCount: 3,
		Alerts: []sshwatch.Alert{{Rule: sshwatch.RuleAuthorizedKeysChange, Severity: sshwatch.SeverityMedium, Message: "authorized_keys changed"}}})
	ctx := context.Background()
	n, err := s.ingestHostSecurity(ctx, hostC.fetch, hostC.health)
	if err != nil || n != 3 {
		t.Fatalf("first sync: n=%d err=%v", n, err)
	}
	logins, _ := audit.Query(SecurityEventQuery{Category: "ssh", Action: "ssh.auth"})
	if len(logins) != 2 {
		t.Fatalf("ssh logins in trail: %d", len(logins))
	}
	var alerted SecurityEvent
	for _, ev := range logins {
		if ev.SourceIP == "203.0.113.200" {
			alerted = ev
		}
	}
	if alerted.PrincipalID != "root" || alerted.CredentialID != "ssh_key:SHA256:knownkey" || alerted.MachineID != "machine-a" ||
		alerted.Severity != securitySeverityCritical || alerted.TargetID != "prod-1" {
		t.Fatalf("login mapped incompletely: %+v", alerted)
	}
	alerts, _ := audit.Alerts(false, 50)
	rules := map[string]SecurityAlert{}
	for _, a := range alerts {
		rules[a.Rule] = a
	}
	if rules["ssh_unapproved_source"].Severity != securitySeverityCritical || rules["ssh_root_login"].Rule == "" || rules["ssh_authorized_keys_change"].Severity != securitySeverityWarning {
		t.Fatalf("host verdicts not raised as alerts: %+v", alerts)
	}
	if rules["ssh_unapproved_source"].DeliveredAt != "" {
		t.Fatal("without a host webhook the server must deliver the alert itself")
	}

	// Collector restart: the host store was replaced and seq restarts, so
	// the server re-reads from 0. Nothing is duplicated.
	before := len(alerts)
	if err := audit.setMetaInt(hostSecurityCursorKey, 999); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ingestHostSecurity(ctx, hostC.fetch, hostC.health); err != nil || n != 0 {
		t.Fatalf("replay inserted %d rows (err %v)", n, err)
	}
	if again, _ := audit.Alerts(false, 50); len(again) != before {
		t.Fatalf("replay raised %d new alerts", len(again)-before)
	}

	// Server down: the host keeps collecting. After a restart only the
	// backlog is copied.
	audit.Close()
	hostC.webhook = true
	hostC.add(sshLogin("deploy", "198.51.100.5", "SHA256:knownkey"))
	hostC.add(sshLogin("admin", "192.0.2.66", "SHA256:other",
		sshwatch.Alert{Rule: sshwatch.RuleUnknownKey, Severity: sshwatch.SeverityHigh, Message: "unknown key"}))
	hostC.add(sshwatch.Event{Kind: sshwatch.KindCollectorGap, GapFrom: time.Now().Add(-time.Minute), GapTo: time.Now(),
		Alerts: []sshwatch.Alert{{Rule: sshwatch.RuleCollectorGap, Severity: sshwatch.SeverityMedium, Message: "collector gap"}}})
	audit2, err := openSecurityAuditLog(dir, securityAuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer audit2.Close()
	audit2.startAlerting("", "")
	s.securityAudit = audit2
	if n, err := s.ingestHostSecurity(ctx, hostC.fetch, hostC.health); err != nil || n != 3 {
		t.Fatalf("backlog after restart: n=%d err=%v", n, err)
	}
	all, _ := audit2.Query(SecurityEventQuery{Category: "ssh", Limit: 100})
	if len(all) != 6 {
		t.Fatalf("ssh events after restart: %d, want 6", len(all))
	}
	gaps, _ := audit2.Query(SecurityEventQuery{Action: "ssh.collector_gap"})
	if len(gaps) != 1 || gaps[0].Result != securityResultError || gaps[0].Details["gap_from"] == nil {
		t.Fatalf("collector gap must be visible as missing evidence: %+v", gaps)
	}
	for _, a := range mustAlerts(t, audit2) {
		if a.Rule == "ssh_unknown_key" && a.DeliveredAt == "" {
			t.Fatal("alerts the host already delivered must not be sent again by the server")
		}
	}
	if status, err := audit2.VerifyChain(); err != nil || !status.OK {
		t.Fatalf("chain after host ingest: %+v %v", status, err)
	}
}

func mustAlerts(t *testing.T, l *securityAuditLog) []SecurityAlert {
	t.Helper()
	alerts, err := l.Alerts(false, 100)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestHostSecurityStatusWithoutHost(t *testing.T) {
	t.Setenv("APTEVA_HOST_SOCKET", "")
	s := &Server{}
	st := s.securityHostStatus(context.Background())
	if st["available"] != false || !strings.Contains(st["reason"].(string), "not monitored") {
		t.Fatalf("missing collector must be stated plainly: %v", st)
	}
}

// A fake apteva-host control socket, for the policy endpoint.
func fakeHostSocket(t *testing.T, handler http.Handler) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ahs") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("APTEVA_HOST_SOCKET", socket)
}

func TestHostSSHPolicyChangeNeedsStepUpAndIsAuditedWithoutSecret(t *testing.T) {
	h := newSecurityHarness(t)
	var mu sync.Mutex
	policy := sshwatch.Policy{ApprovedSources: []string{"198.51.100.0/24"}, FailureBurstThreshold: 10, FailureBurstWindowSeconds: 300}
	puts := 0
	fakeHostSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/security/policy" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(policy)
		case r.URL.Path == "/security/policy" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &policy)
			puts++
			out := policy
			out.AlertWebhookSecretSet, out.AlertWebhookSecret = out.AlertWebhookSecret != "", ""
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/security/health":
			_ = json.NewEncoder(w).Encode(sshwatch.Health{Role: "collector", WebhookConfigured: true})
		default:
			http.NotFound(w, r)
		}
	}))
	admin := h.login(t, "admin@example.test", "198.51.100.60:1")
	newPolicy := map[string]any{"approved_sources": []string{"198.51.100.0/24", "192.0.2.10"}, "failure_burst_threshold": 5,
		"failure_burst_window_seconds": 120, "alert_webhook_url": "https://alerts.example.test/hook", "alert_webhook_secret": "whsec-very-secret"}

	_, _ = h.s.store.db.Exec(`UPDATE sessions SET reauth_at = ? WHERE token = ?`, time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05"), sessionKey(admin))
	if w := h.do(t, harnessReq{method: "PUT", path: "/security/host/policy", cookie: admin, body: newPolicy}); w.Code != http.StatusForbidden {
		t.Fatalf("stale session changed SSH policy: %d %s", w.Code, w.Body.String())
	}
	if puts != 0 {
		t.Fatal("policy reached the host without step-up")
	}
	_ = h.s.store.MarkSessionReauthenticated(admin, time.Now())
	w := h.do(t, harnessReq{method: "PUT", path: "/security/host/policy", cookie: admin, body: newPolicy})
	if w.Code != 200 || strings.Contains(w.Body.String(), "whsec-very-secret") {
		t.Fatalf("policy update: %d %s", w.Code, w.Body.String())
	}
	ev := h.events(t, SecurityEventQuery{Action: "ssh.policy_update", Result: securityResultSuccess})
	if len(ev) != 1 || ev[0].PrincipalID != itoa(h.adminID) {
		t.Fatalf("policy change not attributed: %+v", ev)
	}
	raw, _ := json.Marshal(ev[0])
	if strings.Contains(string(raw), "whsec-very-secret") || strings.Contains(string(raw), "alerts.example.test/hook") {
		t.Fatalf("audit record leaks webhook secret or URL: %s", raw)
	}
	if after, _ := ev[0].Details["after"].(map[string]any); after["failure_burst_threshold"] != float64(5) {
		t.Fatalf("audit record lacks the change itself: %v", ev[0].Details)
	}
	status := h.do(t, harnessReq{method: "GET", path: "/security/host", cookie: admin})
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"available":true`) {
		t.Fatalf("host status: %d %s", status.Code, status.Body.String())
	}
}
