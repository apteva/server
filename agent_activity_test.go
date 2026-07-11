package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildAgentActivityBuildsThoughtsAndToolActions(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "Media Agent", "process media", "autonomous", "{}", "proj-a")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	base := time.Now().Add(-5 * time.Minute).UTC()
	events := []TelemetryEvent{
		activityTestEvent("e1", agent.ID, "main", "llm.start", base, `{"iteration":1,"model":"gpt-test"}`),
		activityTestEvent("e2", agent.ID, "main", "llm.thinking", base.Add(time.Second), `{"iteration":1,"text":"I should inspect "}`),
		activityTestEvent("e3", agent.ID, "main", "llm.thinking", base.Add(2*time.Second), `{"iteration":1,"text":"the upload."}`),
		activityTestEvent("e4", agent.ID, "main", "llm.chunk", base.Add(3*time.Second), `{"iteration":1,"chunk":"Checking media now."}`),
		activityTestEvent("e5", agent.ID, "main", "llm.done", base.Add(4*time.Second), `{"iteration":1,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"duration_ms":1234}`),
		activityTestEvent("e6", agent.ID, "main", "tool.call", base.Add(5*time.Second), `{"id":"call-1","name":"bunny_stream_upload_video","args":{"file":"demo.mp4"},"reason":"Uploading media"}`),
		activityTestEvent("e7", agent.ID, "main", "tool.result", base.Add(6*time.Second), `{"id":"call-1","name":"bunny_stream_upload_video","result":{"video_id":"abc"},"duration_ms":42}`),
		activityTestEvent("e8", agent.ID, "main", "tool.call", base.Add(7*time.Second), `{"id":"reply-1","name":"channels_respond","args":{"text":"Done"}}`),
	}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatalf("InsertTelemetry: %v", err)
	}

	got, err := BuildAgentActivity(s.store, 1, AgentActivityOptions{
		ProjectID:       "proj-a",
		AgentID:         agent.ID,
		Period:          "1h",
		Limit:           20,
		IncludePayloads: true,
	})
	if err != nil {
		t.Fatalf("BuildAgentActivity: %v", err)
	}

	if got.Counts["total"] != 2 {
		t.Fatalf("expected thought + tool only, got counts=%v actions=%#v", got.Counts, got.Actions)
	}
	var thought, tool *AgentActivityAction
	for i := range got.Actions {
		switch got.Actions[i].Kind {
		case "thought":
			thought = &got.Actions[i]
		case "tool":
			tool = &got.Actions[i]
		}
		if got.Actions[i].Title == "Replying in chat" || got.Actions[i].Detail == "channels_respond" {
			t.Fatalf("channels_respond should be omitted, got %#v", got.Actions[i])
		}
	}
	if thought == nil {
		t.Fatalf("missing built thought action: %#v", got.Actions)
	}
	if thought.Status != "success" || thought.Thinking != "I should inspect the upload." || thought.Output != "Checking media now." {
		t.Fatalf("unexpected thought action: %#v", thought)
	}
	if tool == nil {
		t.Fatalf("missing tool action: %#v", got.Actions)
	}
	if tool.Status != "success" || tool.Title != "Video completed" || tool.DurationMS != 42 {
		t.Fatalf("unexpected tool action: %#v", tool)
	}
	if tool.Args == "" || tool.Result == "" {
		t.Fatalf("expected payloads on tool action: %#v", tool)
	}
}

func TestGatewayAgentListActivityTool(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "Worker", "work", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.store.InsertTelemetry([]TelemetryEvent{
		activityTestEvent("tool-1", agent.ID, "main", "tool.call", time.Now().Add(-time.Minute), `{"id":"c1","name":"crm_search","args":{"q":"acme"}}`),
	}); err != nil {
		t.Fatalf("InsertTelemetry: %v", err)
	}
	result, err := handleGatewayAgentTool("agent_list_activity", map[string]any{
		"agent_id":         agent.ID,
		"kind":             "tool",
		"period":           "1h",
		"include_payloads": "true",
	}, "", gatewayAPIClient{userID: 1}, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agent_list_activity returned error: %v", err)
	}
	resp, ok := result.(AgentActivityResponse)
	if !ok {
		t.Fatalf("expected AgentActivityResponse, got %T", result)
	}
	if len(resp.Actions) != 1 || resp.Actions[0].Detail != "crm_search" || resp.Actions[0].Args == "" {
		t.Fatalf("unexpected activity response: %#v", resp)
	}
}

func activityTestEvent(id string, agentID int64, threadID, typ string, ts time.Time, data string) TelemetryEvent {
	if !json.Valid([]byte(data)) {
		panic("invalid test JSON")
	}
	return TelemetryEvent{
		ID:       id,
		AgentID:  agentID,
		ThreadID: threadID,
		Type:     typ,
		Time:     ts,
		Data:     json.RawMessage(data),
	}
}
