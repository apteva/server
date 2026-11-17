package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformUpdateRequiresAdmin(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handlePlatformUpdate(w, httptest.NewRequest(method, "/platform-update", nil))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous status=%d", w.Code)
			}
			if _, err := s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			w = httptest.NewRecorder()
			s.handlePlatformUpdate(w, authedRequest(t, method, "/platform-update", "", nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("nonadmin status=%d", w.Code)
			}
		})
	}
}

func TestPlatformUpdateRejectsUnreviewedReleaseAndArguments(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.platformStatus = &platformStatusPoller{view: platformStatusView{UpdateAvailable: true, BundleVersion: "1.2.3"}}
	for _, tc := range []struct {
		body   map[string]any
		status int
	}{
		{map[string]any{"version": "1.2.2"}, http.StatusConflict},
		{map[string]any{"version": "1.2.3", "agent_policy": "shell"}, http.StatusBadRequest},
		{map[string]any{"version": "1.2.3", "home": "/tmp/other"}, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		req := authedRequest(t, "POST", "/platform-update", "", tc.body)
		req.Header.Set("X-Apteva-Operator-ID", "1")
		s.handlePlatformUpdate(w, req)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d: %s", w.Code, tc.status, w.Body.String())
		}
	}
}

func TestPlatformUpdateRejectsAppOrAgentUsingAdminIdentity(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		s.handlePlatformUpdate(w, authedRequest(t, method, "/platform-update", "", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("delegated admin identity accepted: %d", w.Code)
		}
	}
}

func TestUpdateDatabasePreflightPreservesSourceAndBackup(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "live.db")
	store, err := NewStore(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetSetting("update-sentinel", "keep"); err != nil {
		t.Fatal(err)
	}
	before, err := updateDatabaseSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backups", "saved.db")
	if err := preflightUpdateDatabase(source, backup); err != nil {
		t.Fatal(err)
	}
	after, err := updateDatabaseSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || store.GetSetting("update-sentinel") != "keep" {
		t.Fatal("preflight modified live data")
	}
	backupSchema, err := updateDatabaseSchema(backup)
	if err != nil || backupSchema != before {
		t.Fatalf("invalid backup: %v", err)
	}
	raw, err := os.ReadFile(backup + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Version       string `json:"version"`
		SchemaChanged bool   `json:"schema_changed"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != CLIVersion || report.SchemaChanged {
		t.Fatalf("unexpected report: %+v", report)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup must be private")
	}
	if _, err := os.Stat(backup + ".preflight"); !os.IsNotExist(err) {
		t.Fatal("disposable database was retained")
	}
	if err := preflightUpdateDatabase(source, backup); err == nil {
		t.Fatal("existing backup was overwritten")
	}
}

func TestUpdateDatabaseFingerprintDetectsDataOnlyChanges(t *testing.T) {
	source := filepath.Join(t.TempDir(), "live.db")
	store, err := NewStore(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetSetting("sentinel", "before"); err != nil {
		t.Fatal(err)
	}
	schema, err := updateDatabaseSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	before, err := updateDatabaseFingerprint(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting("sentinel", "after"); err != nil {
		t.Fatal(err)
	}
	after, err := updateDatabaseFingerprint(source)
	if err != nil {
		t.Fatal(err)
	}
	newSchema, err := updateDatabaseSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	if before == after || schema != newSchema {
		t.Fatal("data-only changes must disable automatic binary rollback")
	}
	again, err := updateDatabaseFingerprint(source)
	if err != nil || again != after {
		t.Fatal("fingerprint must be deterministic")
	}
}
