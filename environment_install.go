package main

// environment_install.go — install an app from a LOCAL working-copy directory.
//
// This is the Environment's app-seeding path. It reuses the PRODUCTION install
// machinery — the same app/app_installs rows, the same build→spawn→health
// tail (buildAndSpawn), the same installedApps registry — so an in-environment app
// is a real, project-scoped install: its platform callbacks authenticate
// (real install id + token + permissions) and inter-app routing resolves
// through the registry. The only differences from a git install are (a) the
// source is a local dir built with a temp go.work so the developer's CURRENT
// code + local sibling modules (app-sdk) are used, and (b) everything is
// scoped to the given project id (the Environment id) and removed on teardown.
//
// It does NOT touch the production install path (installFromSource /
// handleInstallApp). Nothing here runs unless a Environment calls it.

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
// APTEVA_ENVIRONMENT_ID); installLocalSource fills in the platform identity vars.
func (s *Server) installLocalSource(srcDir, projectID string, creatorUserID int64, env map[string]string, restoredDataDir string, initialBindings map[string]any, progress func(string)) (*localInstall, error) {
	if s.localApps == nil {
		return nil, fmt.Errorf("installLocalSource: local supervisor not configured")
	}
	if creatorUserID <= 0 {
		return nil, fmt.Errorf("installLocalSource: authenticated creator required")
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

	// 2. Reuse the catalog row without changing it. Environment installs must
	// never rewrite source-project/global catalog metadata. When no catalog row
	// exists, create an explicitly environment-owned row that teardown may
	// remove after its final temporary install is gone.
	manifestJSON, _ := json.Marshal(m)
	var appID int64
	createdAppRow := false
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, m.Name).Scan(&appID); err != nil {
		res, e := s.store.db.Exec(
			`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'environment', '', '', ?)`,
			m.Name, string(manifestJSON))
		if e != nil {
			return nil, fmt.Errorf("create app row: %w", e)
		}
		appID, _ = res.LastInsertId()
		createdAppRow = true
	}

	// 3. Create the install row, project-scoped, permissions from manifest.
	permsJSON, _ := json.Marshal(m.Requires.Permissions)
	bindingsJSON, _ := json.Marshal(initialBindings)
	if initialBindings == nil {
		bindingsJSON = []byte("{}")
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs
		 (app_id, project_id, config_encrypted, status, upgrade_policy, version, manifest_json, source, repo, ref, permissions_json, installed_by, integration_bindings)
		 VALUES (?, ?, '', 'pending', 'manual', ?, ?, 'environment', '', '', ?, ?, ?)`,
		appID, projectID, m.Version, string(manifestJSON), string(permsJSON), creatorUserID, string(bindingsJSON))
	if err != nil {
		if createdAppRow {
			_, _ = s.store.db.Exec(`DELETE FROM apps WHERE id=? AND source='environment'`, appID)
		}
		return nil, fmt.Errorf("create install row: %w", err)
	}
	installID, _ := res.LastInsertId()
	dataDir := localInstallDataDir(s.localApps, m.Name, installID)
	committed := false
	defer func() {
		if committed {
			return
		}
		// The Environment only records an install after this function returns.
		// Roll back here so failures at any later stage (credential creation,
		// compile, spawn, restore, restart, or DB persistence) cannot leave an
		// install row/process/port reservation that Environment.Stop cannot see.
		s.deleteEnvironmentInstall(installID)
	}()

	// 4. Platform identity env (matches installFromSource). The random
	//    per-install capability authenticates sidecar callbacks.
	if env == nil {
		env = map[string]string{}
	}
	env["APTEVA_GATEWAY_URL"] = s.localGatewayURL()
	appToken, err := s.appInstallToken(installID)
	if err != nil {
		return nil, fmt.Errorf("create app credential: %w", err)
	}
	env["APTEVA_APP_TOKEN"] = appToken
	env["APTEVA_INSTALL_ID"] = strconv.FormatInt(installID, 10)
	env["APTEVA_PROJECT_ID"] = projectID

	// 5. Build from local source with a temp go.work overlaying local app-sdk.
	goWork, cleanupGoWork, err := genLocalGoWork(srcDir)
	if err != nil {
		return nil, err
	}
	defer cleanupGoWork()

	port, binPath, err := s.localApps.BuildFromLocalSource(installID, m, srcDir, []string{"GOWORK=" + goWork}, env, progress)
	if err != nil {
		return nil, fmt.Errorf("build/spawn %s: %w", m.Name, err)
	}

	// 6. A restored database must be in place before the app opens it. The
	// build path starts the sidecar to verify the binary, so stop it, replace
	// its fresh data directory, and restart with the same runtime environment.
	sidecarURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if restoredDataDir = strings.TrimSpace(restoredDataDir); restoredDataDir != "" {
		if err := s.localApps.Stop(installID); err != nil {
			return nil, fmt.Errorf("stop %s before restore: %w", m.Name, err)
		}
		if err := os.RemoveAll(dataDir); err != nil {
			return nil, fmt.Errorf("clear %s data before restore: %w", m.Name, err)
		}
		if err := copyTree(restoredDataDir, dataDir); err != nil {
			return nil, fmt.Errorf("restore %s data: %w", m.Name, err)
		}
		if err := s.localApps.Restart(installID, m, port, binPath, env); err != nil {
			return nil, fmt.Errorf("restart %s after restore: %w", m.Name, err)
		}
	}

	// 7. Persist running state.
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET status='running', local_pid=?, local_bin_path=?, local_port=?, sidecar_url_override=?, status_message='', error_message='' WHERE id=?`,
		s.localApps.PID(installID), binPath, port, sidecarURL, installID); err != nil {
		return nil, fmt.Errorf("persist running install %s: %w", m.Name, err)
	}

	// 8. Register in the in-memory registry so project-scoped lookups
	//    (GetByNameAndProject) and inter-app CallApp routing resolve.
	s.LoadInstalledApps()
	committed = true

	// Data dir layout mirrors spawn(): persistentRoot is under the canonical
	// app cache root, independent of the local build binary's _local path —
	//   <cacheDir>/<appName>/data/<installID>/app.db
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

func localInstallDataDir(supervisor *LocalSupervisor, appName string, installID int64) string {
	return filepath.Join(supervisor.cacheDir, appName, "data", strconv.FormatInt(installID, 10))
}

// deleteEnvironmentInstall removes the install + (orphaned) app rows for one
// in-environment install. Guarded to project-scoped rows by the caller (Environment
// teardown passes only environment-project installs) so it can never delete a
// production install.
func (s *Server) deleteEnvironmentInstall(installID int64) {
	var appID int64
	var appName, appSource string
	_ = s.store.db.QueryRow(`SELECT i.app_id, a.name, COALESCE(a.source,'')
		FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID).Scan(&appID, &appName, &appSource)
	_ = s.localApps.Stop(installID)
	s.localApps.ReleaseFixedPorts(installID)
	if s.installedApps != nil {
		s.installedApps.Remove(installID)
	}
	_, _ = s.store.db.Exec(`DELETE FROM app_installs WHERE id=?`, installID)
	// Drop the apps row only if no other install references it.
	if appID != 0 && appSource == "environment" {
		var n int
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE app_id=?`, appID).Scan(&n)
		if n == 0 {
			_, _ = s.store.db.Exec(`DELETE FROM apps WHERE id=?`, appID)
		}
	}
	if appName != "" {
		_ = os.RemoveAll(localInstallDataDir(s.localApps, appName, installID))
	}
}

