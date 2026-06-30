package main

import (
	"strings"
	"testing"
)

func TestAppendDirectiveLearningCreatesSection(t *testing.T) {
	before := "# Role\nYou are a test agent.\n\n# Goals\n- Help users."
	after := appendDirectiveLearning(before, []string{"Always confirm before deleting records."})

	if !strings.Contains(after, "# Learning\n- Always confirm before deleting records.") {
		t.Fatalf("expected learning section, got:\n%s", after)
	}
	if !strings.Contains(after, "# Goals\n- Help users.") {
		t.Fatalf("expected existing section preserved, got:\n%s", after)
	}
}

func TestAppendDirectiveLearningUsesExistingSection(t *testing.T) {
	before := "# Role\nYou are a test agent.\n\n# Learning\n- Existing lesson.\n\n# Tone\nBe brief."
	after := appendDirectiveLearning(before, []string{"Prefer the CRM over spreadsheets."})

	if !strings.Contains(after, "# Learning\n- Existing lesson.\n\n- Prefer the CRM over spreadsheets.\n\n# Tone") {
		t.Fatalf("expected insertion inside Learning before next section, got:\n%s", after)
	}
}

func TestAppendDirectiveLearningPlainDirectivePreservesLegacyAppend(t *testing.T) {
	before := "You are a test agent."
	after := appendDirectiveLearning(before, []string{"Always cite sources."})

	want := "You are a test agent.\n\nAlways cite sources."
	if after != want {
		t.Fatalf("expected %q, got %q", want, after)
	}
}

func TestApplyServerDirectiveEditsSectionReplaceLine(t *testing.T) {
	before := "# Role\nYou are a test agent.\n\n# Tools and Integrations\n- Use Slack.\n- Use CRM."
	after, changed, err := applyServerDirectiveEdits(before, map[string]any{
		"directive_edit_mode": "section_replace_line",
		"directive_section":   "Tools and Integrations",
		"directive_match":     "Use Slack",
		"directive_content":   "- Use Slack only for inbound notifications.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(after, "- Use Slack only for inbound notifications.") {
		t.Fatalf("expected replacement line, got:\n%s", after)
	}
	if strings.Contains(after, "- Use Slack.\n") {
		t.Fatalf("expected old line removed, got:\n%s", after)
	}
}

func TestApplyServerDirectiveEditsSectionReplaceLineSearchesNestedSubsections(t *testing.T) {
	before := "# Current Offer Focus\nDeskora Reception.\n\n## Deskora Reception Overview\nWebsite: http://deskorareception.com/\nOutreach email: contact@deskoraception.com\n\n# Responsibilities\n- Qualify leads"
	after, changed, err := applyServerDirectiveEdits(before, map[string]any{
		"directive_edit_mode": "section_replace_line",
		"directive_section":   "Current Offer Focus",
		"directive_match":     "Outreach email:",
		"directive_content":   "Outreach email: contact@deskorareception.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(after, "Outreach email: contact@deskorareception.com") {
		t.Fatalf("expected nested replacement line, got:\n%s", after)
	}
	if strings.Contains(after, "contact@deskoraception.com") {
		t.Fatalf("expected typo removed, got:\n%s", after)
	}
}

func TestApplyServerDirectiveEditsSectionReplaceLineStopsAtSiblingSection(t *testing.T) {
	_, _, err := applyServerDirectiveEdits("# Current Offer Focus\nDeskora Reception.\n\n# Responsibilities\nOutreach email: contact@deskoraception.com", map[string]any{
		"directive_edit_mode": "section_replace_line",
		"directive_section":   "Current Offer Focus",
		"directive_match":     "Outreach email:",
		"directive_content":   "Outreach email: contact@deskorareception.com",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `directive line containing "Outreach email:" not found`) {
		t.Fatalf("error = %q", err)
	}
}

func TestApplyServerDirectiveEditsBatch(t *testing.T) {
	before := "# Role\nYou are a test agent."
	after, changed, err := applyServerDirectiveEdits(before, map[string]any{
		"directive_edits": `[{"mode":"section_append","section":"Goals","content":"- Keep subscriptions clear."},{"mode":"section_append","section":"Learning","content":"- Remember project-specific MCP choices."}]`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	for _, want := range []string{"# Goals\n- Keep subscriptions clear.", "# Learning\n- Remember project-specific MCP choices."} {
		if !strings.Contains(after, want) {
			t.Fatalf("expected %q in:\n%s", want, after)
		}
	}
}
