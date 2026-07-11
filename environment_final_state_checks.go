package main

// environment_final_state_checks.go contains the deterministic half of environment
// evals. It deliberately uses the same authenticated app-tool path as seeding,
// but accepts only read-shaped tool names. This lets scenarios grade final
// state without baking app storage internals into the benchmark runner.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func (s *Server) evaluateEnvironmentStateChecks(environment *Environment, checks []EnvironmentStateCheck) (*DeterministicVerdict, error) {
	if len(checks) == 0 {
		return nil, nil
	}
	verdict := &DeterministicVerdict{Overall: "pass", Checks: make([]DeterministicCheckResult, 0, len(checks))}
	for i, check := range checks {
		result := DeterministicCheckResult{ID: check.ID, Description: check.Description}
		if result.ID == "" {
			result.ID = fmt.Sprintf("check-%d", i+1)
		}
		if !isReadOnlyAssertionTool(check.Tool) {
			return nil, fmt.Errorf("final state check %q uses non-read-only tool %q", result.ID, check.Tool)
		}
		responses, err := s.ExecuteSeedPlan(environment, []SeedCall{{App: check.App, Tool: check.Tool, Input: check.Input}})
		if err != nil {
			return nil, fmt.Errorf("final state check %q: %w", result.ID, err)
		}
		if len(responses) != 1 {
			return nil, fmt.Errorf("final state check %q returned no observation", result.ID)
		}
		observed, err := decodeSeedResult(responses[0])
		if err != nil {
			return nil, fmt.Errorf("final state check %q: %w", result.ID, err)
		}
		selected, found := selectAssertionPath(observed, check.Path)
		if !found {
			result.Reason = fmt.Sprintf("path %q was not present", check.Path)
		} else if count, countable := assertionCount(selected); (check.MinCount != nil || check.MaxCount != nil) && !countable {
			result.Reason = fmt.Sprintf("path %q is not a collection", check.Path)
		} else if check.MinCount != nil && count < *check.MinCount {
			result.Reason = fmt.Sprintf("count %d is below minimum %d", count, *check.MinCount)
		} else if check.MaxCount != nil && count > *check.MaxCount {
			result.Reason = fmt.Sprintf("count %d exceeds maximum %d", count, *check.MaxCount)
		} else if check.Contains != "" && !assertionContains(selected, check.Contains) {
			result.Reason = fmt.Sprintf("observed state did not contain %q", check.Contains)
		} else if check.Expected != nil && !assertionMatches(selected, check.Expected, check.Exact) {
			result.Reason = "observed state did not match expected value"
		} else {
			result.Pass = true
		}
		if preview, err := json.Marshal(selected); err == nil {
			result.Observed = compactAssertionObservation(preview)
		}
		if !result.Pass {
			verdict.Overall = "fail"
		}
		verdict.Checks = append(verdict.Checks, result)
	}
	return verdict, nil
}

func isReadOnlyAssertionTool(tool string) bool {
	name := strings.ToLower(strings.TrimSpace(tool))
	return strings.Contains(name, "_get") || strings.Contains(name, "_list") ||
		strings.Contains(name, "_search") || strings.Contains(name, "_find") ||
		strings.Contains(name, "_read")
}

func selectAssertionPath(value any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return value, true
	}
	current := value
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return nil, false
		}
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[segment]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(segment)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			current = node[i]
		default:
			return nil, false
		}
	}
	return current, true
}

func assertionCount(value any) (int, bool) {
	switch typed := value.(type) {
	case []any:
		return len(typed), true
	case map[string]any:
		return len(typed), true
	case string:
		return len(typed), true
	default:
		return 0, false
	}
}

func assertionMatches(actual, expected any, exact bool) bool {
	if exact {
		return reflect.DeepEqual(actual, expected)
	}
	switch want := expected.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, expectedValue := range want {
			actualValue, ok := got[key]
			if !ok || !assertionMatches(actualValue, expectedValue, false) {
				return false
			}
		}
		return true
	case []any:
		got, ok := actual.([]any)
		if !ok || len(want) > len(got) {
			return false
		}
		used := make([]bool, len(got))
		for _, expectedItem := range want {
			matched := false
			for i, actualItem := range got {
				if !used[i] && assertionMatches(actualItem, expectedItem, false) {
					used[i], matched = true, true
					break
				}
			}
			if !matched {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, expected)
	}
}

func assertionContains(value any, needle string) bool {
	if text, ok := value.(string); ok {
		return strings.Contains(text, needle)
	}
	raw, err := json.Marshal(value)
	return err == nil && strings.Contains(string(raw), needle)
}

func compactAssertionObservation(raw []byte) json.RawMessage {
	const maxBytes = 8 << 10
	if len(raw) <= maxBytes {
		return json.RawMessage(raw)
	}
	preview, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(raw), "preview": string(raw[:maxBytes])})
	return json.RawMessage(preview)
}
