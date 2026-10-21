package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func consoleTargetFixture(t *testing.T, legacy bool) (*Server, *appRuntimeManager, runtimePlacement) {
	t.Helper()
	s, m, f, _, p := runtimeTestSetup(t)
	original := f.server.Config.Handler
	// This fixture advertises an arbitrary resource type; it deliberately has no
	// connection to Instances, a cloud vendor or the app being deployed.
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/targets") {
			if r.Header.Get("Authorization") == "" {
				t.Error("private inventory missing authentication")
			}
			writeJSON(w, runtimeTargetPage{Targets: []runtimeTarget{{Ref: p.Target, Name: "Test worker", Available: true, Status: "ready", Platform: "linux-amd64"}}})
			return
		}
		if r.URL.Path == "/private/inventory" {
			writeJSON(w, map[string]any{"resources": []any{map[string]any{"id": 9007199254740993, "label": "Large ID worker", "state": "ready"}, map[string]any{"id": 2, "label": "Offline worker", "state": "stopped"}}})
			return
		}
		original.ServeHTTP(w, r)
	})
	reg, _ := m.registration(p.Backend)
	reg.Caps.TargetListing = true
	if legacy {
		reg.TargetDiscovery = &runtimeTargetDiscovery{Path: "/private/inventory", Items: "resources", IDField: "id", NameField: "label", StatusField: "state", ReadyValue: "ready", ResourceType: "compute.machine"}
	}
	_, err := s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, m, p
}
func TestRuntimeConsoleTargetCatalog(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{true: "legacy inventory", false: "native catalog"}[legacy], func(t *testing.T) {
			s, m, p := consoleTargetFixture(t, legacy)
			page, e := m.listTargets(context.Background(), p.Backend, "", "worker", "")
			if e != nil {
				t.Fatal(e)
			}
			expected := 1
			if legacy {
				expected = 2
			}
			if len(page.Targets) != expected {
				t.Fatalf("targets: %+v", page)
			}
			if legacy && (page.Targets[0].Ref.ID != "9007199254740993" || page.Targets[1].Available) {
				t.Fatalf("lost precision or availability: %+v", page)
			}
			var cached string
			if e = s.store.db.QueryRow(`SELECT record_json FROM app_runtime_target_catalog WHERE resource_id=?`, page.Targets[0].Ref.ID).Scan(&cached); e != nil || !strings.Contains(cached, page.Targets[0].Name) {
				t.Fatal("target label not persisted", e)
			}
			s.store.db.Exec(`UPDATE app_installs SET project_id='private-project' WHERE id=?`, p.Backend.InstallID)
			if _, e = m.listTargets(context.Background(), p.Backend, "other-project", "", ""); e == nil {
				t.Fatal("cross-project inventory exposed")
			}
		})
	}
}
func TestRuntimeConsoleDiscoverySafety(t *testing.T) {
	for _, path := range []string{"https://example.com/inventory", "//example.com/x", "/a/../b", "/inventory?token=x", "/inventory%2fsecret"} {
		if validateTargetDiscovery(&runtimeTargetDiscovery{Path: path, IDField: "id", NameField: "name", ResourceType: "x"}) == nil {
			t.Fatal("unsafe inventory URL accepted", path)
		}
	}
	_, m, p := consoleTargetFixture(t, true)
	if _, e := m.listTargets(context.Background(), p.Backend, "", "", "nonsense"); e == nil {
		t.Fatal("invalid cursor accepted")
	}
}
func TestRuntimeConsoleDeployAndSummary(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	op, e := m.begin(id, "deploy", "console-click-once", p, nil)
	if e != nil {
		t.Fatal(e)
	}
	duplicate, e := m.begin(id, "deploy", "console-click-once", p, nil)
	if e != nil || duplicate.ID != op.ID {
		t.Fatal("duplicate deploy", e)
	}
	if op = waitRuntimeOperation(t, m, op); op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	d, e := m.active(id)
	if e != nil {
		t.Fatal(e)
	}
	summary := m.deploymentSummary(id)
	if summary.Type != "backend" || summary.State == "healthy" || summary.BackendName != "arbitrary-execution-provider" {
		t.Fatalf("unverified status: %+v", summary)
	}
	m.observeDeployment(d, d.Execution, nil)
	summary = m.deploymentSummary(id)
	if summary.State != "running" || summary.AdmissionState != "unknown" {
		t.Fatalf("readiness: %+v", summary)
	}
	s.store.db.Exec(`UPDATE app_runtime_observations SET checked_at=? WHERE deployment_id=?`, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano), d.ID)
	if m.deploymentSummary(id).State != "unknown" {
		t.Fatal("stale status reported current")
	}
	m.observeDeployment(d, runtimeExecution{}, context.DeadlineExceeded)
	if m.deploymentSummary(id).State != "unavailable" {
		t.Fatal("backend outage hidden")
	}
	operations := m.consoleOperations(id)
	if len(operations) != 1 || operations[0].ID != op.ID {
		t.Fatal("operation history missing")
	}
	raw, _ := json.Marshal(operations)
	token, _ := m.token(d)
	if strings.Contains(string(raw), token) {
		t.Fatal("credential exposed in operation history")
	}
}
func TestRuntimeConsoleNativeOwnership(t *testing.T) {
	_, m, p := consoleTargetFixture(t, false)
	reg, _ := m.registration(p.Backend)
	if reg.TargetDiscovery != nil {
		t.Fatal("unexpected adapter")
	}
	// A target record cannot impersonate a different installation.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign := p.Target
		foreign.InstallID++
		writeJSON(w, runtimeTargetPage{Targets: []runtimeTarget{{Ref: foreign, Name: "forged", Available: true}}})
	}))
	defer bad.Close()
	m.s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, bad.URL, p.Backend.InstallID)
	if _, e := m.listTargets(context.Background(), p.Backend, "", "", ""); e == nil {
		t.Fatal("foreign target accepted")
	}
}

