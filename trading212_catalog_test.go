package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrading212CatalogBasicAuthAndReadOnlyProbe(t *testing.T) {
	primary, err := integrationsCatalogFS.ReadFile("integrations-catalog/trading212.json")
	if err != nil {
		t.Fatal(err)
	}

	var app AppTemplate
	if err := json.Unmarshal(primary, &app); err != nil {
		t.Fatal(err)
	}
	if got := app.Auth.Headers["Authorization"]; got != "Basic {{basic_auth}}" {
		t.Fatalf("authorization template = %q", got)
	}
	if len(app.Auth.CredentialFields) != 2 ||
		app.Auth.CredentialFields[0].Name != "api_key" || app.Auth.CredentialFields[0].Required == nil || !*app.Auth.CredentialFields[0].Required ||
		app.Auth.CredentialFields[1].Name != "api_secret" || app.Auth.CredentialFields[1].Required == nil || !*app.Auth.CredentialFields[1].Required {
		t.Fatal("Trading 212 must require API Key ID and API Secret")
	}
	if app.HealthCheck == nil || app.HealthCheck.Tool != "get_account_summary" {
		t.Fatal("Trading 212 must validate with get_account_summary")
	}
	var probe *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == app.HealthCheck.Tool {
			probe = &app.Tools[i]
			break
		}
	}
	if probe == nil || probe.Method != http.MethodGet || probe.Path != "/equity/account/summary" {
		t.Fatal("health check must use read-only account summary route")
	}

	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		if r.Method != http.MethodGet || r.URL.Path != "/api/v0/equity/account/summary" {
			t.Errorf("probe request = %s %s", r.Method, r.URL.Path)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("key-id:secret-value"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("authorization header differs from API Key ID:API Secret Basic auth")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"currencyCode":"EUR"}`))
	}))
	defer srv.Close()
	app.BaseURL = srv.URL + "/api/v0"
	result, err := executeIntegrationTool(&app, probe, map[string]string{
		"api_key": "key-id", "api_secret": "secret-value",
	}, map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !seen || !result.Success {
		t.Fatal("account summary probe did not succeed")
	}
}
