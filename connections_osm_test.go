package main

import (
	"strings"
	"testing"
)

func TestNominatimRejectsPublicEndpoint(t *testing.T) {
	app := &AppTemplate{Slug: "openstreetmap-nominatim"}
	tool := &AppToolDef{Name: "search_place"}
	_, err := executeIntegrationTool(app, tool, map[string]string{"nominatim_base_url": "https://nominatim.openstreetmap.org"}, map[string]any{"q": "Baba Nahm"}, "")
	if err == nil || !strings.Contains(err.Error(), "public endpoint") {
		t.Fatalf("expected public endpoint rejection, got %v", err)
	}
}
