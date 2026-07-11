package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func versionedAppManifest(t *testing.T, name, version string, tools ...string) (sdk.Manifest, string) {
	t.Helper()
	mcpTools := make([]sdk.MCPToolSpec, 0, len(tools))
	for _, tool := range tools {
		mcpTools = append(mcpTools, sdk.MCPToolSpec{Name: tool, Description: version + " tool"})
	}
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: name, DisplayName: name + " " + version,
		Version: version, Provides: sdk.Provides{MCPTools: mcpTools},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, string(raw)
}

func TestProjectAppVersionsKeepIndependentRuntimeManifests(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.installedApps = NewInstalledAppsRegistry()
	s.catalog = NewAppCatalog()
	projectV1, err := s.store.CreateProject(1, "Project V1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	projectV2, err := s.store.CreateProject(1, "Project V2", "", "")
	if err != nil {
		t.Fatal(err)
	}

	_, v1JSON := versionedAppManifest(t, "reports", "1.0.0", "reports_v1")
	_, v2JSON := versionedAppManifest(t, "reports", "2.0.0", "reports_v2")
	_, v3JSON := versionedAppManifest(t, "reports", "3.0.0", "reports_v3")
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json)
		 VALUES ('reports', 'builtin', 'github.com/example/reports', 'main', ?)`, v2JSON)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	insertInstall := func(projectID, version, manifestJSON string) int64 {
		result, insertErr := s.store.db.Exec(
			`INSERT INTO app_installs
			 (app_id, project_id, status, version, manifest_json, source, repo, ref, permissions_json, installed_by)
			 VALUES (?, ?, 'running', ?, ?, 'git', 'github.com/example/reports', 'main', '[]', 1)`,
			appID, projectID, version, manifestJSON,
		)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		id, _ := result.LastInsertId()
		return id
	}
	installV1 := insertInstall(projectV1.ID, "1.0.0", v1JSON)
	installV2 := insertInstall(projectV2.ID, "2.0.0", v2JSON)

	s.LoadInstalledApps()
	if got := s.installedApps.Get(installV1); got == nil || got.Manifest.Version != "1.0.0" {
		t.Fatalf("project-v1 registry entry = %#v", got)
	}
	if got := s.installedApps.Get(installV2); got == nil || got.Manifest.Version != "2.0.0" {
		t.Fatalf("project-v2 registry entry = %#v", got)
	}
	if err := s.registerAppMCP(installV1); err != nil {
		t.Fatal(err)
	}
	if err := s.registerAppMCP(installV2); err != nil {
		t.Fatal(err)
	}
	assertInstallTools := func(installID int64, want, notWant string) {
		var toolsJSON string
		if err := s.store.db.QueryRow(`SELECT allowed_tools FROM mcp_servers WHERE upstream_id = ?`, appMCPUpstreamID(installID)).Scan(&toolsJSON); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(toolsJSON, want) || strings.Contains(toolsJSON, notWant) {
			t.Fatalf("install %d tools = %s, want %s without %s", installID, toolsJSON, want, notWant)
		}
	}
	assertInstallTools(installV1, "reports_v1", "reports_v2")
	assertInstallTools(installV2, "reports_v2", "reports_v1")

	req := httptest.NewRequest("GET", "/api/apps?project_id="+projectV1.ID, nil)
	req.Header.Set("X-User-ID", "1")
	recorder := httptest.NewRecorder()
	s.handleListApps(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("list apps status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows []AppRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].InstallID != installV1 || rows[0].Version != "1.0.0" || rows[0].AvailableVersion != "2.0.0" {
		t.Fatalf("project-v1 app rows = %#v", rows)
	}

	// Marketplace refreshes must not mutate either running install.
	if _, err := s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE id = ?`, v3JSON, appID); err != nil {
		t.Fatal(err)
	}
	s.LoadInstalledApps()
	if s.installedApps.Get(installV1).Manifest.Version != "1.0.0" || s.installedApps.Get(installV2).Manifest.Version != "2.0.0" {
		t.Fatalf("shared manifest leaked into project installs: v1=%s v2=%s",
			s.installedApps.Get(installV1).Manifest.Version, s.installedApps.Get(installV2).Manifest.Version)
	}
}

