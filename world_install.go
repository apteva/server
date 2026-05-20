package main

// world_install.go — install an app from a LOCAL working-copy directory.
//
// This is the World's app-seeding path. It reuses the PRODUCTION install
// machinery — the same app/app_installs rows, the same build→spawn→health
// tail (buildAndSpawn), the same installedApps registry — so an in-world app
// is a real, project-scoped install: its platform callbacks authenticate
// (real install id + token + permissions) and inter-app routing resolves
// through the registry. The only differences from a git install are (a) the
// source is a local dir built with a temp go.work so the developer's CURRENT
// code + local sibling modules (app-sdk) are used, and (b) everything is
// scoped to the given project id (the World id) and removed on teardown.
//
// It does NOT touch the production install path (installFromSource /
// handleInstallApp). Nothing here runs unless a World calls it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// localInstall is the result of installing an app from local source.
type localInstall struct {
	InstallID  int64
	AppName    string
	ProjectID  string
	Port       int
	SidecarURL string
	DataDir    string // <cacheDir>/<name>/data/<installID>
	DBPath     string // <DataDir>/app.db
}

// installLocalSource builds + installs the app whose source lives at srcDir,
// scoped to projectID, and returns its running coordinates. env is the spawn
// env the caller wants threaded to the sidecar (e.g. HTTP_PROXY=<edge>,
// APTEVA_WORLD_ID); installLocalSource fills in the platform identity vars.
func (s *Server) installLocalSource(srcDir, projectID string, env map[string]string, progress func(string)) (*localInstall, error) {
	if s.localApps == nil {
		return nil, fmt.Errorf("installLocalSource: local supervisor not configured")
	}
	if progress == nil {
		progress = func(string) {}
	}

	// 1. Parse the manifest from the working copy.
	yamlBytes, err := os.ReadFile(filepath.Join(srcDir, "apteva.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read apteva.yaml: %w", err)
	}
	m, err := sdk.ParseManifest(yamlBytes)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("manifest has no name")
	}

	// 2. Upsert the apps row (source=local).
	manifestJSON, _ := json.Marshal(m)
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, m.Name).Scan(&appID); err != nil {
		res, e := s.store.db.Exec(
			`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'local', '', '', ?)`,
			m.Name, string(manifestJSON))
		if e != nil {
			return nil, fmt.Errorf("create app row: %w", e)
		}
		appID, _ = res.LastInsertId()
	} else {
		_, _ = s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE id = ?`, string(manifestJSON), appID)
	}

	// 3. Create the install row, project-scoped, permissions from manifest.
	permsJSON, _ := json.Marshal(m.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, config_encrypted, status, upgrade_policy, version, permissions_json, installed_by, integration_bindings)
		 VALUES (?, ?, '', 'pending', 'manual', ?, ?, 0, '{}')`,
		appID, projectID, m.Version, string(permsJSON))
	if err != nil {
		return nil, fmt.Errorf("create install row: %w", err)
	}
	installID, _ := res.LastInsertId()

	// 4. Platform identity env (matches installFromSource). The install id
	//    + dev token are what make the sidecar's callbacks authenticate.
	if env == nil {
		env = map[string]string{}
	}
	env["APTEVA_GATEWAY_URL"] = s.localGatewayURL()
	env["APTEVA_APP_TOKEN"] = fmt.Sprintf("dev-%d", installID)
	env["APTEVA_INSTALL_ID"] = strconv.FormatInt(installID, 10)
	env["APTEVA_PROJECT_ID"] = projectID

	// 5. Build from local source with a temp go.work overlaying local app-sdk.
	goWork, cleanupGoWork, err := genLocalGoWork(srcDir)
	if err != nil {
		_, _ = s.store.db.Exec(`UPDATE app_installs SET status='error', error_message=? WHERE id=?`, err.Error(), installID)
		return nil, err
	}
	defer cleanupGoWork()

	port, binPath, err := s.localApps.BuildFromLocalSource(installID, m, srcDir, []string{"GOWORK=" + goWork}, env, progress)
	if err != nil {
		_, _ = s.store.db.Exec(`UPDATE app_installs SET status='error', error_message=? WHERE id=?`, err.Error(), installID)
		return nil, fmt.Errorf("build/spawn %s: %w", m.Name, err)
	}

	// 6. Persist running state.
	sidecarURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	_, _ = s.store.db.Exec(
		`UPDATE app_installs SET status='running', local_pid=?, local_bin_path=?, local_port=?, sidecar_url_override=?, status_message='', error_message='' WHERE id=?`,
		s.localApps.PID(installID), binPath, port, sidecarURL, installID)

	// 7. Register in the in-memory registry so project-scoped lookups
	//    (GetByNameAndProject) and inter-app CallApp routing resolve.
	s.LoadInstalledApps()

	// Data dir layout mirrors spawn(): <cacheDir>/<name>/data/<installID>.
	dataDir := filepath.Join(s.localApps.cacheDir, m.Name, "data", strconv.FormatInt(installID, 10))
	return &localInstall{
		InstallID:  installID,
		AppName:    m.Name,
		ProjectID:  projectID,
		Port:       port,
		SidecarURL: sidecarURL,
		DataDir:    dataDir,
		DBPath:     filepath.Join(dataDir, "app.db"),
	}, nil
}

