package main

// HTTP edge of the AppEventBus:
//   - POST /api/app-events/internal/emit: requires sidecar token
//     (auth middleware sets X-Apteva-App-Install-ID), looks up
//     app/project from the install row, publishes onto the bus.
//   - GET  /api/app-events/<app>?project_id=...&since=...: SSE
//     stream, replays from ring on connect, live-tails after.
//
// These tests skip the auth middleware itself (covered in
// auth_test.go) and call the handlers directly with the headers
// the middleware would have set. That keeps the unit under test
// the bus integration, not the auth layer.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------

// seedInstall creates an apps row + an app_installs row and returns
// the install id. The real apps_wire_test.go uses the same DDL the
// production seed path runs; for our unit-test purposes a minimal
// insert is enough — appbus only reads (a.name, i.project_id).
func seedInstall(t *testing.T, s *Server, appName, projectID string) int64 {
	t.Helper()
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', '{}')`,
		appName,
	); err != nil {
		t.Fatalf("insert apps: %v", err)
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, appName).Scan(&appID); err != nil {
		t.Fatalf("select app id: %v", err)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, ?, 'running')`,
		appID, projectID,
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedProject creates a row in the projects table so the SSE
// access check passes. Inserts a minimal user first because the
// projects table FKs into users(id).
func seedProject(t *testing.T, s *Server, id string) {
	t.Helper()
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (?, ?, ?)`,
		1, "test@test.local", "x",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO projects (id, user_id, name) VALUES (?, ?, ?)`,
		id, 1, "test",
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func newBusServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.appBus = NewAppEventBus()
	return s
}

// --- POST /internal/emit --------------------------------------------

func TestEmitHandler_RequiresSidecarToken(t *testing.T) {
	s := newBusServer(t)
	body := strings.NewReader(`{"topic":"file.added","data":{}}`)
	req := httptest.NewRequest("POST", "/app-events/internal/emit", body)
	// No X-Apteva-App-Install-ID header — middleware would have
	// rejected this, but we're unit-testing the handler.
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without install id header, got %d", rec.Code)
	}
}

func TestEmitHandler_RejectsBadJSON(t *testing.T) {
	s := newBusServer(t)
	installID := seedInstall(t, s, "storage", "p1")
	req := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(`not json`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", rec.Code)
	}
}

func TestEmitHandler_RequiresTopic(t *testing.T) {
	s := newBusServer(t)
	installID := seedInstall(t, s, "storage", "p1")
	req := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(`{"topic":"  ","data":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty topic, got %d", rec.Code)
	}
}