func TestRuntimeConsolePreflightDoesNotDeploy(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	req := authedRequest(t, "POST", "/apps/deployment-preflight", "", map[string]any{"install_id": id, "placement": p})
	w := httptest.NewRecorder()
	s.handleRuntimeConsolePreflight(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"compatible":true`) {
		t.Fatalf("preflight: %d %s", w.Code, w.Body.String())
	}
	var count int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations`).Scan(&count)
	if count != 0 || len(f.events) != 0 {
		t.Fatal("preflight performed deployment work")
	}
	p.Target.InstallID++
	req = authedRequest(t, "POST", "/apps/deployment-preflight", "", map[string]any{"install_id": id, "placement": p})
	w = httptest.NewRecorder()
	s.handleRuntimeConsolePreflight(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"compatible":false`) {
		t.Fatalf("invalid target allowed: %d %s", w.Code, w.Body.String())
	}
	_ = m
	req = httptest.NewRequest("POST", "/apps/deployment-preflight", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	s.handleRuntimeConsolePreflight(w, req)
	if w.Code == 200 {
		t.Fatal("unauthenticated preflight")
	}
}
func TestRuntimeConsoleInstallReceiptRecovery(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	_, e := s.store.db.Exec(`INSERT INTO app_runtime_install_receipts VALUES(1,'request','fingerprint',?)`, id)
	if e != nil {
		t.Fatal(e)
	}
	request := authedRequest(t, "POST", "/apps/install", "", nil)
	w := httptest.NewRecorder()
	if !s.replayRuntimeConsoleInstall(w, request, 1, "request", "different", p) || w.Code != 409 {
		t.Fatal("changed retry accepted")
	}
	w = httptest.NewRecorder()
	if !s.replayRuntimeConsoleInstall(w, request, 1, "request", "fingerprint", p) || w.Code != 202 {
		t.Fatalf("interrupted handoff not resumed: %d %s", w.Code, w.Body.String())
	}
	var first struct {
		InstallID int64            `json:"install_id"`
		Operation runtimeOperation `json:"operation"`
	}
	json.Unmarshal(w.Body.Bytes(), &first)
	if first.InstallID != id || waitRuntimeOperation(t, m, first.Operation).Status != "succeeded" {
		t.Fatal("wrong recovered installation")
	}
	w = httptest.NewRecorder()
	s.replayRuntimeConsoleInstall(w, request, 1, "request", "fingerprint", p)
	var second struct {
		Operation runtimeOperation `json:"operation"`
	}
	json.Unmarshal(w.Body.Bytes(), &second)
	if second.Operation.ID != first.Operation.ID {
		t.Fatal("retried install queued a duplicate execution")
	}
}
