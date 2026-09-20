package main

import (
	"encoding/json"
	"strings"
)

// Agent controls are structured server-owned inputs to runtime policy. The
// operator directive remains clean, while each registered control can compile
// private model guidance or, in the future, a hard server/runtime constraint.
// Never persist compiled prompt fragments as the agent's public directive.
const (
	agentControlApprovalMode = "execution.approval"
	agentControlInitiative   = "initiative.level"
)

type agentControlDefinition struct {
	Key         string
	Visibility  string
	Enforcement string
	Inheritance string
	Value       func(agentControlValues) any
	Prompt      func(agentControlValues) string
}

type agentControlValues struct {
	Mode        string
	Proactivity int
}

var agentControlRegistry = []agentControlDefinition{
	{
		Key: agentControlApprovalMode, Visibility: "operator", Enforcement: "prompt", Inheritance: "no_escalation",
		Value: func(values agentControlValues) any { return values.Mode },
		Prompt: func(values agentControlValues) string {
			var rule string
			switch values.Mode {
			case "cautious":
				rule = "Cautious: you may inspect and perform read-only work within your assigned scope. Before an action that changes state or has external effects, explain the action, request approval through an available user communication channel, and wait for approval. If you cannot obtain approval, leave that action pending."
			case "learn":
				rule = "Learn: before using an unfamiliar combination of tool and scope, including read-only actions, explain what you intend to do and request approval through an available user communication channel. Wait for approval. You may reuse approval only for the same tool and approved scope when that approval is still available in your context; ask again if uncertain. This instruction does not provide a dedicated safety-profile memory store."
			default:
				rule = "Autonomous: proceed independently within the assigned scope and available permissions. Clarify material uncertainty and respect explicit approval requirements and user constraints."
			}
			return "Server-managed behavior instructions (" + values.Mode + "). These are instructions, not enforced approval gates.\n" + rule
		},
	},
	{
		Key: agentControlInitiative, Visibility: "operator", Enforcement: "prompt", Inheritance: "capped",
		Value:  func(values agentControlValues) any { return values.Proactivity },
		Prompt: func(values agentControlValues) string { return agentProactivityInstructions(values.Proactivity) },
	},
}

type agentControlView struct {
	Value       any    `json:"value"`
	Editable    bool   `json:"editable"`
	Enforcement string `json:"enforcement"`
	Inheritance string `json:"inheritance"`
}

type publicAgentAlias Agent

// MarshalJSON keeps every agent-list/detail response on the public side of
// the control boundary, even while a legacy row is awaiting migration.
func (inst Agent) MarshalJSON() ([]byte, error) {
	clean := inst
	clean.Directive = withoutAgentControls(clean.Directive)
	if strings.TrimSpace(clean.Config) != "" {
		var config map[string]any
		if json.Unmarshal([]byte(clean.Config), &config) == nil && config != nil {
			sanitizeDirectiveFields(config)
			if encoded, err := json.Marshal(config); err == nil {
				clean.Config = string(encoded)
			}
		}
	}
	return json.Marshal(struct {
		publicAgentAlias
		Controls map[string]agentControlView `json:"controls"`
	}{publicAgentAlias: publicAgentAlias(clean), Controls: publicAgentControls(&clean)})
}

func publicAgentControls(inst *Agent) map[string]agentControlView {
	if inst == nil {
		return nil
	}
	values := agentControlValues{Mode: agentMode(inst.Mode), Proactivity: inst.Proactivity}
	out := make(map[string]agentControlView, len(agentControlRegistry))
	for _, definition := range agentControlRegistry {
		if definition.Visibility != "operator" {
			continue
		}
		out[definition.Key] = agentControlView{
			Value:       definition.Value(values),
			Editable:    true,
			Enforcement: definition.Enforcement,
			Inheritance: definition.Inheritance,
		}
	}
	return out
}

func renderAgentControlPolicy(mode string, proactivity int) string {
	values := agentControlValues{Mode: agentMode(mode), Proactivity: proactivity}
	fragments := make([]string, 0, len(agentControlRegistry))
	for _, definition := range agentControlRegistry {
		if definition.Prompt == nil {
			continue
		}
		if fragment := definition.Prompt(values); fragment != "" {
			fragments = append(fragments, fragment)
		}
	}
	return strings.Join(fragments, "\n")
}

// withoutAgentControls returns the operator/core-authored part of a directive.
// The exact marker namespace is reserved for server-generated policy. Remove
// the two separator newlines that compileAgentDirective adds when the managed
// section is appended, but otherwise preserve authored whitespace verbatim.
func withoutAgentControls(directive string) string {
	matches := behaviorSection.FindAllStringIndex(directive, -1)
	if len(matches) == 0 {
		return directive
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		if start >= last+2 && directive[start-2:start] == "\n\n" {
			start -= 2
		}
		out.WriteString(directive[last:start])
		last = end
	}
	out.WriteString(directive[last:])
	return out.String()
}

func compileAgentDirective(directive string, mode string, proactivity int) string {
	return withAgentBehavior(withoutAgentControls(directive), mode, proactivity)
}

func sanitizeDirectiveFields(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "directive" {
				if directive, ok := child.(string); ok {
					typed[key] = withoutAgentControls(directive)
				}
				continue
			}
			sanitizeDirectiveFields(child)
		}
	case []any:
		for _, child := range typed {
			sanitizeDirectiveFields(child)
		}
	case []map[string]any:
		for _, child := range typed {
			sanitizeDirectiveFields(child)
		}
	}
}

func sanitizeDirectiveJSON(data []byte) ([]byte, bool) {
	var payload any
	if json.Unmarshal(data, &payload) != nil {
		return data, false
	}
	sanitizeDirectiveFields(payload)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return encoded, true
}

func sanitizeDirectiveSSELine(line []byte) []byte {
	if !strings.HasPrefix(string(line), "data: ") {
		return line
	}
	payload := strings.TrimSuffix(strings.TrimSuffix(string(line[len("data: "):]), "\n"), "\r")
	clean, ok := sanitizeDirectiveJSON([]byte(payload))
	if !ok {
		return line
	}
	return append(append([]byte("data: "), clean...), '\n')
}

func publicTelemetryEvent(event TelemetryEvent) TelemetryEvent {
	if len(event.Data) == 0 {
		return event
	}
	var payload any
	if json.Unmarshal(event.Data, &payload) != nil {
		return event
	}
	sanitizeDirectiveFields(payload)
	if encoded, err := json.Marshal(payload); err == nil {
		event.Data = encoded
	}
	return event
}

func publicTelemetryEvents(events []TelemetryEvent) []TelemetryEvent {
	out := make([]TelemetryEvent, len(events))
	for i, event := range events {
		out[i] = publicTelemetryEvent(event)
	}
	return out
}