func TestEmitHandler_StampsAppAndProjectFromInstall(t *testing.T) {
	s := newBusServer(t)
	installID := seedInstall(t, s, "storage", "proj-A")

	// Subscribe to the (storage, proj-A) lane BEFORE emit so we
	// catch the live event (not the ring replay path).
	ch, _, cancel := s.appBus.Subscribe("storage", "proj-A", 0)
	defer cancel()

	body := `{"topic":"file.added","data":{"id":42,"name":"foo.pdf"}}`
	req := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.App != "storage" {
			t.Errorf("App = %q, want storage", ev.App)
		}
		if ev.ProjectID != "proj-A" {
			t.Errorf("ProjectID = %q, want proj-A", ev.ProjectID)
		}
		if ev.InstallID != installID {
			t.Errorf("InstallID = %d, want %d", ev.InstallID, installID)
		}
		if ev.Topic != "file.added" {
			t.Errorf("Topic = %q", ev.Topic)
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("data not JSON: %v", err)
		}
		if data["name"] != "foo.pdf" {
			t.Errorf("data.name = %v, want foo.pdf", data["name"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber didn't receive event")
	}
}

func TestEmitHandler_CannotSpoofAppName(t *testing.T) {
	// The sidecar can't pass app= in the body; the server derives
	// it from the install row. Even if a malicious sidecar tried,
	// the server-stamped fields are the only ones written to the bus.
	s := newBusServer(t)
	installID := seedInstall(t, s, "storage", "proj-A")

	// Subscribe to a DIFFERENT app's lane the attacker might want
	// to forge events into.
	chCRM, _, cancelCRM := s.appBus.Subscribe("crm", "proj-A", 0)
	defer cancelCRM()

	// Body claims app=crm — should be ignored.
	body := `{"topic":"contact.added","app":"crm","project_id":"proj-A","data":{}}`
	req := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	select {
	case ev := <-chCRM:
		t.Fatalf("CRM lane received forged event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected — server stamped app=storage, not crm
	}
}

func TestEmitHandler_UnknownInstallReturns404(t *testing.T) {
	s := newBusServer(t)
	body := strings.NewReader(`{"topic":"x","data":{}}`)
	req := httptest.NewRequest("POST", "/app-events/internal/emit", body)
	req.Header.Set("X-Apteva-App-Install-ID", "999999")
	rec := httptest.NewRecorder()
	s.handleAppEventEmit(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown install, got %d", rec.Code)
	}
}

// --- GET /<app>?project_id=&since= -----------------------------------

func TestStreamHandler_RequiresProjectID(t *testing.T) {
	s := newBusServer(t)
	req := httptest.NewRequest("GET", "/app-events/storage", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppEventStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without project_id, got %d", rec.Code)
	}
}

func TestStreamHandler_RejectsSidecarToken(t *testing.T) {
	s := newBusServer(t)
	req := httptest.NewRequest("GET", "/app-events/storage?project_id=p1", nil)
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("X-Apteva-App-Install-ID", "5")
	rec := httptest.NewRecorder()
	s.handleAppEventStream(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for sidecar token on stream, got %d", rec.Code)
	}
}

func TestStreamHandler_RequiresLogin(t *testing.T) {
	s := newBusServer(t)
	seedProject(t, s, "p1")
	req := httptest.NewRequest("GET", "/app-events/storage?project_id=p1", nil)
	rec := httptest.NewRecorder()
	s.handleAppEventStream(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without login, got %d", rec.Code)
	}
}

func TestStreamHandler_DeliversFrameOverSSE(t *testing.T) {
	s := newBusServer(t)
	seedProject(t, s, "p1")

	// httptest server so we get a real flushable response writer
	// and the request context cancels naturally on close.
	mux := http.NewServeMux()
	mux.HandleFunc("/app-events/", func(w http.ResponseWriter, r *http.Request) {
		// Inject the headers the auth middleware would add.
		r.Header.Set("X-User-ID", "1")
		s.handleAppEventStream(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/app-events/storage?project_id=p1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body.String())
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Publish — subscriber should get a frame.
	go func() {
		// Tiny delay so the consumer is subscribed before we publish.
		time.Sleep(50 * time.Millisecond)
		s.appBus.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{"id":1}`))
	}()

	br := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(br, 1*time.Second)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame.id != "1" {
		t.Errorf("frame id = %q, want 1", frame.id)
	}
	var ev AppEvent
	if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
		t.Fatalf("frame data not JSON: %v\nraw: %s", err, frame.data)
	}
	if ev.Topic != "file.added" || ev.App != "storage" || ev.ProjectID != "p1" {
		t.Errorf("frame envelope mismatch: %+v", ev)
	}
}

func TestStreamHandler_SinceReplaysFromRing(t *testing.T) {
	s := newBusServer(t)
	seedProject(t, s, "p1")

	// Pre-publish 5 events.
	for i := 0; i < 5; i++ {
		s.appBus.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app-events/", func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		s.handleAppEventStream(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// since=2 → should replay seq 3, 4, 5.
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/app-events/storage?project_id=p1&since=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	wantSeqs := []string{"3", "4", "5"}
	for _, want := range wantSeqs {
		f, err := readSSEFrame(br, 1*time.Second)
		if err != nil {
			t.Fatalf("expected frame seq=%s, got err %v", want, err)
		}
		if f.id != want {
			t.Fatalf("got frame id=%s, want %s", f.id, want)
		}
	}
}

// --- SSE frame reader ------------------------------------------------

type sseFrame struct {
	id   string
	data string
}

// readSSEFrame parses one SSE event (id + data) from a persistent
// bufio.Reader. Skips keepalive comments. Caller passes the SAME
// reader across calls so buffered bytes don't get dropped between
// frames.
func readSSEFrame(br *bufio.Reader, deadline time.Duration) (sseFrame, error) {
	_ = deadline // the underlying response body's read timeout enforces this
	out := sseFrame{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return out, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if out.data != "" {
				return out, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// keepalive comment
			continue
		}
		if strings.HasPrefix(line, "id:") {
			out.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "data:") {
			out.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}
