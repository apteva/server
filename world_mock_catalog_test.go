package main

import (
	"encoding/json"
	"testing"
)

// TestCatalogMockResponses_Parse asserts the curated mock_response shapes ship
// in the embedded catalog and parse into AppToolDef.MockResponse for the
// common integrations an agent reaches in a World. These are what
// executeIntegrationTool serves (instead of hitting the real API) when an
// agent runs inside a World — so they must load and be valid JSON objects.
//
// Not gated: pure catalog parse, no core/LLM.
func TestCatalogMockResponses_Parse(t *testing.T) {
	cat := NewAppCatalog()
	if err := cat.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// app -> tools that must carry a mock_response.
	want := map[string][]string{
		"slack":   {"send_message", "create_channel", "list_channels", "upload_file", "add_reaction"},
		"discord": {"send_message", "send_embed", "create_channel", "get_messages"},
		"github":  {"create_issue", "add_issue_comment", "create_pull", "merge_pull", "get_authenticated_user", "list_issues"},
		"gmail":   {"send_email", "create_draft", "list_messages", "get_message", "create_label"},
		"twilio":  {"send_sms", "send_whatsapp", "make_call", "get_balance", "lookup_phone_number"},
	}

	for app, tools := range want {
		tmpl := cat.Get(app)
		if tmpl == nil {
			t.Errorf("%s: not in catalog", app)
			continue
		}
		byName := map[string]AppToolDef{}
		for _, td := range tmpl.Tools {
			byName[td.Name] = td
		}
		for _, name := range tools {
			td, ok := byName[name]
			if !ok {
				t.Errorf("%s.%s: tool missing from catalog", app, name)
				continue
			}
			if len(td.MockResponse) == 0 {
				t.Errorf("%s.%s: no mock_response", app, name)
				continue
			}
			// Must be valid JSON (object or array — both are real API shapes).
			var v any
			if err := json.Unmarshal(td.MockResponse, &v); err != nil {
				t.Errorf("%s.%s: mock_response is not valid JSON: %v", app, name, err)
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
			default:
				t.Errorf("%s.%s: mock_response should be an object or array, got %T", app, name, v)
			}
		}
	}
}
