package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedPlatformBackupInstall(t *testing.T, s *Server, projectID, status string, installedBy int64, permissions ...sdk.Permission) (int64, string) {
	t.Helper()
	ensureTestAdmin(t, s)
	if installedBy > 1 {
		if _, err := s.store.db.Exec(
			`INSERT OR IGNORE INTO users(id,email,password_hash,role) VALUES(?,?,?,'user')`,
			installedBy, "backup-user-"+itoa(installedBy)+"@test.local", "x",
		); err != nil {
			t.Fatal(err)
		}
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "backup-callback-test", Requires: sdk.Requires{Permissions: permissions}}
	manifestJSON, _ := json.Marshal(manifest)
	result, err := s.store.db.Exec(
		`INSERT INTO apps(name,source,manifest_json) VALUES(?,?,?)`,
		"backup-callback-"+generateToken(4), "git", string(manifestJSON),
	)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := result.LastInsertId()
	permissionJSON, _ := json.Marshal(permissions)
	result, err = s.store.db.Exec(
		`INSERT INTO app_installs(app_id,project_id,status,installed_by,permissions_json,manifest_json) VALUES(?,?,?,?,?,?)`,
		appID, projectID, status, installedBy, string(permissionJSON), string(manifestJSON),
	)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := result.LastInsertId()
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	return installID, token
}

func callPlatformBackup(t *testing.T, s *Server, method, path, token string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(recorder, req)
	return recorder
}

func TestPlatformBackupCallbackPermittedGlobalAdminInstallStreamsSnapshot(t *testing.T) {
	s := newTestServer(t)
	_, token := seedPlatformBackupInstall(t, s, "", "running", 1, sdk.PermPlatformBackupRead)
	recorder := callPlatformBackup(t, s, http.MethodGet, "/apps/callback/platform/snapshot", token, nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("content type=%q", recorder.Header().Get("Content-Type"))
	}
	files := readTarGz(t, recorder.Body.Bytes())
	if _, ok := files["server/apteva-server.db"]; !ok {
		t.Fatalf("snapshot files=%v", mapKeys(files))
	}
}

func TestPlatformBackupCallbackAuthorizationBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		projectID   string
		status      string
		installedBy int64
		permissions []sdk.Permission
		wantStatus  int
		wantBody    string
	}{
		{"missing read permission", "", "running", 1, nil, http.StatusForbidden, "platform.backup.read"},
		{"project scoped", "project-1", "running", 1, []sdk.Permission{sdk.PermPlatformBackupRead}, http.StatusForbidden, "global"},
		{"non admin owner", "", "running", 2, []sdk.Permission{sdk.PermPlatformBackupRead}, http.StatusForbidden, "administrator"},
		{"missing owner", "", "running", 0, []sdk.Permission{sdk.PermPlatformBackupRead}, http.StatusForbidden, "administrator"},
		{"disabled", "", "disabled", 1, []sdk.Permission{sdk.PermPlatformBackupRead}, http.StatusUnauthorized, "unauthorized"},
		{"error", "", "error", 1, []sdk.Permission{sdk.PermPlatformBackupRead}, http.StatusUnauthorized, "unauthorized"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			_, token := seedPlatformBackupInstall(t, s, tc.projectID, tc.status, tc.installedBy, tc.permissions...)
			recorder := callPlatformBackup(t, s, http.MethodGet, "/apps/callback/platform/snapshot", token, nil, nil)
			if recorder.Code != tc.wantStatus || !strings.Contains(recorder.Body.String(), tc.wantBody) {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPlatformBackupRestoreRequiresDedicatedPermissionAndConfirmation(t *testing.T) {
	archive := buildSnapshotTar(t, map[string]any{"format_version": 1}, nil)

	t.Run("read cannot restore", func(t *testing.T) {
		s := newTestServer(t)
		_, token := seedPlatformBackupInstall(t, s, "", "running", 1, sdk.PermPlatformBackupRead)
		recorder := callPlatformBackup(t, s, http.MethodPost, "/apps/callback/platform/restore", token, bytes.NewReader(archive), map[string]string{"X-Confirm-Restore": "yes"})
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), string(sdk.PermPlatformBackupRestore)) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("confirmation required", func(t *testing.T) {
		s := newTestServer(t)
		_, token := seedPlatformBackupInstall(t, s, "", "running", 1, sdk.PermPlatformBackupRestore)
		recorder := callPlatformBackup(t, s, http.MethodPost, "/apps/callback/platform/restore", token, bytes.NewReader(archive), nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "X-Confirm-Restore") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("permitted restore", func(t *testing.T) {
		s := newTestServer(t)
		_, token := seedPlatformBackupInstall(t, s, "", "running", 1, sdk.PermPlatformBackupRestore)
		recorder := callPlatformBackup(t, s, http.MethodPost, "/apps/callback/platform/restore", token, bytes.NewReader(archive), map[string]string{"X-Confirm-Restore": "yes"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestPlatformBackupAppTokenCannotReachAdminSnapshotRoute(t *testing.T) {
	s := newTestServer(t)
	_, token := seedPlatformBackupInstall(t, s, "", "running", 1, sdk.PermPlatformBackupRead)
	req := httptest.NewRequest(http.MethodGet, "/platform/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformSnapshot)(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRestoreRejectsOversizedExpandedEntryBeforeWriting(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "oversized.bin", Mode: 0o600, Size: maxRestoreEntryBytes + 1}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close() // deliberately incomplete body; the header alone must reject it
	_ = gz.Close()

	s := newTestServer(t)
	token := adminSession(t, s)
	req := postBytes(http.MethodPost, "/api/platform/restore", token, compressed.Bytes(), map[string]string{"X-Confirm-Restore": "yes"})
	recorder := httptest.NewRecorder()
	s.authMiddleware(s.handlePlatformRestore)(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
