package main

import (
	"reflect"
	"testing"
)

func TestEnvironmentSourceAgentPolicyPreservesRealtimeSelection(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"realtime_enabled": true,
		"realtime_voice": "Kore",
		"realtime_voice_mcp": ["flexylead-bookings"],
		"mcp_servers": [
			{
				"name": "flexylead-bookings",
				"url": "http://source.test/mcp",
				"tool_loading": {"default": "always"}
			},
			{"name": "crm", "url": "http://source.test/crm"}
		]
	}`)

	bookings := policy.mcpConfig("flexylead-bookings", "http://runtime.test/bookings")
	if bookings["no_spawn"] != false {
		t.Fatalf("bookings no_spawn=%#v, want false", bookings["no_spawn"])
	}
	if got := bookings["tool_loading"]; !reflect.DeepEqual(got, map[string]any{"default": "always"}) {
		t.Fatalf("bookings tool_loading=%#v", got)
	}
	if crm := policy.mcpConfig("crm", "http://runtime.test/crm"); crm["no_spawn"] != true {
		t.Fatalf("crm no_spawn=%#v, want true", crm["no_spawn"])
	}
	if unknown := policy.mcpConfig("runtime-only", "http://runtime.test/other"); unknown["no_spawn"] != true {
		t.Fatalf("runtime-only no_spawn=%#v, want true", unknown["no_spawn"])
	}

	config := map[string]any{}
	policy.copyRealtimeConfig(config)
	if config["realtime_enabled"] != true || config["realtime_voice"] != "Kore" {
		t.Fatalf("realtime config=%#v", config)
	}
	if got := config["realtime_voice_mcp"]; !reflect.DeepEqual(got, []any{"flexylead-bookings"}) {
		t.Fatalf("realtime_voice_mcp=%#v", got)
	}
}

func TestEnvironmentSourceAgentPolicyFallsBackToSourceNoSpawn(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"mcp_servers": [
			{"name": "bookings", "url": "http://source.test/bookings"},
			{"name": "private", "url": "http://source.test/private", "no_spawn": true},
			{"name": "malformed", "url": "http://source.test/malformed", "no_spawn": "false"}
		]
	}`)

	if got := policy.mcpConfig("bookings", "http://runtime.test/bookings")["no_spawn"]; got != false {
		t.Fatalf("bookings no_spawn=%#v, want false", got)
	}
	if got := policy.mcpConfig("private", "http://runtime.test/private")["no_spawn"]; got != true {
		t.Fatalf("private no_spawn=%#v, want true", got)
	}
	if got := policy.mcpConfig("malformed", "http://runtime.test/malformed")["no_spawn"]; got != true {
		t.Fatalf("malformed no_spawn=%#v, want true", got)
	}
	if got := policy.mcpConfig("runtime-only", "http://runtime.test/other")["no_spawn"]; got != true {
		t.Fatalf("runtime-only no_spawn=%#v, want true", got)
	}
}

func TestEnvironmentSourceAgentPolicyExplicitEmptyAllowlistDeniesAll(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"realtime_voice_mcp": [],
		"mcp_servers": [{"name": "bookings", "url": "http://source.test/bookings"}]
	}`)
	if got := policy.mcpConfig("bookings", "http://runtime.test/bookings")["no_spawn"]; got != true {
		t.Fatalf("bookings no_spawn=%#v, want true", got)
	}
}
