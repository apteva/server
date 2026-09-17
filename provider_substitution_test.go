package main

import (
	"strings"
	"testing"
)

// textProvider builds a provider that passes eligibility with no model policy.
func textProvider(providerType string) ProviderInfo {
	return ProviderInfo{
		Type:       providerType,
		ModelLarge: "large-1", ModelMedium: "medium-1", ModelSmall: "small-1",
	}
}

// policedProvider builds a provider whose model policy only admits ids
// matching allowed, which lets a test force a specific tier to fail.
func policedProvider(providerType, large, medium, small string, allowed []string) ProviderInfo {
	return ProviderInfo{
		Type:       providerType,
		ModelLarge: large, ModelMedium: medium, ModelSmall: small,
		// allowsID only admits ids when Purpose is "agent"; without it the
		// policy denies everything and every tier looks blocked.
		ModelPolicy: &RuntimeModelPolicy{Purpose: "agent", AllowedIDPatterns: allowed},
	}
}

// The regression this whole change exists for: an explicit pin that the pool
// cannot satisfy must not look identical to no pin at all.
func TestResolveProviderDefault_UnsatisfiablePinIsDistinguishable(t *testing.T) {
	pool := []ProviderInfo{textProvider("fireworks"), textProvider("anthropic")}

	pinned := resolveProviderDefault(pool, "opencode-go")
	unpinned := resolveProviderDefault(pool, "")

	if pinned.Provider != "fireworks" || unpinned.Provider != "fireworks" {
		t.Fatalf("expected both to fall back to fireworks, got pinned=%q unpinned=%q",
			pinned.Provider, unpinned.Provider)
	}
	if !pinned.Substituted {
		t.Error("unsatisfiable pin must report Substituted")
	}
	if unpinned.Substituted {
		t.Error("unpinned request must not report Substituted")
	}
	if pinned.Requested != "opencode-go" {
		t.Errorf("Requested = %q, want opencode-go", pinned.Requested)
	}
	if !strings.Contains(pinned.Reason, "not configured") {
		t.Errorf("Reason = %q, want it to say the provider is not configured", pinned.Reason)
	}
}

func TestResolveProviderDefault_HonoredPinIsNotSubstituted(t *testing.T) {
	pool := []ProviderInfo{textProvider("fireworks"), textProvider("opencode-go")}

	got := resolveProviderDefault(pool, "opencode-go")
	if got.Provider != "opencode-go" {
		t.Fatalf("Provider = %q, want opencode-go", got.Provider)
	}
	if got.Substituted || got.Reason != "" {
		t.Errorf("honored pin must not be marked substituted (got %+v)", got)
	}
}

// A pin dropped by model policy must say so, and name the failing tier —
// that is what separates "filtered" from "never in scope".
func TestResolveProviderDefault_PolicyFilteredPinNamesTheTier(t *testing.T) {
	pool := []ProviderInfo{
		textProvider("fireworks"),
		policedProvider("opencode-go", "ok-large", "blocked-medium", "ok-small", []string{"^ok-"}),
	}

	got := resolveProviderDefault(pool, "opencode-go")
	if !got.Substituted {
		t.Fatal("policy-filtered pin must report Substituted")
	}
	if !strings.Contains(got.Reason, "model policy") {
		t.Errorf("Reason = %q, want it to attribute the drop to model policy", got.Reason)
	}
	if !strings.Contains(got.Reason, "blocked-medium") {
		t.Errorf("Reason = %q, want it to name the offending model", got.Reason)
	}
}

func TestResolveProviderDefault_RealtimeOnlyPinExplainsItself(t *testing.T) {
	pool := []ProviderInfo{textProvider("fireworks"), textProvider("openai-realtime")}

	got := resolveProviderDefault(pool, "openai-realtime")
	if got.Provider != "fireworks" || !got.Substituted {
		t.Fatalf("realtime pin should substitute a text provider, got %+v", got)
	}
	if !strings.Contains(got.Reason, "realtime-only") {
		t.Errorf("Reason = %q, want it to explain the realtime restriction", got.Reason)
	}
}

func TestResolveProviderDefault_EmptyPoolReportsNoProvider(t *testing.T) {
	got := resolveProviderDefault(nil, "opencode-go")
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty", got.Provider)
	}
	if !got.Substituted {
		t.Error("an unsatisfiable pin against an empty pool is still a substitution")
	}
}

// effectiveProviderDefault must keep returning exactly what it returned
// before this change; every existing call site depends on that.
func TestEffectiveProviderDefault_BehaviorUnchanged(t *testing.T) {
	pool := []ProviderInfo{textProvider("fireworks"), textProvider("opencode-go")}

	cases := []struct{ configured, want string }{
		{"opencode-go", "opencode-go"},
		{"OpenCode-Go", "opencode-go"},
		{"nope", "fireworks"},
		{"", "fireworks"},
	}
	for _, tc := range cases {
		if got := effectiveProviderDefault(pool, tc.configured); got != tc.want {
			t.Errorf("effectiveProviderDefault(%q) = %q, want %q", tc.configured, got, tc.want)
		}
	}
	if got := effectiveProviderDefault(nil, "x"); got != "" {
		t.Errorf("empty pool = %q, want empty", got)
	}
}

func TestEligibleProviderPoolWithExclusions_RecordsReasons(t *testing.T) {
	pool := []ProviderInfo{
		textProvider("fireworks"),
		policedProvider("opencode-go", "ok-large", "ok-medium", "blocked-small", []string{"^ok-"}),
	}

	eligible, excluded := eligibleProviderPoolWithExclusions(pool)
	if len(eligible) != 1 || eligible[0].Type != "fireworks" {
		t.Fatalf("eligible = %+v, want only fireworks", eligible)
	}
	if len(excluded) != 1 {
		t.Fatalf("excluded = %+v, want exactly one entry", excluded)
	}
	if excluded[0].Provider != "opencode-go" {
		t.Errorf("excluded provider = %q, want opencode-go", excluded[0].Provider)
	}
	if !strings.Contains(excluded[0].Reason, "blocked-small") {
		t.Errorf("reason = %q, want it to name the blocked model", excluded[0].Reason)
	}
}

// The wrapper must filter identically to the reason-collecting version.
func TestEligibleProviderPool_MatchesExclusionVariant(t *testing.T) {
	pool := []ProviderInfo{
		textProvider("fireworks"),
		policedProvider("opencode-go", "ok-large", "bad-medium", "ok-small", []string{"^ok-"}),
		policedProvider("anthropic", "ok-large", "ok-medium", "ok-small", []string{"^ok-"}),
	}

	plain := eligibleProviderPool(pool)
	withReasons, _ := eligibleProviderPoolWithExclusions(pool)
	if len(plain) != len(withReasons) {
		t.Fatalf("length mismatch: %d vs %d", len(plain), len(withReasons))
	}
	for i := range plain {
		if plain[i].Type != withReasons[i].Type {
			t.Errorf("index %d: %q vs %q", i, plain[i].Type, withReasons[i].Type)
		}
	}
}

func TestProviderIneligibleReason_EligibleProviderHasNoReason(t *testing.T) {
	ok := policedProvider("opencode-go", "ok-large", "ok-medium", "ok-small", []string{"^ok-"})
	if reason := providerIneligibleReason(ok); reason != "" {
		t.Errorf("reason = %q, want empty for an eligible provider", reason)
	}
	bad := ok
	bad.ModelSelectionError = true
	if reason := providerIneligibleReason(bad); reason == "" {
		t.Error("a provider with ModelSelectionError must report a reason")
	}
}
