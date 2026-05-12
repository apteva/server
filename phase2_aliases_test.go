package main

// Phase 2 rename — wire-level aliases.
//
// Each test pins one alias contract that the handler code enforces:
//
//   * Callback path /apps/callback/agents/... routes to the same case
//     as /apps/callback/instances/... in handleAppCallback.
//   * Handlers that read ?instance_id= from the query string also
//     accept ?agent_id=.
//   * Telemetry ingest accepts either X-Agent-Secret or X-Instance-Secret.
//
// The /api/agents and /api/agents/<id> mux routes are validated by
// straightforward grep of main.go (both apiMux.HandleFunc lines added
// next to the existing /instances ones) — testing the mux assembly
// would require duplicating main.go's route-setup function.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhase2_CallbackAgentsAlias(t *testing.T) {
	// /apps/callback/agents/{id} routes to handleCallbackInstances
	// (same case as /apps/callback/instances/{id}).
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/apps/callback/agents/0", nil)
	req.Header.Set("X-Apteva-App-Install-ID", "1")
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if strings.Contains(rec.Body.String(), "unknown callback") {
		t.Errorf("/apps/callback/agents/0 not aliased: %s", rec.Body.String())
	}
}

func TestPhase2_QueryParam_AgentIDAcceptedByGrants(t *testing.T) {
	// The grants callback accepts either ?agent_id= or ?instance_id=.
	s := newTestServer(t)
	installID := seedInstall(t, s, "storage", "p1")

	for _, q := range []string{"?agent_id=7", "?instance_id=7"} {
		req := httptest.NewRequest(http.MethodGet, "/apps/callback/grants"+q, nil)
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		// Pass the agent_id required gate (handler may 4xx downstream
		// for other reasons but mustn't say "agent_id required").
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "agent_id required") {
			t.Errorf("query %s rejected as missing: %s", q, rec.Body.String())
		}
	}
}

func TestPhase2_JSONBody_AgentIDAcceptedByGrantCreate(t *testing.T) {
	// POST /grants accepts `agent_id` and the legacy `instance_id`.
	s := newTestServer(t)
	installID := seedPhase2GrantsInstall(t, s)

	for _, b := range []string{
		`{"agent_id":7,"effect":"allow","permission":"files.read","resource":"folder/x/"}`,
		`{"instance_id":7,"effect":"allow","permission":"files.read","resource":"folder/x/"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/apps/installs/"+itoa(installID)+"/grants",
			bytes.NewReader([]byte(b)))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleInstallGrants(rec, req)
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "agent_id required") {
			t.Errorf("body %s incorrectly rejected as missing agent_id: %s", b, rec.Body.String())
		}
	}
}

func TestPhase2_TelemetrySecret_BothHeadersAccepted(t *testing.T) {
	// Telemetry ingest reads X-Agent-Secret first, falls back to
	// X-Instance-Secret. Wrong / missing both fail with 401.
	s := newTestServer(t)
	s.instanceSecret = "test-secret-abc"

	body := `[{"id":"x","instance_id":1,"thread_id":"t","type":"llm.done","time":"2026-05-12T00:00:00Z"}]`

	check := func(headerName, headerVal string, want int, label string) {
		req := httptest.NewRequest(http.MethodPost, "/telemetry", strings.NewReader(body))
		if headerName != "" {
			req.Header.Set(headerName, headerVal)
		}
		rec := httptest.NewRecorder()
		s.handleIngestTelemetry(rec, req)
		if rec.Code != want {
			t.Errorf("%s: status=%d want=%d body=%s", label, rec.Code, want, rec.Body.String())
		}
	}
	check("X-Agent-Secret", "test-secret-abc", http.StatusOK, "X-Agent-Secret valid")
	check("X-Instance-Secret", "test-secret-abc", http.StatusOK, "X-Instance-Secret valid (legacy)")
	check("X-Agent-Secret", "wrong", http.StatusUnauthorized, "X-Agent-Secret wrong")
	check("", "", http.StatusUnauthorized, "no header")
}

// seedPhase2GrantsInstall is a thin wrapper around seedInstall that
// also stamps a minimal manifest declaring files.read so the
// grant-add handler's manifest-validation pass doesn't reject the
// body before reaching the agent_id check we want to test.
func seedPhase2GrantsInstall(t *testing.T, s *Server) int64 {
	t.Helper()
	manifest := `{"name":"storage","version":"1","provides":{"permissions":[{"name":"files.read","resource":"folder"}],"resources":[{"name":"folder","matcher":"glob"}]}}`
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		"phase2-grants-app", manifest,
	)
	if err != nil {
		t.Fatalf("seed apps: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings)
		 VALUES (?, ?, 'running', 1, '{}')`,
		appID, "p1",
	)
	if err != nil {
		t.Fatalf("seed install: %v", err)
	}
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	return id
}
