package main

import (
	"fmt"
	"regexp"
)

var startupPauseThreadID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

// Build the gate before the cloned Core starts. An omitted scope preserves the
// historical instance-wide pause; only an explicit exact thread opts out.
func environmentStartupExecutionControl(spec EnvironmentAgentSpec) (map[string]any, error) {
	scope, id := spec.StartPausedScope, spec.StartPausedThreadID
	if !spec.StartPaused {
		if scope != "" || id != "" {
			return nil, fmt.Errorf("startup pause scope requires start_paused:true")
		}
		return map[string]any{"mode": "auto"}, nil
	}
	rule := map[string]any{"mode": "paused", "breakpoints": []string{"iteration.start"}}
	switch scope {
	case "", "instance":
		if id != "" {
			return nil, fmt.Errorf("instance startup pause cannot specify thread_id")
		}
		rule["scope"] = "instance"
		return rule, nil
	case "thread":
		if len(id) > 128 || !startupPauseThreadID.MatchString(id) {
			return nil, fmt.Errorf("thread startup pause requires a valid exact thread ID")
		}
		return map[string]any{"mode": "auto", "scope": "instance", "threads": map[string]any{id: rule}}, nil
	default:
		return nil, fmt.Errorf("start_paused_scope must be instance or thread")
	}
}
