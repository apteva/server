package main

import (
	"encoding/json"
	"testing"
)

func TestParseDelegatedProviderCredentialsScopedExecuteURL(t *testing.T) {
	raw, err := json.Marshal(map[string]string{
		delegatedProviderMarker:  "1",
		"grant_id":               "fleet:tenant:aws-ses:7",
		"resource":               "provider.connection",
		"controller_execute_url": "https://controller.example/api/apps/fleet/provider-grants/tenant/grant/execute",
		"controller_token":       "scoped-secret",
		"parent_connection_id":   "7",
		"allowed_tools":          `["SendEmail"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, delegated, err := parseDelegatedProviderCredentials(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !delegated {
		t.Fatal("credentials were not recognized as delegated")
	}
	if got := delegatedProviderExecuteURL(grant); got != "https://controller.example/api/apps/fleet/provider-grants/tenant/grant/execute" {
		t.Fatalf("execute URL = %q", got)
	}
	if grant.ControllerInstallID != "" || grant.ControllerGatewayURL != "" {
		t.Fatalf("scoped credentials unexpectedly require parent app credentials: %+v", grant)
	}
}

func TestParseDelegatedProviderCredentialsLegacyFallback(t *testing.T) {
	raw, err := json.Marshal(map[string]string{
		delegatedProviderMarker:  "1",
		"controller_gateway_url": "https://controller.example",
		"controller_token":       "legacy-app-token",
		"controller_install_id":  "42",
		"parent_connection_id":   "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, delegated, err := parseDelegatedProviderCredentials(string(raw))
	if err != nil || !delegated {
		t.Fatalf("parse legacy: delegated=%v err=%v", delegated, err)
	}
	if got := delegatedProviderExecuteURL(grant); got != "https://controller.example/api/apps/callback/integrations/7/execute" {
		t.Fatalf("legacy execute URL = %q", got)
	}
}
