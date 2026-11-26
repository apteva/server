package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestGatewayProactivityArg(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		want int
	}{{float64(60), 60}, {"60", 60}, {float64(0), 0}, {100, 100}, {int64(25), 25}} {
		got, ok, err := gatewayProactivityArg(map[string]any{"proactivity": tc.raw})
		if err != nil || !ok || got != tc.want {
			t.Fatalf("%#v: got %d %v %v, want %d", tc.raw, got, ok, err, tc.want)
		}
	}
	if _, ok, err := gatewayProactivityArg(map[string]any{}); ok || err != nil {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
	for _, raw := range []any{float64(-1), float64(101), 60.5, "high", true, map[string]any{}} {
		if _, _, err := gatewayProactivityArg(map[string]any{"proactivity": raw}); err == nil || !strings.Contains(err.Error(), "from 0 to 100") {
			t.Fatalf("%#v: err=%v", raw, err)
		}
	}
}

// The helper's tools set proactivity through the same agent routes as the
// dashboard, and the receipt reports the value the agent now has.
func TestGatewayAgentToolsSetProactivityAgainstServerRoutes(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	ts := newGatewayAgentAPITestServer(s)
	t.Cleanup(ts.Close)
	client := gatewayAPIClient{baseURL: ts.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}

	created, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name": "Planner", "directive": "Plan the week.", "mode": "cautious", "proactivity": "40", "start": false, "global": true,
	}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatal(err)
	}
	row := created.(map[string]any)
	if row["proactivity"] != float64(40) {
		t.Fatalf("create receipt: %v", row)
	}
	id := int64(row["id"].(float64))
	agent, err := s.store.GetAgentByID(id)
	if err != nil || agent.Proactivity != 40 {
		t.Fatalf("created agent: %+v err=%v", agent, err)
	}

	out, err := handleGatewayAgentTool("agents_update", map[string]any{"id": float64(id), "proactivity": float64(60)}, "", client, s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt := out.(map[string]any)
	if receipt["proactivity"] != 60 || fmt.Sprint(receipt["changed"]) != "[proactivity]" {
		t.Fatalf("update receipt: %v", receipt)
	}
	agent, err = s.store.GetAgentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Proactivity != 60 || agent.Mode != "cautious" {
		t.Fatalf("saved agent: proactivity=%d mode=%q", agent.Proactivity, agent.Mode)
	}
	if strings.Contains(agent.Config, "proactivity") {
		t.Fatalf("proactivity leaked into config: %s", agent.Config)
	}
}

func TestGatewayAgentToolsRejectBehaviorInsideConfig(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "Manager", "Run things.", "autonomous", `{}`, "p1")
	if err != nil {
		t.Fatal(err)
	}

	// Rejected before anything is written, the name included.
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"name": "Renamed", "config": map[string]any{"proactivity": 60}}, "config.proactivity is not accepted: set it with the top-level proactivity field"},
		{map[string]any{"name": "Renamed", "config": `{"mode":"cautious"}`}, "config.mode is not accepted"},
		{map[string]any{"name": "Renamed", "proactivity": 101}, "proactivity must be an integer from 0 to 100"},
		{map[string]any{"name": "Renamed", "proactivity": "more"}, "proactivity must be an integer from 0 to 100"},
	} {
		fake, client := newFakeModelsAPI(t, "p1")
		tc.args["id"] = float64(agent.ID)
		_, err := handleGatewayAgentTool("agents_update", tc.args, "p1", client, s.store, "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err=%v, want %q", tc.args, err, tc.want)
		}
		if len(fake.writes) != 0 {
			t.Fatalf("%v: wrote %v before rejecting", tc.args, fake.writes)
		}
	}

	fake, client := newFakeModelsAPI(t, "p1")
	if _, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name": "Second", "directive": "Help.", "config": map[string]any{"proactivity": 60},
	}, "p1", client, s.store, ""); err == nil || !strings.Contains(err.Error(), "config.proactivity is not accepted") {
		t.Fatalf("create with config.proactivity: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("create sent %v before rejecting", fake.requests)
	}

	// A write the agent did not keep is reported, not passed off as done.
	fake, client = newFakeModelsAPI(t, "p1")
	_, err = handleGatewayAgentTool("agents_update", map[string]any{"id": float64(agent.ID), "proactivity": float64(60)}, "p1", client, s.store, "")
	if err == nil || !strings.Contains(err.Error(), "proactivity was not saved: the agent still has 25") {
		t.Fatalf("unsaved proactivity: %v", err)
	}
	if body, _ := json.Marshal(fake.writes); string(body) != `[{"proactivity":60}]` {
		t.Fatalf("config body: %s", body)
	}
}
