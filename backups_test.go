package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// sessionForUser creates a user via the store and mints a session
// cookie token bound to it. Bypasses handleRegister/handleLogin so we
// can pin user ids deterministically (id=1 = admin, id=2 = not).
func sessionForUser(t *testing.T, s *Server, email string) (userID int64, token string) {
	t.Helper()
	user, err := s.store.CreateUser(email, "$2y$10$ignored-bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	token = "test-session-" + email
	if err := s.store.CreateSession(token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return user.ID, token
}

func adminSession(t *testing.T, s *Server) string {
	t.Helper()
	id, tok := sessionForUser(t, s, "admin@test.com")
	if id != 1 {
		t.Fatalf("admin user expected id=1, got %d", id)
	}
	return tok
}

func nonAdminSession(t *testing.T, s *Server) string {
	t.Helper()
	_, tok := sessionForUser(t, s, "bob@test.com")
	return tok
}

func sessionReq(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return req
}

func readTarGz(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", h.Name, err)
		}
		out[h.Name] = buf
	}
	return out
}

func TestSnapshot_NonAdmin403(t *testing.T) {
	s := newTestServer(t)
	_ = adminSession(t, s) // claim id=1
	bobTok := nonAdminSession(t, s)

	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, sessionReq("GET", "/api/platform/snapshot", bobTok))

	if w.Code != 403 {
		t.Errorf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSnapshot_Unauthenticated401(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/platform/snapshot", nil)
	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSnapshot_WrongMethod(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, sessionReq("POST", "/api/platform/snapshot", tok))
	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestSnapshot_ServerDBOnly(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)

	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, sessionReq("GET", "/api/platform/snapshot", tok))

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type=%q want application/gzip", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("missing Content-Disposition")
	}

	files := readTarGz(t, w.Body.Bytes())
	if _, ok := files["manifest.json"]; !ok {
		t.Errorf("missing manifest.json — got %v", mapKeys(files))
	}
	dump, ok := files["server/apteva-server.db"]
	if !ok {
		t.Fatalf("missing server/apteva-server.db — got %v", mapKeys(files))
	}
	if !bytes.HasPrefix(dump, []byte("SQLite format 3\x00")) {
		t.Errorf("server dump is not a SQLite file (header %q)", dump[:min(16, len(dump))])
	}

	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if manifest["format_version"] != float64(1) {
		t.Errorf("format_version=%v", manifest["format_version"])
	}
	if _, ok := manifest["installs"]; !ok {
		t.Errorf("installs key missing from manifest")
	}
}