// genLocalGoWork writes a throwaway go.work that overlays the app module +
// the local app-sdk (resolved from the workspace root above appDir), so a
// local-source build uses the working copy of app-sdk rather than the
// published pin in the app's go.mod. Returns the file path + a cleanup func.
func genLocalGoWork(appDir string) (path string, cleanup func(), err error) {
	noop := func() {}
	root := findWorkspaceRoot(appDir)
	appAbs, err := filepath.Abs(appDir)
	if err != nil {
		return "", noop, err
	}
	// Go compares workspace members against the canonical module directory.
	// On macOS, temporary paths commonly arrive as /var/... while getcwd and
	// the Go command resolve them to /private/var/.... Keep the generated
	// go.work on the same canonical path so a listed module is not rejected as
	// "not one of the workspace modules".
	if canonical, evalErr := filepath.EvalSymlinks(appAbs); evalErr == nil {
		appAbs = canonical
	}
	sdkAbs := ""
	if root != "" {
		candidate := filepath.Join(root, "app-sdk")
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			sdkAbs = candidate
		}
	}
	if sdkAbs == "" {
		sdkAbs = findLocalAppSDKDir(appAbs)
	}
	if sdkAbs != "" {
		if canonical, evalErr := filepath.EvalSymlinks(sdkAbs); evalErr == nil {
			sdkAbs = canonical
		}
	}

	// Derive the go directive from the modules this workspace actually
	// contains, not from the repo's root go.work.
	//
	// The generated workspace holds exactly the app and the local app-sdk, so
	// its directive only has to satisfy those two. Copying the root value
	// (currently 1.26.6) pinned it to the running toolchain's own version and
	// forced a needless toolchain switch; taking it from the app alone put it
	// BELOW app-sdk's own requirement, which go rejects outright with
	// "module app-sdk ... requires go >= X, but go.work lists go Y".
	//
	// A workspace directive must be >= every member's, so take the highest of
	// the two rather than either one on its own.
	goVer := goModGoDirective(appAbs)
	if sdkAbs != "" {
		goVer = higherGoDirective(goVer, goModGoDirective(sdkAbs))
	}
	if goVer == "" && root != "" {
		// No member declared one: fall back to the repo workspace so the
		// generated file at least parses.
		if data, e := os.ReadFile(filepath.Join(root, "go.work")); e == nil {
			for _, line := range strings.Split(string(data), "\n") {
				l := strings.TrimSpace(line)
				if strings.HasPrefix(l, "go ") {
					goVer = strings.TrimSpace(strings.TrimPrefix(l, "go "))
					break
				}
			}
		}
	}
	if goVer == "" {
		goVer = "1.25"
	}

	tmp, err := os.MkdirTemp("", "environment-gowork-")
	if err != nil {
		return "", noop, err
	}
	uses := []string{appAbs}
	if sdkAbs != "" {
		uses = append(uses, sdkAbs)
	}
	content := fmt.Sprintf("go %s\n\nuse (\n\t%s\n)\n", goVer, strings.Join(uses, "\n\t"))
	p := filepath.Join(tmp, "go.work")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		os.RemoveAll(tmp)
		return "", noop, err
	}
	return p, func() { os.RemoveAll(tmp) }, nil
}

// goModGoDirective returns the `go` version declared by dir's go.mod, or ""
// when the file is unreadable or declares none.
func goModGoDirective(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "go ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "go "))
		}
	}
	return ""
}

// higherGoDirective returns whichever of a and b is the later Go version,
// comparing dotted components numerically so 1.25.12 sorts above 1.25.2.
func higherGoDirective(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if compareGoDirective(a, b) >= 0 {
		return a
	}
	return b
}

func compareGoDirective(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) || i < len(bParts); i++ {
		av, bv := 0, 0
		if i < len(aParts) {
			av, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bv, _ = strconv.Atoi(bParts[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

func findLocalAppSDKDir(start string) string {
	candidates := []string{}
	if root := findWorkspaceRoot("."); root != "" {
		candidates = append(candidates, filepath.Join(root, "app-sdk"))
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "..", "app-sdk"),
			filepath.Join(exeDir, "..", "..", "app-sdk"),
		)
	}
	for d := start; ; d = filepath.Dir(d) {
		candidates = append(candidates, filepath.Join(d, "app-sdk"))
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil && !fi.IsDir() {
			if abs, aerr := filepath.Abs(candidate); aerr == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
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
