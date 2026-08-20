package main

import (
	"encoding/json"
	"testing"
)

func TestTelegramCatalogExposesConversationOperations(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/telegram.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range app.Tools {
		if tool.Name == "set_my_name" {
			if tool.Method != "POST" || tool.Path != "/setMyName" {
				t.Fatalf("set_my_name = %s %s, want POST /setMyName", tool.Method, tool.Path)
			}
			found[tool.Name] = true
		}
		if tool.Name == "send_message_draft" {
			if tool.Method != "POST" || tool.Path != "/sendMessageDraft" {
				t.Fatalf("send_message_draft = %s %s, want POST /sendMessageDraft", tool.Method, tool.Path)
			}
			found[tool.Name] = true
		}
	}
	for _, name := range []string{"set_my_name", "send_message_draft"} {
		if !found[name] {
			t.Fatalf("%s is missing", name)
		}
	}
}
