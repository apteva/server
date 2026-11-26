package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func stoppedConfigFixture(t *testing.T) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent, err := s.store.CreateAgent(1, "stopped-agent", "directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["realtime_voice_mcp"] = []any{"channels"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(s.agents.instanceDir(agent.ID), "config.json")
}

func putStoppedConfig(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleUpdateConfig(w, authedRequest(t, http.MethodPut, "/instances/1/config", "", body))
	return w
}

func readSavedConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A value Core cannot decode would stop the agent from starting at all, so
// it is refused before anything is written.
func TestStoppedConfigRefusesSettingCoreCannotDecode(t *testing.T) {
	s, path := stoppedConfigFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"voice apps as bool", map[string]any{"realtime_voice_mcp": false}, "realtime_voice_mcp must be a list of strings, not true or false"},
		{"servers as string", map[string]any{"mcp_servers": "tasks"}, "mcp_servers must be a list of objects, not a string"},
		{"server entry as string", map[string]any{"mcp_servers": []any{"tasks"}}, "mcp_servers entries must be an object, not a string"},
		{"server args as string", map[string]any{"mcp_servers": []any{map[string]any{"name": "x", "args": "-v"}}}, "mcp_servers.args must be a list of strings, not a string"},
		{"realtime switch as string", map[string]any{"realtime_enabled": "yes"}, "realtime_enabled must be true or false, not a string"},
		{"memory switch as object", map[string]any{"unconscious": map[string]any{"enabled": true}}, "unconscious must be true or false, not an object"},
		{"pace wake as number", map[string]any{"main_pace": map[string]any{"next_wake_at": 5}}, "main_pace has a time that is not an RFC 3339 timestamp"},
		{"pace wake as bad text", map[string]any{"main_pace": map[string]any{"next_wake_at": "tomorrow"}}, "main_pace has a time that is not an RFC 3339 timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := putStoppedConfig(t, s, tc.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%q, want 400 with %q", w.Code, w.Body.String(), tc.want)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("config.json changed:\n%s", after)
			}
		})
	}
}

func TestStoppedConfigSavesNormalizedVoiceAppsAndRemovesNull(t *testing.T) {
	s, path := stoppedConfigFixture(t)
	if w := putStoppedConfig(t, s, map[string]any{"realtime_voice_mcp": []any{" telephony ", "", "telephony", "crm"}}); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := readSavedConfig(t, path)["realtime_voice_mcp"]; !reflect.DeepEqual(got, []any{"telephony", "crm"}) {
		t.Fatalf("realtime_voice_mcp = %#v", got)
	}
	if w := putStoppedConfig(t, s, map[string]any{"realtime_voice_mcp": nil}); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg := readSavedConfig(t, path); cfg["realtime_voice_mcp"] != nil {
		t.Fatalf("null left the setting in place: %#v", cfg["realtime_voice_mcp"])
	} else if _, present := cfg["realtime_voice_mcp"]; present {
		t.Fatal("null was saved instead of removing the setting")
	}
}
