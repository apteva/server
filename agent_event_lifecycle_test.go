package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func testHTTPServerPort(t *testing.T, serverURL string) int {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func seedAgentEventTestTarget(t *testing.T, s *Server) (*Agent, int64) {
	t.Helper()
	user, err := s.store.CreateUser("agent-events@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "Tracked", "work", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "tasks", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermInstancesWrite}}}
	installID := seedInstallWithBindings(t, s, "tasks", manifest, nil)
	return agent, installID
}

func attachTestCore(t *testing.T, s *Server, agent *Agent, core *httptest.Server) {
	t.Helper()
	agent.Status = "running"
	if _, err := s.store.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{
		port: testHTTPServerPort(t, core.URL), coreAPIKey: "core-key", reattached: true,
	}
	s.agents.mu.Unlock()
}

func TestSendTrackedAgentEventIsDurablyIdempotent(t *testing.T) {
	s := newTestServer(t)
	agent, installID := seedAgentEventTestTarget(t, s)
	coreEventID := agentEventCoreID(installID, "occurrence:123")
	var posts atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		if r.Header.Get("Authorization") != "Bearer core-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["event_id"] != coreEventID || body["track_lifecycle"] != true || body["thread_id"] != "main" {
			t.Errorf("core body = %#v", body)
		}
		writeJSON(w, map[string]any{
			"status": "queued", "thread_id": "main",
			"events": map[string]any{
				"accepted": []string{coreEventID}, "duplicates": []string{},
				"executions": map[string]string{coreEventID: "exe_123"},
			},
		})
	}))
	defer core.Close()
	attachTestCore(t, s, agent, core)

	first, err := s.sendTrackedAgentEvent(t.Context(), agent, installID, "occurrence:123", "main", "do work")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.Duplicate || first.ExecutionID != "exe_123" {
		t.Fatalf("first receipt = %#v", first)
	}
	duplicate, err := s.sendTrackedAgentEvent(t.Context(), agent, installID, "occurrence:123", "main", "do work")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Accepted || !duplicate.Duplicate || duplicate.ExecutionID != "exe_123" {
		t.Fatalf("duplicate receipt = %#v", duplicate)
	}
	if posts.Load() != 1 {
		t.Fatalf("core posts = %d, want 1", posts.Load())
	}
	if _, err := s.sendTrackedAgentEvent(t.Context(), agent, installID, "occurrence:123", "main", "different work"); !errors.Is(err, errAgentEventConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestTrackedAgentEventCallbackAuthorizesBeforeCoreDelivery(t *testing.T) {
	s := newTestServer(t)
	agent, installID := seedAgentEventTestTarget(t, s)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	coreEventID := agentEventCoreID(installID, "occurrence:callback")
	var posts atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		writeJSON(w, map[string]any{
			"events": map[string]any{
				"accepted": []string{coreEventID}, "duplicates": []string{},
				"executions": map[string]string{coreEventID: "exe_callback"},
			},
		})
	}))
	defer core.Close()
	attachTestCore(t, s, agent, core)

	call := func(sourceEventID string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"message": "do work", "source_event_id": sourceEventID, "track_lifecycle": true,
		})
		req := httptest.NewRequest(http.MethodPost, "/apps/callback/agents/"+strconv.FormatInt(agent.ID, 10)+"/event", strings.NewReader(string(body)))
		req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		return rec
	}

	if rec := call("occurrence:callback"); rec.Code != http.StatusForbidden {
		t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
	}
	if posts.Load() != 0 {
		t.Fatalf("unauthorized request reached Core %d time(s)", posts.Load())
	}
	permissions, _ := json.Marshal([]sdk.Permission{sdk.PermInstancesWrite})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, permissions, installID); err != nil {
		t.Fatal(err)
	}
	if rec := call("bad\nevent"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid event status=%d body=%s", rec.Code, rec.Body.String())
	}
	if posts.Load() != 0 {
		t.Fatalf("invalid request reached Core %d time(s)", posts.Load())
	}
	rec := call("occurrence:callback")
	if rec.Code != http.StatusOK {
		t.Fatalf("tracked callback status=%d body=%s", rec.Code, rec.Body.String())
	}
	var receipt sdk.AgentEventReceipt
	if err := json.NewDecoder(rec.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ExecutionID != "exe_callback" || !receipt.Accepted || receipt.Duplicate {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestLifecycleRelayUsesTelemetryAndDeliversThroughEvents(t *testing.T) {
	s := newTestServer(t)
	agent, installID := seedAgentEventTestTarget(t, s)
	coreEventID := agentEventCoreID(installID, "occurrence:456")
	hash, err := agentEventPayloadHash("main", "do tracked work")
	if err != nil {
		t.Fatal(err)
	}
	execution, _, err := s.store.prepareAgentEventExecution(agent.ID, agent.ProjectID, "tasks", installID, "occurrence:456", coreEventID, "main", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.completeAgentEventExecution(execution.ID, "exe_456"); err != nil {
		t.Fatal(err)
	}

	transitionTime := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	transition := coreEventLifecycleTransition{
		ID: "exe_456:3", Type: "event.settled", EventID: coreEventID,
		ExecutionID: "exe_456", ThreadID: "main", Timestamp: transitionTime,
		Reason: "event_wait", Sequence: 3,
	}
	var acknowledged atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/event-lifecycle":
			writeJSON(w, map[string]any{"transitions": []coreEventLifecycleTransition{transition}})
		case r.Method == http.MethodPost && r.URL.Path == "/event-lifecycle":
			var body struct {
				AckIDs []string `json:"ack_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.AckIDs) != 1 || body.AckIDs[0] != transition.ID {
				t.Errorf("ack ids = %#v", body.AckIDs)
			}
			acknowledged.Add(1)
			writeJSON(w, map[string]any{"status": "acknowledged"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	attachTestCore(t, s, agent, core)

	var deliveryRequests atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		var event sdk.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.DeliveryID != transition.ID || event.Event != sdk.AgentEventLifecycleEvent || event.InstanceID != agent.ID {
			t.Errorf("delivered event = %#v", event)
		}
		if event.Data["source_event_id"] != "occurrence:456" || event.Data["type"] != "event.settled" {
			t.Errorf("delivered data = %#v", event.Data)
		}
		requestNumber := deliveryRequests.Add(1)
		if requestNumber == 1 {
			http.Error(w, "database unavailable", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: installID, AppName: "tasks", ProjectID: agent.ProjectID,
		SidecarURL: sidecar.URL, Token: "app-token",
	})

	relay := NewAgentEventLifecycleService(s)
	if err := relay.relayAgent(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	if acknowledged.Load() != 1 {
		t.Fatalf("acknowledgements = %d", acknowledged.Load())
	}
	telemetryID := agentLifecycleTelemetryID(agent.ID, transition.ID)
	var telemetryCount, deliveryCount int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE id=?`, telemetryID).Scan(&telemetryCount); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_event_deliveries WHERE transition_id=?`, transition.ID).Scan(&deliveryCount); err != nil {
		t.Fatal(err)
	}
	if telemetryCount != 1 || deliveryCount != 1 {
		t.Fatalf("telemetry=%d delivery=%d", telemetryCount, deliveryCount)
	}

	if err := relay.deliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if deliveryRequests.Load() != 1 {
		t.Fatalf("sidecar delivery requests = %d", deliveryRequests.Load())
	}
	var attempts int
	if err := s.store.db.QueryRow(`SELECT attempts FROM agent_event_deliveries WHERE transition_id=?`, transition.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("delivery attempts = %d, want 1", attempts)
	}
	if _, err := s.store.db.Exec(`UPDATE agent_event_deliveries SET next_attempt_at=? WHERE transition_id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), transition.ID); err != nil {
		t.Fatal(err)
	}
	if err := relay.deliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if deliveryRequests.Load() != 2 {
		t.Fatalf("sidecar delivery requests after retry = %d", deliveryRequests.Load())
	}
	var deliveredAt any
	if err := s.store.db.QueryRow(`SELECT delivered_at FROM agent_event_deliveries WHERE transition_id=?`, transition.ID).Scan(&deliveredAt); err != nil {
		t.Fatal(err)
	}
	if deliveredAt == nil {
		t.Fatal("delivery was not marked successful")
	}

	// A repeated Core transition overwrites/enriches the same telemetry row and
	// cannot enqueue or redeliver a second copy.
	if err := relay.relayAgent(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	if err := relay.deliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if deliveryRequests.Load() != 2 {
		t.Fatalf("duplicate transition redelivered: %d", deliveryRequests.Load())
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE id=?`, telemetryID).Scan(&telemetryCount); err != nil {
		t.Fatal(err)
	}
	if telemetryCount != 1 {
		t.Fatalf("telemetry rows = %d", telemetryCount)
	}
}

func TestNormalizeAgentLifecycleTelemetryID(t *testing.T) {
	events := []TelemetryEvent{{
		ID: "ordinary-core-id", AgentID: 12, ThreadID: "main", Type: "event.active",
		Data: json.RawMessage(`{"id":"exe_12:2","execution_id":"exe_12"}`),
	}}
	normalizeAgentLifecycleTelemetryIDs(events)
	if events[0].ID != "agent:12:exe_12:2" {
		t.Fatalf("telemetry id = %q", events[0].ID)
	}
}

func TestUnownedTrackedTransitionUsesTelemetryWithoutAppDelivery(t *testing.T) {
	s := newTestServer(t)
	agent, _ := seedAgentEventTestTarget(t, s)
	transition := coreEventLifecycleTransition{
		ID: "exe_admin:1", Type: "event.claimed", EventID: "mcp:1:request-1",
		ExecutionID: "exe_admin", ThreadID: "main", Timestamp: time.Now().UTC(), Sequence: 1,
	}
	persisted, err := s.store.persistAgentLifecycleTransition(agent.ID, transition)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("unowned transition was not durably persisted for acknowledgement")
	}
	var telemetryCount, deliveryCount int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE id=?`, agentLifecycleTelemetryID(agent.ID, transition.ID)).Scan(&telemetryCount); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_event_deliveries WHERE transition_id=?`, transition.ID).Scan(&deliveryCount); err != nil {
		t.Fatal(err)
	}
	if telemetryCount != 1 || deliveryCount != 0 {
		t.Fatalf("telemetry=%d delivery=%d", telemetryCount, deliveryCount)
	}
}

func TestPendingLifecycleDeliveriesPreserveExecutionOrder(t *testing.T) {
	s := newTestServer(t)
	for sequence := 1; sequence <= 2; sequence++ {
		transitionID := fmt.Sprintf("exe_order:%d", sequence)
		if _, err := s.store.db.Exec(`INSERT INTO agent_event_deliveries
			(transition_id, telemetry_id, execution_id, sequence, source_install_id, payload_json)
			VALUES (?, ?, 'exe_order', ?, 1, '{}')`, transitionID, "telemetry:"+transitionID, sequence); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.store.pendingAgentEventDeliveries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].TransitionID != "exe_order:1" {
		t.Fatalf("initial pending deliveries = %#v", pending)
	}
	if err := s.store.markAgentEventDeliverySucceeded("exe_order:1"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.store.pendingAgentEventDeliveries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].TransitionID != "exe_order:2" {
		t.Fatalf("pending deliveries after sequence 1 = %#v", pending)
	}
}
