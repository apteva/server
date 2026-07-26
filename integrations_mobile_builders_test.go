package main

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedMobileBuilderCatalogs(t *testing.T) {
	tests := []struct {
		file         string
		slug         string
		toolCount    int
		requiredTool string
	}{
		{file: "integrations-catalog/bitrise.json", slug: "bitrise", toolCount: 97, requiredTool: "trigger_build"},
		{file: "integrations-catalog/appcircle.json", slug: "appcircle", toolCount: 101, requiredTool: "start_build"},
		{file: "integrations-catalog/buildkite.json", slug: "buildkite", toolCount: 51, requiredTool: "create_build"},
	}

	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			raw, err := integrationsCatalogFS.ReadFile(tt.file)
			if err != nil {
				t.Fatalf("read embedded catalog: %v", err)
			}
			var app AppTemplate
			if err := json.Unmarshal(raw, &app); err != nil {
				t.Fatalf("decode embedded catalog: %v", err)
			}
			if app.Slug != tt.slug {
				t.Fatalf("slug=%q want=%q", app.Slug, tt.slug)
			}
			if len(app.Tools) != tt.toolCount {
				t.Fatalf("tool count=%d want=%d", len(app.Tools), tt.toolCount)
			}
			seen := make(map[string]bool, len(app.Tools))
			for _, tool := range app.Tools {
				if seen[tool.Name] {
					t.Fatalf("duplicate tool %q", tool.Name)
				}
				seen[tool.Name] = true
			}
			if !seen[tt.requiredTool] {
				t.Fatalf("missing required tool %q", tt.requiredTool)
			}
		})
	}
}

func TestEmbeddedAppcircleUsesDurableAPIKeyExchange(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/appcircle.json")
	if err != nil {
		t.Fatalf("read embedded Appcircle catalog: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode embedded Appcircle catalog: %v", err)
	}
	exchange := app.Auth.TokenExchange
	if exchange == nil {
		t.Fatal("Appcircle token exchange is missing")
	}
	if exchange.URL != "https://auth.appcircle.io/auth/v1/api-key/token" {
		t.Fatalf("token exchange URL=%q", exchange.URL)
	}
	if exchange.BodyParams["name"] != "{{credential.api_key_name}}" ||
		exchange.BodyParams["secret"] != "{{credential.api_key_secret}}" {
		t.Fatalf("unexpected exchange body params: %#v", exchange.BodyParams)
	}
}