func TestAppCatalogMetadataDoesNotDowngradeForOlderProjectInstall(t *testing.T) {
	s := newTestServer(t)
	_, v2JSON := versionedAppManifest(t, "reports", "2.0.0")
	v1, _ := versionedAppManifest(t, "reports", "1.0.0")
	v3, _ := versionedAppManifest(t, "reports", "3.0.0")
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('reports', 'git', '', '', ?)`, v2JSON)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	s.updateAppCatalogMetadata(appID, &v1, "git", "", "")
	assertAppCatalogVersion(t, s, appID, "2.0.0")
	s.updateAppCatalogMetadata(appID, &v3, "git", "", "")
	assertAppCatalogVersion(t, s, appID, "3.0.0")

	versions := []sdk.Manifest{v1, v3}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		manifest := versions[i%len(versions)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.updateAppCatalogMetadata(appID, &manifest, "git", "", "")
		}()
	}
	wg.Wait()
	assertAppCatalogVersion(t, s, appID, "3.0.0")
}

func assertAppCatalogVersion(t *testing.T, s *Server, appID int64, want string) {
	t.Helper()
	var raw string
	if err := s.store.db.QueryRow(`SELECT manifest_json FROM apps WHERE id = ?`, appID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != want {
		t.Fatalf("catalog version = %s, want %s", manifest.Version, want)
	}
}

func TestAppInstallSnapshotMigrationBackfillsLegacyRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, manifestJSON := versionedAppManifest(t, "legacy", "1.4.0", "legacy_tool")
	res, err := store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json)
		 VALUES ('legacy', 'git', 'github.com/example/legacy', 'release', ?)`, manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	installResult, err := store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version) VALUES (?, 'project-a', 'running', '1.4.0')`, appID)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := installResult.LastInsertId()
	_ = store.Close()

	reopened, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var gotManifest, source, repo, ref string
	if err := reopened.db.QueryRow(
		`SELECT manifest_json, source, repo, ref FROM app_installs WHERE id = ?`, installID,
	).Scan(&gotManifest, &source, &repo, &ref); err != nil {
		t.Fatal(err)
	}
	if gotManifest != manifestJSON || source != "git" || repo != "github.com/example/legacy" || ref != "release" {
		t.Fatalf("backfill = manifest:%t source:%q repo:%q ref:%q", gotManifest == manifestJSON, source, repo, ref)
	}
}

func TestPruneUnreferencedAppVersionsKeepsEveryProjectVersion(t *testing.T) {
	s := newTestServer(t)
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	_, manifestJSON := versionedAppManifest(t, "reports", "2.0.0")
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, manifest_json) VALUES ('reports', 'git', ?)`, manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	for _, version := range []string{"1.0.0", "2.0.0"} {
		if _, err := s.store.db.Exec(
			`INSERT INTO app_installs (app_id, project_id, status, version, manifest_json)
			 VALUES (?, ?, 'running', ?, ?)`, appID, "project-"+version, version, manifestJSON,
		); err != nil {
			t.Fatal(err)
		}
	}
	appDir := filepath.Join(cache, "reports")
	versions := []string{"0.8.0", "0.9.0", "1.0.0", "2.0.0"}
	for i, version := range versions {
		path := filepath.Join(appDir, version)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i-len(versions)) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	s.pruneUnreferencedAppVersions("reports", "2.0.0")
	for _, active := range []string{"1.0.0", "2.0.0"} {
		if _, err := os.Stat(filepath.Join(appDir, active)); err != nil {
			t.Fatalf("active project version %s was pruned: %v", active, err)
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "0.8.0")); !os.IsNotExist(err) {
		t.Fatalf("old unreferenced version should be removed, err=%v", err)
	}
}
