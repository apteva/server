package main

import (
	"strings"
	"testing"
)

// buildRespondDescription splices the AVAILABLE COMPONENTS catalog
// into the respond tool's docs each turn. The agent learns what's
// renderable without a separate discovery call. This guards the
// happy path + the filtering rules.
func TestBuildRespondDescription_BakesComponents(t *testing.T) {
	channels := []string{"chat", "cli"}
	components := []componentEntry{
		// In scope — chat slot.
		{App: "storage", Name: "file-card", Slots: []string{"chat.message_attachment"}},
		// In scope — chat slot in addition to other slots.
		{App: "social", Name: "post-card", Slots: []string{"chat.message_attachment", "dashboard.project_sidebar"}, Description: "post status"},
		// Out of scope — only sidebar.
		{App: "media", Name: "usage-tile", Slots: []string{"dashboard.project_sidebar"}},
		// Out of scope — empty slots.
		{App: "weird", Name: "no-slots"},
	}
	desc := buildRespondDescription(channels, components)

	// Catalog header + a line for each chat-eligible component.
	if !strings.Contains(desc, "AVAILABLE COMPONENTS") {
		t.Errorf("expected AVAILABLE COMPONENTS header in description")
	}
	if !strings.Contains(desc, `{app:"storage", name:"file-card"}`) {
		t.Errorf("expected storage/file-card in catalog: %s", desc)
	}
	if !strings.Contains(desc, `{app:"social", name:"post-card"}`) {
		t.Errorf("expected social/post-card in catalog")
	}
	if !strings.Contains(desc, "post status") {
		t.Errorf("expected per-component description text rendered")
	}
	// Out-of-scope components must not leak in. Match by the
	// rendered catalog line (which uses {app:"…", name:"…"}) so
	// we don't false-positive on the prose text that mentions
	// "media preview".
	if strings.Contains(desc, `{app:"media"`) || strings.Contains(desc, `{app:"weird"`) {
		t.Errorf("non-chat components leaked into catalog: %s", desc)
	}
}

func TestBuildRespondDescription_EmptyCatalogOmitsBlock(t *testing.T) {
	desc := buildRespondDescription([]string{"chat"}, nil)
	// The header must not appear when there's nothing to list —
	// otherwise the agent thinks something is broken.
	if strings.Contains(desc, "AVAILABLE COMPONENTS") {
		t.Errorf("empty catalog should not render the AVAILABLE COMPONENTS header")
	}
	// Sanity — main description still renders.
	if !strings.Contains(desc, "KNOWN CHANNELS") {
		t.Errorf("main description missing")
	}
}
