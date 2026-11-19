package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestPlatformUpdateViewShowsTheDatabaseCheck(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	job := `{"id":"j","source":"cli","state":"preflight","message":"Copying the database (1/2)","target_version":"1.2.3","previous_version":"1.2.2",
		"progress":{"phase":"copy","done":1,"total":2},
		"check":{"classification":"incompatible","compatible":false,"reasons":["unique index"],"backup":"/b/apteva.db","backup_bytes":7,"duration_ms":12,"sampled_tables":["t"]},
		"request":{"home":"/secret/home","db_path":"/secret/db","server_pid":9,"agent_policy":"preserve"}}`
	if err := os.WriteFile(filepath.Join(s.dataDir, "platform-update.json"), []byte(job), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := s.platformUpdater(context.Background(), false, "", "", "")
	if err != nil || view.Job == nil {
		t.Fatalf("view %+v: %v", view, err)
	}
	if view.Job.Source != "cli" || view.Job.Progress == nil || view.Job.Progress.Total != 2 || view.Job.Check == nil || view.Job.Check.Classification != "incompatible" || view.Job.Check.Backup != "/b/apteva.db" {
		t.Fatalf("check not passed through: %+v", view.Job)
	}
	raw, _ := json.Marshal(view)
	for _, private := range []string{"/secret/home", "/secret/db", "server_pid", "sampled_tables"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("view exposes %s: %s", private, raw)
		}
	}
}
