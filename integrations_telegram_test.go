package main

import (
	"encoding/json"
	"testing"
)

func TestTelegramCatalogExposesSetMyName(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/telegram.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	for _, tool := range app.Tools {
		if tool.Name == "set_my_name" {
			if tool.Method != "POST" || tool.Path != "/setMyName" {
				t.Fatalf("set_my_name = %s %s, want POST /setMyName", tool.Method, tool.Path)
			}
			return
		}
	}
	t.Fatal("set_my_name is missing")
}
