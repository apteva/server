package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestFetchManifestBytesAcceptsAgentPluginURL(t *testing.T) {
	const nativeManifest = `schema: apteva-app/v1
name: crm
display_name: CRM
version: 0.8.24
runtime:
  kind: source
  source:
    repo: github.com/apteva/apps
    ref: main
    entry: mcp/crm
  port: 8080
upgrade_policy: manual
`
	mux := http.NewServeMux()
	mux.HandleFunc("/crm/plugin.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name":"crm",
			"version":"0.8.24",
			"extensions":{"com.apteva":{"manifest":"apteva.yaml"}}
		}`))
	})
	mux.HandleFunc("/crm/apteva.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nativeManifest))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	s := newTestServer(t)
	document, err := s.fetchManifestBytes(upstream.URL+"/crm/plugin.json", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := sdk.ParseManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "crm" || manifest.Version != "0.8.24" {
		t.Fatalf("manifest=%+v", manifest)
	}

	direct, err := s.fetchManifestBytes(upstream.URL+"/crm/apteva.yaml", "")
	if err != nil || string(direct) != nativeManifest {
		t.Fatalf("legacy manifest changed: err=%v body=%q", err, direct)
	}
}

func TestAgentPluginURLRejectsMissingExtensionAndEscapes(t *testing.T) {
	s := newTestServer(t)
	missing := []byte(`{
		"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name":"crm"
	}`)
	if _, err := s.resolveAgentPluginManifestDocument("https://example.test/crm/plugin.json", missing); err == nil || !strings.Contains(err.Error(), "no com.apteva.manifest") {
		t.Fatalf("missing extension error=%v", err)
	}
	escaping := []byte(`{
		"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name":"crm",
		"extensions":{"com.apteva":{"manifest":"../apteva.yaml"}}
	}`)
	if _, err := s.resolveAgentPluginManifestDocument("https://example.test/crm/plugin.json", escaping); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping extension error=%v", err)
	}
	if _, err := resolvePluginSiblingURL("https://example.test/crm/plugin.json", "%2e%2e/apteva.yaml"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("encoded escaping extension error=%v", err)
	}
}

func TestSyncAgentPluginSkillsDiscoversPortableSkill(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "crm", "project-1", nil)
	root := t.TempDir()
	writeServerTestFile(t, filepath.Join(root, "plugin.json"), `{
		"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name":"crm",
		"version":"0.1.0",
		"extensions":{"com.apteva":{"manifest":"apteva.yaml"}}
	}`)
	writeServerTestFile(t, filepath.Join(root, "apteva.yaml"), "schema: apteva-app/v1\nname: crm\nversion: 0.1.0\n")
	writeServerTestFile(t, filepath.Join(root, "skills", "crm", "SKILL.md"), `---
name: crm
description: Use CRM tools for contacts, customer conversations, lists, leads, opportunities, and pipeline questions.
metadata:
  author: apteva
---
Read before writing and use the narrowest CRM tool.`)
	writeServerTestFile(t, filepath.Join(root, "skills", "invalid", "SKILL.md"), "not a skill")

	manifest := &sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "crm", DisplayName: "CRM", Version: "0.1.0",
	}
	if err := s.syncAgentPluginSkillsForInstall(installID, manifest, "project-1", root); err != nil {
		t.Fatal(err)
	}
	var slug, description, body, metadataJSON string
	if err := s.store.db.QueryRow(`SELECT slug, description, body, metadata_json FROM skills WHERE install_id=?`, installID).
		Scan(&slug, &description, &body, &metadataJSON); err != nil {
		t.Fatal(err)
	}
	if slug != "crm:crm" || !strings.Contains(description, "pipeline") || !strings.Contains(body, "Read before writing") {
		t.Fatalf("skill slug=%q description=%q body=%q", slug, description, body)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil || metadata["author"] != "apteva" {
		t.Fatalf("metadata=%v err=%v", metadata, err)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM skills WHERE install_id=?`, installID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("skill count=%d err=%v", count, err)
	}
}

func TestSyncAgentPluginSkillsKeepsNativeDeclarationOnDuplicate(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "crm", "project-1", nil)
	root := t.TempDir()
	writeServerTestFile(t, filepath.Join(root, "plugin.json"), `{
		"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name":"crm",
		"version":"0.1.0",
		"extensions":{"com.apteva":{"manifest":"apteva.yaml"}}
	}`)
	writeServerTestFile(t, filepath.Join(root, "apteva.yaml"), "schema: apteva-app/v1\nname: crm\nversion: 0.1.0\n")
	writeServerTestFile(t, filepath.Join(root, "skills", "crm", "SKILL.md"), `---
name: crm
description: Portable description.
---
Portable body.`)
	manifest := &sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "crm", Version: "0.1.0",
		Provides: sdk.Provides{Skills: []sdk.Skill{{
			Name: "crm", Description: "Native description", Body: "Native body.",
		}}},
	}
	if err := s.syncAgentPluginSkillsForInstall(installID, manifest, "project-1", root); err != nil {
		t.Fatal(err)
	}
	var description, body string
	if err := s.store.db.QueryRow(`SELECT description, body FROM skills WHERE install_id=?`, installID).Scan(&description, &body); err != nil {
		t.Fatal(err)
	}
	if description != "Native description" || body != "Native body." {
		t.Fatalf("native declaration was replaced: description=%q body=%q", description, body)
	}
}

func TestSyncAgentPluginSkillsKeepsNativeSkillsWhenPluginIsInvalid(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "crm", "project-1", nil)
	root := t.TempDir()
	writeServerTestFile(t, filepath.Join(root, "plugin.json"), `{"name":"broken"}`)
	manifest := &sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "crm", Version: "0.1.0",
		Provides: sdk.Provides{Skills: []sdk.Skill{{
			Name: "crm", Description: "Native description", Body: "Native body.",
		}}},
	}
	err := s.syncAgentPluginSkillsForInstall(installID, manifest, "project-1", root)
	if err == nil {
		t.Fatal("invalid compatibility package should be reported")
	}
	var description, body string
	if queryErr := s.store.db.QueryRow(`SELECT description, body FROM skills WHERE install_id=?`, installID).Scan(&description, &body); queryErr != nil {
		t.Fatal(queryErr)
	}
	if description != "Native description" || body != "Native body." {
		t.Fatalf("native skill not preserved: description=%q body=%q", description, body)
	}
}

func writeServerTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
