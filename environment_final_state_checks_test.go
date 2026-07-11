package main

import "testing"

func TestEnvironmentStateMatcherSupportsSubsetAndArrays(t *testing.T) {
	actual := map[string]any{
		"contact": map[string]any{
			"job_title": "VP of Partnerships",
			"attributes": []any{
				map[string]any{"key": "lifecycle", "value": "customer"},
				map[string]any{"key": "lead_score", "value": float64(91)},
			},
		},
	}
	contact, ok := selectAssertionPath(actual, "contact")
	if !ok || !assertionMatches(contact, map[string]any{"job_title": "VP of Partnerships"}, false) {
		t.Fatal("subset object match failed")
	}
	attributes, ok := selectAssertionPath(actual, "contact.attributes")
	if !ok || !assertionMatches(attributes, []any{map[string]any{"key": "lifecycle", "value": "customer"}}, false) {
		t.Fatal("subset array match failed")
	}
}

func TestEnvironmentStateMatcherContainsAndCount(t *testing.T) {
	if !assertionContains("@media (max-width: 720px)", "@media") {
		t.Fatal("string contains failed")
	}
	if count, ok := assertionCount([]any{"a", "b"}); !ok || count != 2 {
		t.Fatalf("array count = %d, %v", count, ok)
	}
}
