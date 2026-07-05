package main

import (
	"encoding/base64"
	"net/url"
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
			"login password basic",
			map[string]string{"login": "api@example.com", "password": "secret"},
			"api@example.com:secret",
		},
		{
			"api key empty password basic",
			map[string]string{"api_key": "close-api-key"},
			"close-api-key:",
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
		"Body": "hello environment",
	})
	// url.Values.Encode sorts keys alphabetically, so the order is
	// deterministic.
	if !strings.Contains(got, "Body=hello+environment") {
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

func TestFormEncode_NestedStripeShape(t *testing.T) {
	encoded := formEncode(map[string]any{
		"mode":        "payment",
		"success_url": "https://example.test/success",
		"line_items": []any{
			map[string]any{
				"quantity": float64(1),
				"price_data": map[string]any{
					"currency":    "usd",
					"unit_amount": float64(2000),
					"product_data": map[string]any{
						"name": "Starter",
					},
				},
			},
		},
		"metadata": map[string]any{
			"apteva_invoice_id": "123",
		},
	})
	values, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	assertFormValue := func(key, want string) {
		t.Helper()
		if got := values.Get(key); got != want {
			t.Fatalf("%s=%q, want %q; body=%s", key, got, want, encoded)
		}
	}
	assertFormValue("line_items[0][price_data][currency]", "usd")
	assertFormValue("line_items[0][price_data][product_data][name]", "Starter")
	assertFormValue("line_items[0][quantity]", "1")
	assertFormValue("metadata[apteva_invoice_id]", "123")
	if values.Get("line_items") != "" {
		t.Fatalf("line_items encoded as scalar: %s", encoded)
	}
}
