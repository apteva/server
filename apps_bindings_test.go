package main

import (
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestBuildPreflightRoles_IntegrationFallsBackToCompatibleAppNames(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	if _, err := s.store.CreateConnection(1, "zernio", "Zernio", "Zernio", "api_key", "enc", "proj-1"); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	manifest := &sdk.Manifest{}
	manifest.Requires.Integrations = []sdk.IntegrationDep{{
		Role:               "social_provider",
		Kind:               "integration",
		CompatibleAppNames: []string{"zernio"},
	}}

	roles := s.buildPreflightRoles(manifest, "proj-1", 1)
	if len(roles) != 1 {
		t.Fatalf("roles = %d, want 1", len(roles))
	}
	if len(roles[0].IntegrationCands) != 1 {
		t.Fatalf("integration candidates = %+v, want zernio", roles[0].IntegrationCands)
	}
	if roles[0].IntegrationCands[0].AppSlug != "zernio" {
		t.Fatalf("candidate slug = %q, want zernio", roles[0].IntegrationCands[0].AppSlug)
	}
}