func TestSnapshot_WithInstall(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)

	cacheDir := t.TempDir()
	s.localApps = NewLocalSupervisor(cacheDir)
	s.installedApps = NewInstalledAppsRegistry()

	const installID = int64(42)
	const appName = "demo-app"
	dbDir := filepath.Join(cacheDir, appName, "data", "42")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "app.db")
	mustSeedSqlite(t, dbPath)

	s.installedApps.Add(&InstalledApp{
		InstallID:  installID,
		AppName:    appName,
		SidecarURL: "http://127.0.0.1:9999",
		Manifest:   sdk.Manifest{Version: "0.1.0"},
	})

	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, sessionReq("GET", "/api/platform/snapshot", tok))

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	files := readTarGz(t, w.Body.Bytes())

	wantPath := fmt.Sprintf("apps/%d-%s/app.db", installID, appName)
	dump, ok := files[wantPath]
	if !ok {
		t.Fatalf("missing %s in archive — got %v", wantPath, mapKeys(files))
	}
	if !bytes.HasPrefix(dump, []byte("SQLite format 3\x00")) {
		t.Errorf("install dump is not a SQLite file")
	}

	var manifest struct {
		Installs []struct {
			InstallID  int64  `json:"install_id"`
			Name       string `json:"name"`
			DBIncluded bool   `json:"db_included"`
			DBPath     string `json:"db_path_in_archive"`
		} `json:"installs"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(manifest.Installs) != 1 {
		t.Fatalf("want 1 install in manifest, got %d", len(manifest.Installs))
	}
	got := manifest.Installs[0]
	if got.InstallID != installID || got.Name != appName ||
		!got.DBIncluded || got.DBPath != wantPath {
		t.Errorf("manifest install = %+v", got)
	}
}

func TestSnapshot_StaticInstallSkipped(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 7,
		AppName:   "static-thing",
		// SidecarURL deliberately empty: this is a static (no-process) app.
		Manifest: sdk.Manifest{Version: "0.0.1"},
	})

	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w, sessionReq("GET", "/api/platform/snapshot", tok))

	if w.Code != 200 {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	files := readTarGz(t, w.Body.Bytes())
	for name := range files {
		if strings.HasPrefix(name, "apps/") {
			t.Errorf("static install should not produce an apps/ entry, got %q", name)
		}
	}
	var manifest struct {
		Installs []struct {
			Name       string `json:"name"`
			DBIncluded bool   `json:"db_included"`
			Note       string `json:"note"`
		} `json:"installs"`
	}
	_ = json.Unmarshal(files["manifest.json"], &manifest)
	if len(manifest.Installs) != 1 || manifest.Installs[0].DBIncluded ||
		manifest.Installs[0].Note == "" {
		t.Errorf("expected one install marked db_included=false with a note, got %+v", manifest.Installs)
	}
}

func TestVacuumIntoFromHandle_ProducesValidSqlite(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.db")
	mustSeedSqlite(t, src)
	dst := filepath.Join(t.TempDir(), "dump.db")

	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := vacuumIntoFromHandle(db, dst); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	bs, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bs, []byte("SQLite format 3\x00")) {
		t.Errorf("not a sqlite file")
	}
}

func TestSafeArchiveSegment(t *testing.T) {
	cases := map[string]string{
		"crm":     "crm",
		"":        "unnamed",
		"a/b":     "a_b",
		`a\b`:    "a_b",
		"..":      "_",
		"../etc": "__etc",
	}
	for in, want := range cases {
		if got := safeArchiveSegment(in); got != want {
			t.Errorf("safeArchiveSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers ---

func mustSeedSqlite(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t (name) VALUES ('hello');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─── Restore tests ─────────────────────────────────────────────────

// buildSnapshotTar returns a gzipped tar containing the given files.
// Pass "" for content to omit the manifest entirely.
func buildSnapshotTar(t *testing.T, manifest map[string]any, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if manifest != nil {
		manifestBytes, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifestBytes)), ModTime: time.Now()})
		_, _ = tw.Write(manifestBytes)
	}
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now()})
		_, _ = tw.Write(content)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// insertTestInstall registers an app + install row directly in the
// store. Returns the install id. Caller picks the install id by way
// of the unique-name suffix; the autoincrement may not produce a
// predictable id when other tests share the same DB, so we always
// look it up after insert.
func insertTestInstall(t *testing.T, s *Server, appName string) int64 {
	t.Helper()
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, manifest_json) VALUES (?, 'builtin', '{}')`,
		appName)
	if err != nil {
		t.Fatalf("insert apps: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs (app_id, status) VALUES (?, 'running')`, appID)
	if err != nil {
		t.Fatalf("insert app_installs: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func postBytes(method, path, token string, body []byte, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestRestore_NonAdmin403(t *testing.T) {
	s := newTestServer(t)
	_ = adminSession(t, s)
	bobTok := nonAdminSession(t, s)

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", bobTok, []byte("ignored"),
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 403 {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRestore_Unauthenticated401(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/platform/restore", bytes.NewReader([]byte("x")))
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRestore_WrongMethod(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	w := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformRestore)(w, sessionReq("GET", "/api/platform/restore", tok))
	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestRestore_MissingConfirmHeader(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, []byte("anything"), nil)
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 without X-Confirm-Restore, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRestore_InvalidGzip(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, []byte("not gzip at all"),
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 for non-gzip body, got %d", w.Code)
	}
}

func TestRestore_MissingManifest(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	body := buildSnapshotTar(t, nil, map[string][]byte{"server/apteva-server.db": []byte("noise")})

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, body,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 missing manifest, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRestore_WrongFormatVersion(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	body := buildSnapshotTar(t, map[string]any{"format_version": 999}, nil)

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, body,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 wrong format_version, got %d", w.Code)
	}
}

func TestRestore_ServerDBStaged(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	// Test server has store but no dbPath set; point it at a real
	// (empty) file under a temp dir so the staging logic has somewhere
	// to write.
	s.dbPath = filepath.Join(t.TempDir(), "apteva-server.db")
	if err := os.WriteFile(s.dbPath, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	restoredBytes := []byte("SQLite format 3\x00FAKE-RESTORED-DB-PAYLOAD")
	body := buildSnapshotTar(t,
		map[string]any{"format_version": 1},
		map[string][]byte{"server/apteva-server.db": restoredBytes})

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, body,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	// Marker + .restored present, original untouched.
	if _, err := os.Stat(s.dbPath + ".restored.marker"); err != nil {
		t.Errorf("missing marker: %v", err)
	}
	got, err := os.ReadFile(s.dbPath + ".restored")
	if err != nil {
		t.Fatalf("missing .restored: %v", err)
	}
	if !bytes.Equal(got, restoredBytes) {
		t.Errorf(".restored bytes differ from input")
	}
	if cur, _ := os.ReadFile(s.dbPath); string(cur) != "placeholder" {
		t.Errorf("live DB should not be touched until next boot, got %q", string(cur))
	}

	var report restoreReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.ServerDB != "staged" || !report.RestartRequired {
		t.Errorf("report = %+v", report)
	}
}

func TestRestore_AppDBLive(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)

	cacheDir := t.TempDir()
	s.localApps = NewLocalSupervisor(cacheDir)
	s.installedApps = NewInstalledAppsRegistry()

	// Pre-create the app + install row so lookupAppNameForInstall works.
	const appName = "demo-app"
	installID := insertTestInstall(t, s, appName)

	// Pre-create the dest DB with old data + a stale -wal companion to
	// verify both get cleaned up.
	dbDir := filepath.Join(cacheDir, appName, "data", strconv.FormatInt(installID, 10))
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "app.db")
	if err := os.WriteFile(dbPath, []byte("OLD-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("STALE-WAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	restoredBytes := []byte("SQLite format 3\x00restored-app-db-bytes")
	archivePath := fmt.Sprintf("apps/%d-%s/app.db", installID, appName)
	body := buildSnapshotTar(t,
		map[string]any{"format_version": 1},
		map[string][]byte{archivePath: restoredBytes})

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, body,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	got, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read swapped DB: %v", err)
	}
	if !bytes.Equal(got, restoredBytes) {
		t.Errorf("restored bytes mismatch: got %q", string(got))
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Errorf("stale -wal should have been removed (err=%v)", err)
	}
	// Old bytes preserved in a backup file with the prerestore prefix.
	matches, _ := filepath.Glob(dbPath + ".prerestore-*")
	if len(matches) != 1 {
		t.Errorf("expected one prerestore backup, got %d: %v", len(matches), matches)
	} else if bs, _ := os.ReadFile(matches[0]); string(bs) != "OLD-DATA" {
		t.Errorf("backup contents wrong: %q", string(bs))
	}

	var report restoreReport
	_ = json.Unmarshal(w.Body.Bytes(), &report)
	if len(report.Installs) != 1 || report.Installs[0].Status != "applied" ||
		report.Installs[0].InstallID != installID {
		t.Errorf("report = %+v", report)
	}
}

func TestRestore_UnknownInstallID(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()

	body := buildSnapshotTar(t,
		map[string]any{"format_version": 1},
		map[string][]byte{"apps/9999-ghost/app.db": []byte("payload")})

	w := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, body,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 (unknown install is per-entry error, not a top-level fail), got %d", w.Code)
	}
	var report restoreReport
	_ = json.Unmarshal(w.Body.Bytes(), &report)
	if len(report.Installs) != 1 || report.Installs[0].Status != "error" {
		t.Errorf("expected one error entry, got %+v", report.Installs)
	}
	if !strings.Contains(report.Installs[0].Note, "not present on this server") {
		t.Errorf("note doesn't mention missing install: %q", report.Installs[0].Note)
	}
}

func TestRestore_RoundTrip_AppDB(t *testing.T) {
	s := newTestServer(t)
	tok := adminSession(t, s)
	// Snapshot will tar the server DB; give the restore stage somewhere
	// to land it.
	s.dbPath = filepath.Join(t.TempDir(), "apteva-server.db")
	_ = os.WriteFile(s.dbPath, []byte("placeholder"), 0o644)

	cacheDir := t.TempDir()
	s.localApps = NewLocalSupervisor(cacheDir)
	s.installedApps = NewInstalledAppsRegistry()

	const appName = "round-trip-app"
	installID := insertTestInstall(t, s, appName)

	dbDir := filepath.Join(cacheDir, appName, "data", strconv.FormatInt(installID, 10))
	_ = os.MkdirAll(dbDir, 0o755)
	dbPath := filepath.Join(dbDir, "app.db")
	mustSeedSqlite(t, dbPath)

	s.installedApps.Add(&InstalledApp{
		InstallID:  installID,
		AppName:    appName,
		SidecarURL: "http://127.0.0.1:9999",
		Manifest:   sdk.Manifest{Version: "0.1.0"},
	})

	// Snapshot. The bytes inside the tar are the canonical "what would
	// be restored" form (a fresh VACUUM INTO of the live DB), which
	// differ from the live file's on-disk bytes by header counters /
	// WAL state. So we compare against the tar contents, not the live
	// file as it was before the snapshot.
	w1 := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(w1, sessionReq("GET", "/api/platform/snapshot", tok))
	if w1.Code != 200 {
		t.Fatalf("snapshot failed: %d %s", w1.Code, w1.Body.String())
	}
	snapshot := w1.Body.Bytes()
	files := readTarGz(t, snapshot)
	expectedAfter, ok := files[fmt.Sprintf("apps/%d-%s/app.db", installID, appName)]
	if !ok {
		t.Fatalf("snapshot doesn't contain the install's app.db; entries=%v", mapKeys(files))
	}

	// Mutate the live file so we can prove restore actually brought
	// back data and didn't just leave the file alone.
	if err := os.WriteFile(dbPath, []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Restore.
	w2 := httptest.NewRecorder()
	req := postBytes("POST", "/api/platform/restore", tok, snapshot,
		map[string]string{"X-Confirm-Restore": "yes"})
	s.authMiddleware(s.handlePlatformRestore)(w2, req)
	if w2.Code != 200 {
		t.Fatalf("restore failed: %d %s", w2.Code, w2.Body.String())
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, expectedAfter) {
		t.Errorf("round-trip mismatch: expected %d bytes from snapshot, got %d on disk", len(expectedAfter), len(after))
	}

	// And the restored file is a valid SQLite header.
	if !bytes.HasPrefix(after, []byte("SQLite format 3\x00")) {
		t.Errorf("restored file is not SQLite")
	}
}

// ─── applyPendingRestore unit tests ────────────────────────────────

func TestApplyPendingRestore_Swaps(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "apteva-server.db")
	if err := os.WriteFile(dbPath, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+restorePendingSuffix, []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+restoreMarkerSuffix, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	applyPendingRestore(dbPath)

	if bs, _ := os.ReadFile(dbPath); string(bs) != "NEW" {
		t.Errorf("DB not swapped, got %q", string(bs))
	}
	if _, err := os.Stat(dbPath + restorePendingSuffix); !os.IsNotExist(err) {
		t.Errorf("pending file should have been moved")
	}
	if _, err := os.Stat(dbPath + restoreMarkerSuffix); !os.IsNotExist(err) {
		t.Errorf("marker should have been removed")
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Errorf("stale -wal should have been removed")
	}
	matches, _ := filepath.Glob(dbPath + restoreBackupPrefix + "*")
	if len(matches) != 1 {
		t.Errorf("expected one backup file, got %d: %v", len(matches), matches)
	} else if bs, _ := os.ReadFile(matches[0]); string(bs) != "OLD" {
		t.Errorf("backup contents = %q want OLD", string(bs))
	}
}

func TestApplyPendingRestore_NoMarker_NoOp(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "apteva-server.db")
	if err := os.WriteFile(dbPath, []byte("LIVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	applyPendingRestore(dbPath)

	if bs, _ := os.ReadFile(dbPath); string(bs) != "LIVE" {
		t.Errorf("DB unexpectedly changed: %q", string(bs))
	}
}

func TestApplyPendingRestore_MarkerWithoutPending_ClearsMarker(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "apteva-server.db")
	_ = os.WriteFile(dbPath, []byte("LIVE"), 0o644)
	_ = os.WriteFile(dbPath+restoreMarkerSuffix, []byte("x"), 0o644)

	applyPendingRestore(dbPath)

	if bs, _ := os.ReadFile(dbPath); string(bs) != "LIVE" {
		t.Errorf("DB should be untouched, got %q", string(bs))
	}
	if _, err := os.Stat(dbPath + restoreMarkerSuffix); !os.IsNotExist(err) {
		t.Errorf("orphan marker should have been cleared")
	}
}

func TestParseInstallIDFromArchivePath(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"apps/42-crm/app.db", 42, false},
		{"apps/1-x/app.db", 1, false},
		{"apps/abc-x/app.db", 0, true},
		{"apps/-x/app.db", 0, true},
		{"apps/0-x/app.db", 0, true},
		{"apps/no-dash/app.db", 0, false}, // "no" parses as a number? No, "no" is not numeric → error
	}
	// Fix the last case expectation.
	cases[len(cases)-1].err = true
	for _, c := range cases {
		got, err := parseInstallIDFromArchivePath(c.in)
		if (err != nil) != c.err {
			t.Errorf("parseInstallIDFromArchivePath(%q) err=%v want err=%v", c.in, err, c.err)
		}
		if got != c.want {
			t.Errorf("parseInstallIDFromArchivePath(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
