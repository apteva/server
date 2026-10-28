package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvironmentStartupExecutionControl(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec EnvironmentAgentSpec
		want string
	}{
		{"auto", EnvironmentAgentSpec{}, `{"mode":"auto"}`},
		{"legacy", EnvironmentAgentSpec{StartPaused: true}, `{"mode":"paused","scope":"instance","breakpoints":["iteration.start"]}`},
		{"instance", EnvironmentAgentSpec{StartPaused: true, StartPausedScope: "instance"}, `{"mode":"paused","scope":"instance","breakpoints":["iteration.start"]}`},
		{"thread", EnvironmentAgentSpec{StartPaused: true, StartPausedScope: "thread", StartPausedThreadID: "main"}, `{"mode":"auto","scope":"instance","threads":{"main":{"mode":"paused","breakpoints":["iteration.start"]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := environmentStartupExecutionControl(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("got %s want %s", a, b)
			}
		})
	}
	for _, spec := range []EnvironmentAgentSpec{
		{StartPausedScope: "thread", StartPausedThreadID: "main"},
		{StartPaused: true, StartPausedScope: "all"},
		{StartPaused: true, StartPausedThreadID: "main"},
		{StartPaused: true, StartPausedScope: "thread"},
	} {
		if _, err := environmentStartupExecutionControl(spec); err == nil {
			t.Fatalf("invalid startup scope accepted: %+v", spec)
		}
	}
	for _, id := range []string{"../main", "a/b", "a\\b", "a b", "a?b", "é", strings.Repeat("x", 129)} {
		if _, err := environmentStartupExecutionControl(EnvironmentAgentSpec{StartPaused: true, StartPausedScope: "thread", StartPausedThreadID: id}); err == nil {
			t.Fatalf("invalid thread ID accepted: %q", id)
		}
	}
}
