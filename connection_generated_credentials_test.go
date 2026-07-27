package main

import (
	"encoding/base64"
	"testing"
)

func TestMaterializeGeneratedConnectionCredentials(t *testing.T) {
	app := &AppTemplate{}
	app.Auth.CredentialFields = []CredentialField{
		{Name: "team_id", Source: "user"},
		{Name: "relay_encryption_key", Source: "generated", Hidden: true},
	}

	first, err := materializeGeneratedConnectionCredentials(app, map[string]string{
		"team_id":              "TEAM123",
		"relay_encryption_key": "operator-must-not-control-this",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := materializeGeneratedConnectionCredentials(app, map[string]string{"team_id": "TEAM123"})
	if err != nil {
		t.Fatal(err)
	}

	if first["team_id"] != "TEAM123" {
		t.Fatalf("user credential was not preserved: %#v", first)
	}
	if first["relay_encryption_key"] == "operator-must-not-control-this" {
		t.Fatal("generated credential accepted an operator-supplied value")
	}
	if first["relay_encryption_key"] == second["relay_encryption_key"] {
		t.Fatal("generated credentials must be unique per connection")
	}
	raw, err := base64.RawURLEncoding.DecodeString(first["relay_encryption_key"])
	if err != nil {
		t.Fatalf("generated credential is not base64url: %v", err)
	}
	if len(raw) != generatedCredentialBytes {
		t.Fatalf("generated credential has %d bytes, want %d", len(raw), generatedCredentialBytes)
	}
}

func TestMaterializeGeneratedConnectionCredentialsWithoutTemplate(t *testing.T) {
	credentials, err := materializeGeneratedConnectionCredentials(nil, map[string]string{"token": "kept"})
	if err != nil {
		t.Fatal(err)
	}
	if credentials["token"] != "kept" {
		t.Fatalf("credentials changed without a template: %#v", credentials)
	}
}

func TestBackfillGeneratedConnectionCredentialsPreservesExistingSecret(t *testing.T) {
	app := &AppTemplate{}
	app.Auth.CredentialFields = []CredentialField{
		{Name: "relay_encryption_key", Source: "generated", Hidden: true},
	}
	credentials, err := backfillGeneratedConnectionCredentials(app, map[string]string{
		"relay_encryption_key": "already-in-use",
	})
	if err != nil {
		t.Fatal(err)
	}
	if credentials["relay_encryption_key"] != "already-in-use" {
		t.Fatalf("existing generated credential rotated: %#v", credentials)
	}
}
