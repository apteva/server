package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Three small unit tests for the runner extensions added in v0.3:
//   - basic_auth derivation in normalizeCredentials
//   - resolveTemplate substitutes {{X}} against the credential map
//   - formEncode marshals body fields as x-www-form-urlencoded

func TestNormalizeCredentials_DerivesBasicAuth(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string // expected basic_auth value (decoded), empty = should not be set
	}{
		{
			"twilio shape",
			map[string]string{"account_sid": "AC123", "auth_token": "secret"},
			"AC123:secret",
		},
		{
			"generic basic",
			map[string]string{"username": "alice", "password": "wonderland"},
			"alice:wonderland",
		},
		{
			"missing one half",
			map[string]string{"account_sid": "AC123"},
			"",
		},
		{
			"explicit override wins",
			map[string]string{"account_sid": "AC123", "auth_token": "secret", "basic_auth": "PRECOMPUTED"},
			"", // checked separately — the explicit value should be preserved
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizeCredentials(tc.in)
			got := out["basic_auth"]
			if tc.name == "explicit override wins" {
				if got != "PRECOMPUTED" {
					t.Errorf("expected explicit basic_auth to survive, got %q", got)
				}
				return
			}
			if tc.want == "" {
				if got != "" {
					t.Errorf("expected no basic_auth, got %q", got)
				}
				return
			}
			decoded, err := base64.StdEncoding.DecodeString(got)
			if err != nil {
				t.Fatalf("basic_auth not valid base64: %v", err)
			}
			if string(decoded) != tc.want {
				t.Errorf("basic_auth decoded = %q, want %q", decoded, tc.want)
			}
		})
	}
}

func TestResolveTemplate_SubstitutesCredentialPlaceholders(t *testing.T) {
	creds := map[string]string{
		"account_sid": "AC123",
		"auth_token":  "secret",
	}
	got := resolveTemplate("/Accounts/{{account_sid}}/Messages.json", creds)
	if got != "/Accounts/AC123/Messages.json" {
		t.Errorf("path resolve: got %q", got)
	}
	got = resolveTemplate("Basic {{basic_auth}}", creds)
	if !strings.HasPrefix(got, "Basic ") || got == "Basic {{basic_auth}}" {
		t.Errorf("basic_auth resolve: got %q", got)
	}
}

func TestFormEncode_BasicShape(t *testing.T) {
	got := formEncode(map[string]any{
		"From": "+15551112222",
		"To":   "+15553334444",
		"Body": "hello world",
	})
	// url.Values.Encode sorts keys alphabetically, so the order is
	// deterministic.
	if !strings.Contains(got, "Body=hello+world") {
		t.Errorf("missing Body field: %q", got)
	}
	if !strings.Contains(got, "From=%2B15551112222") {
		t.Errorf("missing/unencoded From: %q", got)
	}
	if !strings.Contains(got, "To=%2B15553334444") {
		t.Errorf("missing/unencoded To: %q", got)
	}
}

func TestFormEncode_RepeatsArrays(t *testing.T) {
	got := formEncode(map[string]any{
		"MediaUrl": []any{"https://x/a.png", "https://x/b.png"},
	})
	// Each array element becomes a repeated key=value pair.
	if strings.Count(got, "MediaUrl=") != 2 {
		t.Errorf("expected MediaUrl repeated twice, got %q", got)
	}
}
