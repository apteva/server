package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type resourceFixtureApp struct{}

func (*resourceFixtureApp) Manifest() sdk.Manifest {
	raw, err := os.ReadFile(os.Getenv("RESOURCE_FIXTURE_MANIFEST"))
	if err != nil {
		panic(err)
	}
	var m sdk.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	return m
}
func (*resourceFixtureApp) OnMount(*sdk.AppCtx) error         { return nil }
func (*resourceFixtureApp) OnUnmount(*sdk.AppCtx) error       { return nil }
func (*resourceFixtureApp) HTTPRoutes() []sdk.Route           { return nil }
func (*resourceFixtureApp) MCPTools() []sdk.Tool              { return nil }
func (*resourceFixtureApp) Channels() []sdk.ChannelFactory    { return nil }
func (*resourceFixtureApp) Workers() []sdk.Worker             { return nil }
func (*resourceFixtureApp) EventHandlers() []sdk.EventHandler { return nil }
func TestResourceSidecarHelper(t *testing.T) {
	if os.Getenv("RESOURCE_FIXTURE_MANIFEST") == "" {
		return
	}
	sdk.Run(&resourceFixtureApp{})
}
func resourceFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}
func TestPackagedResourcesSurviveLocalLifecycle(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.port = "1"
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	s.installedApps = NewInstalledAppsRegistry()
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := sdk.Manifest{Schema: sdk.SchemaCurrent, ConfigSchema: []sdk.ConfigField{{Name: "label", Type: "string"}}, Name: "resource-fixture", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "source", Port: 8080, Source: &sdk.SourceSpec{Repo: "https://example.invalid/fixture", Ref: "v1", Entry: "mcp/fixture"}}, DB: &sdk.DBConfig{Driver: "sqlite", Path: "app.db", Migrations: "migrations"}, Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{Name: "panel", Entry: "/ui/panel.mjs", Slots: []string{"dashboard.home"}}}}}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	raw, _ := json.Marshal(m)
	resourceFile(t, manifest, string(raw))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A real SDK sidecar serves /ui only if the injected directory exists at boot.
	wrapper := "#!/bin/sh\nexport RESOURCE_FIXTURE_MANIFEST='" + manifest + "'\nexec '" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' -test.run='^TestResourceSidecarHelper$'\n"
	var bin string
	for _, digest := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		path := filepath.Join(cache, m.Name, m.Version, "packages", digest, "bin")
		resourceFile(t, path, wrapper)
		root := artifactAppRoot(&m, path)
		resourceFile(t, filepath.Join(root, "ui/panel.mjs"), "export default '"+digest+"'")
		resourceFile(t, filepath.Join(root, "migrations/001_records.sql"), "CREATE TABLE records(id TEXT PRIMARY KEY,value TEXT);")
		if bin == "" {
			bin = path
		}
	}
	if _, err := os.Lstat(filepath.Join(cache, m.Name, m.Version, "src")); !os.IsNotExist(err) {
		t.Fatal("test must have no compatibility symlink")
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET local_bin_path=? WHERE id=?`, bin, id); err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(id)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"APTEVA_INSTALL_ID": strconv.FormatInt(id, 10), "APTEVA_APP_TOKEN": token, "APTEVA_GATEWAY_URL": "http://127.0.0.1:1"}
	port, err := s.localApps.startBuiltSource(id, &m, bin, env, nil)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "stderr.log"))
		t.Fatalf("%v: %s", err, log)
	}
	if err := s.localApps.persistReadyProcess(id, port, func(pid int) error { return s.updateLocalInstallRuntime(id, pid, int64(port)) }); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(cache, m.Name, "data", strconv.FormatInt(id, 10), "app.db")
	appDB, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	if _, err := appDB.Exec(`INSERT INTO records VALUES('saved','keep')`); err != nil {
		t.Fatal(err)
	}
	layout := json.RawMessage(`{"projects":{"p":{"pages":[{"id":"saved-page","title":"Tasks"}],"surfaces":{"page.saved-page":[{"id":"widget","component":"resource-fixture:panel","settings":{"filter":"mine"}}]}}}}`)
	if err := s.store.SetUserUILayout(1, layout); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		p := s.localApps.currentProc(id)
		if p == nil {
			t.Fatal("missing process")
		}
		assertEnvValue(t, p.cmd.Env, "APTEVA_UI_DIR", filepath.Join(artifactAppRoot(&m, bin), "ui"))
		assertEnvValue(t, p.cmd.Env, "APTEVA_MIGRATIONS_DIR", filepath.Join(artifactAppRoot(&m, bin), "migrations"))
		assertEnvValue(t, p.cmd.Env, "DB_PATH", database)
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/ui/panel.mjs", p.port), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), strings.Repeat("a", 64)) {
			t.Fatalf("wrong package UI: %d %s", resp.StatusCode, body)
		}
		var value string
		if err := appDB.QueryRow(`SELECT value FROM records WHERE id='saved'`).Scan(&value); err != nil || value != "keep" {
			t.Fatalf("records lost: %q %v", value, err)
		}
		current, _ := s.store.GetUserUILayoutWithRevision(1)
		if !bytes.Equal(current, layout) {
			t.Fatal("saved page/widget settings changed")
		}
		var selected string
		if err := s.store.db.QueryRow(`SELECT local_bin_path FROM app_installs WHERE id=?`, id).Scan(&selected); err != nil || selected != bin {
			t.Fatal("installation/package identity changed")
		}
	}
	check()
	// Recreate the supervisor and registry exactly as a server boot does.
	if err := s.localApps.Stop(id); err != nil {
		t.Fatal(err)
	}
	s.localApps = NewLocalSupervisor(cache)
	s.installedApps = NewInstalledAppsRegistry()
	s.resumeOneLocalInstall(id, 0, int64(port), bin, m.Name, "", "", m.Version, string(raw))
	check()
	for _, operation := range []struct {
		suffix, body string
		handler      http.HandlerFunc
	}{
		{"config", `{"config":{"label":"kept"}}`, s.handleSetInstallConfig},
		{"bindings", `{}`, s.handleSetInstallBindings2},
		{"status", `{"status":"disabled"}`, s.handleSetInstallStatus},
		{"status", `{"status":"running"}`, s.handleSetInstallStatus},
	} {
		previousPID := s.localApps.PID(id)
		req := httptest.NewRequest("PUT", fmt.Sprintf("/apps/installs/%d/%s", id, operation.suffix), strings.NewReader(operation.body))
		req.Header.Set("X-User-ID", "1")
		w := httptest.NewRecorder()
		operation.handler(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", operation.suffix, w.Code, w.Body.String())
		}
		if !strings.Contains(operation.body, "disabled") {
			check()
			if s.localApps.PID(id) == previousPID {
				t.Fatalf("%s returned success without respawning", operation.suffix)
			}
		}

	}
}

func TestResourcePathsValidationAndPortability(t *testing.T) {
	for _, layout := range []string{"legacy", "packages", "artifacts"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			suffix := filepath.Join("app", "1.0.0")
			if layout != "legacy" {
				suffix = filepath.Join(suffix, layout, strings.Repeat("a", 64))
			}
			bin := filepath.Join(root, suffix, "bin")
			resourceFile(t, bin, "bin")
			old := filepath.Join("/old-cache", suffix, "bin")
			if got, ok := portableLocalBinPath(old, root, "app", "1.0.0"); !ok || got != bin {
				t.Fatalf("rebase: %s %v", got, ok)
			}
			if layout == "packages" {
				other := filepath.Join(root, "app", "1.0.0", "packages", strings.Repeat("b", 64), "bin")
				resourceFile(t, other, "other")
				missing := filepath.Join("/old-cache", "app", "1.0.0", "packages", strings.Repeat("c", 64), "bin")
				if _, ok := portableLocalBinPath(missing, root, "app", "1.0.0"); ok {
					t.Fatal("selected another package digest")
				}
			}
			m := sdk.Manifest{Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Entry: "mcp/app"}}, DB: &sdk.DBConfig{Migrations: "migrations"}, Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{Entry: "/ui/panel.mjs"}}}}
			appRoot := artifactAppRoot(&m, bin)
			resourceFile(t, filepath.Join(appRoot, "ui/panel.mjs"), "module")
			os.MkdirAll(filepath.Join(appRoot, "migrations"), 0755)
			env := map[string]string{}
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err != nil {
				t.Fatal(err)
			}
			if env["APTEVA_UI_DIR"] != filepath.Join(appRoot, "ui") || env["APTEVA_MIGRATIONS_DIR"] != filepath.Join(appRoot, "migrations") {
				t.Fatal(env)
			}
			os.Remove(filepath.Join(appRoot, "ui/panel.mjs"))
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err == nil || !strings.Contains(err.Error(), "72") || !strings.Contains(err.Error(), "panel.mjs") {
				t.Fatalf("missing asset error: %v", err)
			}
			m.Provides = sdk.Provides{}
			os.RemoveAll(filepath.Join(appRoot, "migrations"))
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err == nil || !strings.Contains(err.Error(), "migrations") {
				t.Fatalf("missing migration error: %v", err)
			}
			m.DB = &sdk.DBConfig{Migrations: t.TempDir()}
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err != nil || env["APTEVA_MIGRATIONS_DIR"] != m.DB.Migrations {
				t.Fatalf("absolute migration: %v", err)
			}
			m.DB = nil
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err != nil {
				t.Fatalf("headless app: %v", err)
			}
			m.Runtime.Source.Entry = "../escape"
			if err := applyAppResourceEnv(72, &m, bin, "", "", env); err == nil {
				t.Fatal("unsafe source entry accepted")
			}
		})
	}
}

func TestWorkingCopyResourcesUseExplicitModuleDirectory(t *testing.T) {
	root := t.TempDir()
	resourceFile(t, filepath.Join(root, "ui/panel.mjs"), "module")
	if err := os.MkdirAll(filepath.Join(root, "migrations"), 0755); err != nil {
		t.Fatal(err)
	}
	m := sdk.Manifest{Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Entry: "mcp/app"}}, DB: &sdk.DBConfig{Migrations: "migrations"}, Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{Entry: "/ui/panel.mjs"}}}}
	env := map[string]string{}
	if err := applyAppResourceEnv(73, &m, filepath.Join(t.TempDir(), "bin"), root, ".", env); err != nil {
		t.Fatal(err)
	}
	if env["APTEVA_UI_DIR"] != filepath.Join(root, "ui") || env["APTEVA_MIGRATIONS_DIR"] != filepath.Join(root, "migrations") {
		t.Fatal(env)
	}
}
