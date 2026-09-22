package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestGatewayResultSerializationDoesNotRejectBySize(t *testing.T) {
	value := strings.Repeat("x", 64*1024)
	raw, err := gatewayMarshalToolResult(map[string]any{"value": value})
	if err != nil {
		t.Fatalf("large valid MCP result rejected: %v", err)
	}
	if len(raw) <= len(value) {
		t.Fatalf("serialized result unexpectedly short: %d", len(raw))
	}
}

func TestCompactGatewayAgentRedactsRuntimeConfigAndChunksDirective(t *testing.T) {
	directive := strings.Repeat("directive ", 1200)
	row := compactGatewayAgent(map[string]any{
		"id": 7, "name": "Worker", "status": "running", "directive": directive,
		"config": `{"model":"test-model","mcp_servers":[{"url":"http://127.0.0.1:5280/mcp/1?mcp_token=secret"}],"provider_token":"secret"}`,
		"port":   9921, "pid": 42, "user_id": 9,
	}, true, map[string]any{})
	if row["directive_has_more"] != true || len(row["directive"].(string)) > gatewayDirectiveChunk {
		t.Fatalf("directive was not chunked: %#v", row)
	}
	settings, _ := row["settings"].(map[string]any)
	if settings["model"] != "test-model" {
		t.Fatalf("safe scalar settings missing: %#v", settings)
	}
	raw := string(mustGatewayJSON(row))
	for _, forbidden := range []string{"mcp_token", "provider_token", "127.0.0.1", `"port"`, `"pid"`, "user_id"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("agent detail leaked %q: %s", forbidden, raw)
		}
	}
}

func TestCompactGatewaySetupOmitsFullPresetDirectives(t *testing.T) {
	result := compactGatewaySetup(map[string]any{
		"schema_version": 2,
		"presets": []any{map[string]any{
			"id": "p", "name": "Preset", "description": "A useful preset",
			"agents": []any{map[string]any{"key": "lead", "name": "Lead", "directive": strings.Repeat("private directive ", 500), "mode": "cautious"}},
		}},
	}, "setup_presets_list")
	raw := string(mustGatewayJSON(result))
	if strings.Contains(raw, strings.Repeat("private directive ", 20)) || !strings.Contains(raw, "directive_summary") {
		t.Fatalf("preset response was not compacted: %s", raw)
	}
}

func TestCompactGatewayIntegrationOmitsExecutionTemplates(t *testing.T) {
	app := AppTemplate{
		Slug: "fixture", Name: "Fixture", Description: strings.Repeat("description ", 100), BaseURL: "https://secret-upstream.example",
		Auth:  AppAuthConfig{Types: []string{"api_key"}, Headers: map[string]string{"Authorization": "Bearer {{api_key}}"}, CredentialFields: []CredentialField{{Name: "api_key", Label: "API key"}}},
		Tools: []AppToolDef{{Name: "items_list", Description: "List items", Method: "GET", Path: "/private/items", InputSchema: map[string]any{"type": "object"}}},
	}
	row := compactGatewayIntegration(app, true)
	raw := string(mustGatewayJSON(row))
	for _, forbidden := range []string{"secret-upstream", "Authorization", "{{api_key}}", "/private/items", `"method"`} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("integration summary leaked execution template %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, "credential_fields") || !strings.Contains(raw, "items_list") {
		t.Fatalf("integration summary lost operator-facing metadata: %s", raw)
	}
}

func TestCompactGatewayAppKeepsShortDescription(t *testing.T) {
	row := compactGatewayApp(AppRow{
		InstallID: 4, Name: "notes", DisplayName: "Notes", Description: strings.Repeat("useful ", 100),
		Version: "1.0.0", ProjectID: "p", Status: "running", Serving: true,
		Permissions: []sdk.Permission{"platform.apps.call"},
	}, false)
	description, _ := row["description"].(string)
	if description == "" || len(description) > gatewayDescriptionSize+3 {
		t.Fatalf("short description missing or unbounded: %#v", row)
	}
	if _, heavy := row["permissions"]; heavy {
		t.Fatalf("list row included detail-only permissions: %#v", row)
	}
	for _, detailOnly := range []string{"app_id", "available_version", "serving", "upgrade_in_progress"} {
		if _, found := row[detailOnly]; found {
			t.Fatalf("list row included detail-only field %q: %#v", detailOnly, row)
		}
	}
}

func TestGatewayAppsPageSupportsLargeRequestedPageEfficiently(t *testing.T) {
	items := make([]any, 0, 110)
	for i := 0; i < 110; i++ {
		items = append(items, compactGatewayApp(map[string]any{
			"install_id": i + 1, "app_id": i + 1000,
			"name": fmt.Sprintf("app-%03d", i), "display_name": fmt.Sprintf("App %03d", i),
			"version": "1.2.3", "available_version": "1.2.4",
			"project_id": "1779623058749-dcd3c4b280005d9b", "status": "running", "serving": true, "source": "registry",
			"description": strings.Repeat("Useful app capability. ", 20),
		}, false))
	}
	page, err := gatewayPage(items, map[string]any{"limit": 100}, "apps")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := gatewayMarshalToolResult(page)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(page["apps"].([]any)); got != 100 || page["next_offset"] != 100 {
		t.Fatalf("page size=%d next=%v", got, page["next_offset"])
	}
	if len(raw) >= 40*1024 {
		t.Fatalf("compact 100-app page is still too heavy: %d bytes", len(raw))
	}
}

func mustGatewayJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
