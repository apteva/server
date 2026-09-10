package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

	sdk "github.com/apteva/app-sdk"
)

func TestBackupProxyRequiresPlatformAdminDespiteProjectEditor(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	calls := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 707, AppName: "backup", SidecarURL: sidecar.URL})
	for _, path := range []string{"/runs", "/restore", "/mcp", "/destinations"} {
		method := http.MethodPost
		if path == "/runs" {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, "/apps/backup"+path+"?install_id=707&project_id="+projectID, strings.NewReader(`{"run_id":1,"confirm":true}`))
		req.Header.Set("X-User-ID", itoa64(userID))
		w := httptest.NewRecorder()
		s.handleAppProxy(w, req)
		if w.Code != 403 {
			t.Fatalf("%s status=%d %s", path, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("non-admin reached backup sidecar")
	}
	req := httptest.NewRequest(http.MethodPost, "/apps/backup/restore?install_id=707&project_id="+projectID, strings.NewReader(`{"run_id":1,"confirm":true}`))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleAppProxy(w, req)
	if w.Code != 204 || calls != 1 {
		t.Fatalf("admin blocked: %d %s", w.Code, w.Body.String())
	}
	if !manifestHasPlatformBackupPermission(sdk.Manifest{Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermPlatformBackupRead}}}) {
		t.Fatal("backup permission unrecognized")
	}
}

func TestRecoveryExcludesBackupArchivesAndPreservesLocalCopies(t *testing.T) {
	source := newTestServer(t)
	source.dataDir = t.TempDir()
	source.localApps = NewLocalSupervisor(t.TempDir())
	id := insertTestInstall(t, source, "backup")
	root := filepath.Join(source.localApps.cacheDir, "backup", "data", strconv.FormatInt(id, 10))
	if err := os.MkdirAll(filepath.Join(root, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	mustSeedSqlite(t, filepath.Join(root, "app.db"))
	if err := os.WriteFile(filepath.Join(root, "backups", "old.tar.gz"), bytes.Repeat([]byte("old backup"), 1000), 0600); err != nil {
		t.Fatal(err)
	}
	// A custom marked folder must be excluded as well as the legacy default.
	if err := os.MkdirAll(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "custom", ".apteva-backup-destination"), nil, 0600)
	os.WriteFile(filepath.Join(root, "custom", "another.tar.gz"), []byte("custom backup"), 0600)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Backup-Passphrase", "portable recovery passphrase")
	w := httptest.NewRecorder()
	source.writePlatformSnapshot(w, request)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var manifest recoveryManifest
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "/backups/") || strings.Contains(h.Name, "/custom/") {
			t.Fatal("archive recursively included:", h.Name)
		}
		if h.Name == "manifest.json" {
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	gz.Close()
	key := fmt.Sprintf("apps/%d-backup", id)
	if len(manifest.Excluded[key]) != 2 || manifest.KeyPolicy != "passphrase_wrapped" {
		t.Fatalf("manifest exclusions/recovery: %+v", manifest)
	}
	dest := newTestServer(t)
	dest.dataDir = t.TempDir()
	dest.dbPath = dest.store.path
	dest.secret = bytes.Repeat([]byte{0x76}, 32)
	dest.localApps = NewLocalSupervisor(t.TempDir())
	destRoot := filepath.Join(dest.localApps.cacheDir, "backup", "data", strconv.FormatInt(id, 10))
	os.MkdirAll(filepath.Join(destRoot, "backups"), 0700)
	archivePath := filepath.Join(destRoot, "backups", "keep.tar.gz")
	os.WriteFile(archivePath, []byte("keep current archives"), 0600)
	restore := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(w.Body.Bytes()))
	restore.Header.Set("X-Backup-Passphrase", "portable recovery passphrase")
	out := httptest.NewRecorder()
	dest.restorePlatformSnapshot(out, restore)
	if out.Code != 200 {
		t.Fatalf("stage: %d %s", out.Code, out.Body.String())
	}
	dest.store.Close()
	if err := applyPendingRecovery(dest.dbPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil || string(data) != "keep current archives" {
		t.Fatalf("backup storage lost: %q %v", data, err)
	}
	if err := validateRecoveryDB(filepath.Join(destRoot, "app.db")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryExclusionRejectsSymlinkAncestor(t *testing.T) {
	root, target, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := preserveRecoveryDirectory(root, target, "link/archives"); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	for _, path := range []string{".", "..", "../outside", "/absolute", "a/../b"} {
		if safeRecoveryRelativePath(path) {
			t.Fatal("unsafe exclusion accepted:", path)
		}
	}
}

func TestBackupAppCallsOnlyAdmitScheduledPolicies(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"accepted"}`)
	}))
	defer sidecar.Close()
	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "backup"}
	targetID := seedInstallWithBindings(t, s, "backup", targetManifest, nil)
	s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, targetID)
	s.installedApps.Add(&InstalledApp{InstallID: targetID, AppName: "backup", Manifest: targetManifest, SidecarURL: sidecar.URL})
	callerManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "jobs", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: "backup"}}}}
	callerID := seedInstallWithBindings(t, s, "jobs", callerManifest, map[string]any{"backup": targetID})
	s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, callerID)
	s.installedApps.Add(&InstalledApp{InstallID: callerID, AppName: "jobs", Manifest: callerManifest, SidecarURL: sidecar.URL})
	call := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/backup/call", strings.NewReader(body))
		r.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
		w := httptest.NewRecorder()
		s.handleAppCallback(w, r)
		return w
	}
	for _, body := range []string{`{"tool":"backup_restore","input":{"run_id":1,"confirm":true}}`, `{"tool":"backup_now","input":{"destination_id":1}}}`} {
		if w := call(body); w.Code != 403 {
			t.Fatalf("unsafe call=%d %s", w.Code, w.Body.String())
		}
	}
	if w := call(`{"tool":"backup_now","input":{"policy_id":1,"async":true}}`); w.Code != 200 {
		t.Fatalf("schedule denied=%d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/backup/proxy/restore", strings.NewReader(`{"run_id":1,"confirm":true}`))
	r.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	w := httptest.NewRecorder()
	s.handleAppCallback(w, r)
	if w.Code != 403 {
		t.Fatalf("HTTP proxy bypass=%d %s", w.Code, w.Body.String())
	}
}
