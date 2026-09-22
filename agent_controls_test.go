package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentControlsStayStructuredAndPrivate(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "controlled", compileAgentDirective("Operator mission", "learn", 91), "cautious", `{}`, "", 42)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Directive != "Operator mission" || strings.Contains(agent.Directive, behaviorStart) {
		t.Fatalf("storage contains compiled controls: %q", agent.Directive)
	}

	rows, err := s.store.db.Query(`SELECT control_key,value_json FROM agent_control_assignments WHERE agent_id=? ORDER BY control_key`, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	assignments := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		assignments[key] = value
	}
	rows.Close()
	if assignments[agentControlApprovalMode] != `"cautious"` || assignments[agentControlInitiative] != "42" {
		t.Fatalf("unexpected control assignments: %#v", assignments)
	}

	if w := behaviorRequest(t, s, agent.ID, map[string]any{"mode": "learn", "proactivity": 73}); w.Code != http.StatusOK {
		t.Fatalf("control update status=%d body=%s", w.Code, w.Body.String())
	}
	agent, _ = s.store.GetAgentByID(agent.ID)
	if agent.Directive != "Operator mission" || agent.Mode != "learn" || agent.Proactivity != 73 {
		t.Fatalf("structured update changed authored directive: %+v", agent)
	}

	var modeJSON, initiativeJSON string
	if err := s.store.db.QueryRow(`SELECT value_json FROM agent_control_assignments WHERE agent_id=? AND control_key=?`, agent.ID, agentControlApprovalMode).Scan(&modeJSON); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow(`SELECT value_json FROM agent_control_assignments WHERE agent_id=? AND control_key=?`, agent.ID, agentControlInitiative).Scan(&initiativeJSON); err != nil {
		t.Fatal(err)
	}
	if modeJSON != `"learn"` || initiativeJSON != "73" {
		t.Fatalf("control projections not synchronized: mode=%s initiative=%s", modeJSON, initiativeJSON)
	}

	w := httptest.NewRecorder()
	s.handleUpdateConfig(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/instances/%d/config", agent.ID), nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), behaviorStart) || strings.Contains(w.Body.String(), "AGENT-WIDE PROACTIVITY") {
		t.Fatalf("private policy leaked through config API: status=%d body=%s", w.Code, w.Body.String())
	}
	var public map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	controls := public["controls"].(map[string]any)
	if controls[agentControlApprovalMode].(map[string]any)["value"] != "learn" || controls[agentControlInitiative].(map[string]any)["value"] != float64(73) {
		t.Fatalf("public structured controls missing: %#v", controls)
	}

	encoded, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), behaviorStart) || !strings.Contains(string(encoded), `"controls"`) {
		t.Fatalf("agent API representation leaked compiled policy: %s", encoded)
	}
	internal := readBehaviorDisk(t, s, agent.ID)["directive"].(string)
	if !strings.Contains(internal, "instructions (learn)") || !strings.Contains(internal, "PROACTIVITY: 73/100") {
		t.Fatalf("Core-facing config did not receive compiled controls: %s", internal)
	}
}

func TestAgentControlSanitizesWorkerAndTelemetryResponses(t *testing.T) {
	s := newTestServer(t)
	agent := behaviorAgent(t, s, "cautious")
	compiled := compileAgentDirective("Worker mission", agent.Mode, agent.Proactivity)
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["directive"] = compileAgentDirective(agent.Directive, agent.Mode, agent.Proactivity)
		cfg["threads"] = []any{map[string]any{"id": "worker", "directive": compiled}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleProxy(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/instances/%d/threads", agent.ID), nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), behaviorStart) || !strings.Contains(w.Body.String(), "Worker mission") {
		t.Fatalf("worker API leaked compiled controls: status=%d body=%s", w.Code, w.Body.String())
	}

	event := TelemetryEvent{Data: json.RawMessage(`{"directive":` + fmt.Sprintf("%q", compiled) + `,"nested":{"directive":` + fmt.Sprintf("%q", compiled) + `}}`)}
	public := publicTelemetryEvent(event)
	if strings.Contains(string(public.Data), behaviorStart) || strings.Count(string(public.Data), "Worker mission") != 2 {
		t.Fatalf("telemetry leaked compiled controls: %s", public.Data)
	}
	sse := sanitizeDirectiveSSELine([]byte("data: " + string(event.Data) + "\n"))
	if strings.Contains(string(sse), behaviorStart) || strings.Count(string(sse), "Worker mission") != 2 {
		t.Fatalf("live event leaked compiled controls: %s", sse)
	}
}

func TestPlatformHelperRefreshUsesGenericControlCompiler(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	helper, err := s.store.GetOrCreatePlatformHelper(1, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	helper.Mode = "cautious"
	helper.Proactivity = 64
	if err := s.store.UpdateAgent(helper); err != nil {
		t.Fatal(err)
	}
	core := attachBehaviorTestCore(t, s, helper)
	if err := s.refreshPlatformHelperDirective(helper); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.store.GetAgentByID(helper.ID)
	if strings.Contains(stored.Directive, behaviorStart) || stored.Directive != platformHelperSystemPrompt {
		t.Fatalf("Helper database directive is not clean")
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	effective, _ := core.cfg["directive"].(string)
	if !strings.Contains(effective, "instructions (cautious)") || !strings.Contains(effective, "PROACTIVITY: 64/100") {
		t.Fatalf("Helper runtime policy missing: %s", effective)
	}
	if servers, _ := core.cfg["mcp_servers"].([]any); len(servers) != 1 {
		t.Fatalf("Helper refresh lost MCP configuration: %#v", core.cfg["mcp_servers"])
	}
}
