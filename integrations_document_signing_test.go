package main

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedDocumentSigningCatalogs(t *testing.T) {
	tests := []struct {
		slug         string
		toolCount    int
		requiredTool string
	}{
		{slug: "docusign", toolCount: 26, requiredTool: "create_recipient_view"},
		{slug: "pandadoc", toolCount: 30, requiredTool: "create_document_session"},
		{slug: "dropbox-sign", toolCount: 22, requiredTool: "send_signature_request"},
		{slug: "signwell", toolCount: 23, requiredTool: "create_document"},
		{slug: "docuseal", toolCount: 19, requiredTool: "create_submission"},
		{slug: "documenso", toolCount: 29, requiredTool: "create_envelope"},
	}

	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + tt.slug + ".json")
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
			if app.HealthCheck == nil {
				t.Fatal("health check is missing")
			}

			names := make(map[string]bool, len(app.Tools))
			routes := make(map[string]bool, len(app.Tools))
			for _, tool := range app.Tools {
				if names[tool.Name] {
					t.Fatalf("duplicate tool name %q", tool.Name)
				}
				names[tool.Name] = true
				route := tool.Method + " " + tool.Path
				if routes[route] {
					t.Fatalf("duplicate HTTP route %q", route)
				}
				routes[route] = true
			}
			if !names[tt.requiredTool] {
				t.Fatalf("missing required tool %q", tt.requiredTool)
			}
		})
	}
}