// deleteWorldInstall removes the install + (orphaned) app rows for one
// in-world install. Guarded to project-scoped rows by the caller (World
// teardown passes only world-project installs) so it can never delete a
// production install.
func (s *Server) deleteWorldInstall(installID int64) {
	var appID int64
	_ = s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, installID).Scan(&appID)
	_ = s.localApps.Stop(installID)
	_, _ = s.store.db.Exec(`DELETE FROM app_installs WHERE id=?`, installID)
	// Drop the apps row only if no other install references it.
	if appID != 0 {
		var n int
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE app_id=?`, appID).Scan(&n)
		if n == 0 {
			_, _ = s.store.db.Exec(`DELETE FROM apps WHERE id=?`, appID)
		}
	}
}

// genLocalGoWork writes a throwaway go.work that overlays the app module +
// the local app-sdk (resolved from the workspace root above appDir), so a
// local-source build uses the working copy of app-sdk rather than the
// published pin in the app's go.mod. Returns the file path + a cleanup func.
func genLocalGoWork(appDir string) (path string, cleanup func(), err error) {
	noop := func() {}
	root := findWorkspaceRoot(appDir)
	if root == "" {
		return "", noop, fmt.Errorf("no workspace go.work found above %s", appDir)
	}
	appAbs, err := filepath.Abs(appDir)
	if err != nil {
		return "", noop, err
	}
	sdkAbs := filepath.Join(root, "app-sdk")
	if _, err := os.Stat(filepath.Join(sdkAbs, "go.mod")); err != nil {
		return "", noop, fmt.Errorf("app-sdk not found at %s", sdkAbs)
	}

	// Mirror the workspace's go directive so the temp workspace parses.
	goVer := "1.25"
	if data, e := os.ReadFile(filepath.Join(root, "go.work")); e == nil {
		for _, line := range strings.Split(string(data), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "go ") {
				goVer = strings.TrimSpace(strings.TrimPrefix(l, "go "))
				break
			}
		}
	}

	tmp, err := os.MkdirTemp("", "world-gowork-")
	if err != nil {
		return "", noop, err
	}
	content := fmt.Sprintf("go %s\n\nuse (\n\t%s\n\t%s\n)\n", goVer, appAbs, sdkAbs)
	p := filepath.Join(tmp, "go.work")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		os.RemoveAll(tmp)
		return "", noop, err
	}
	return p, func() { os.RemoveAll(tmp) }, nil
}

// findWorkspaceRoot walks up from start to the first dir containing go.work.
func findWorkspaceRoot(start string) string {
	d, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}
