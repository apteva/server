package main

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupEventTargets(t *testing.T) (*Server, int64, int64) {
	s := newBusServer(t)
	seedProject(t, s, "project-a")
	source := seedInstall(t, s, "signup", "project-a")
	target := seedInstall(t, s, "processes", "project-a")
	_, e := s.store.db.Exec(`UPDATE app_installs SET installed_by=1,permissions_json='["platform.events.subscribe"]' WHERE id IN (?,?)`, source, target)
	if e != nil {
		t.Fatal(e)
	}
	s.installedApps = NewInstalledAppsRegistry()
	return s, source, target
}
func targetRequest(s *Server, target int64, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/apps/callback/event-subscriptions"+path, strings.NewReader(body))
	r.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(target))
	w := httptest.NewRecorder()
	s.handleAppCallback(w, r)
	return w
}
func registerTarget(t *testing.T, s *Server, source, target int64, revision int, enabled bool) {
	t.Helper()
	raw, _ := json.Marshal(sdk.AppEventSubscription{Key: "on-signup", ProjectID: "project-a", SourceInstallID: source, Topic: "customer.signed_up", Revision: revision, Enabled: enabled})
	w := targetRequest(s, target, "POST", "", string(raw))
	if w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
}
func publishSignup(t *testing.T, s *Server, source int64, key string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(fmt.Sprintf(`{"event_id":%q,"topic":"customer.signed_up","data":{"id":"customer-1"}}`, key)))
	r.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(source))
	w := httptest.NewRecorder()
	s.handleAppEventEmit(w, r)
	return w.Code
}
func TestDurableAppTargetRetryRestartAndPublisherDedup(t *testing.T) {
	s, source, target := setupEventTargets(t)
	registerTarget(t, s, source, target, 1, true)
	if c := publishSignup(t, s, source, "signup-1"); c != 200 {
		t.Fatal(c)
	}
	if c := publishSignup(t, s, source, "signup-1"); c != 200 {
		t.Fatal(c)
	}
	var count int
	s.store.db.QueryRow(`SELECT count(*) FROM app_event_target_outbox`).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate outbox", count)
	}
	d := NewAppEventDispatcher(s)
	d.drainAppEventTargets(context.Background()) // target offline
	var status string
	var attempts int
	s.store.db.QueryRow(`SELECT status,attempts FROM app_event_target_outbox`).Scan(&status, &attempts)
	if status != "pending" || attempts != 1 {
		t.Fatalf("lost offline delivery: %s %d", status, attempts)
	}
	var delivered []sdk.Event
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer target-token" {
			t.Error("missing target authentication")
		}
		var ev sdk.Event
		if e := json.NewDecoder(r.Body).Decode(&ev); e != nil {
			t.Error(e)
		}
		delivered = append(delivered, ev)
		if len(delivered) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: target, AppName: "processes", ProjectID: "project-a", SidecarURL: sidecar.URL, Token: "target-token"})
	for i := 0; i < 2; i++ {
		s.store.db.Exec(`UPDATE app_event_target_outbox SET next_attempt=0`)
		NewAppEventDispatcher(s).drainAppEventTargets(context.Background())
	}
	if len(delivered) != 2 || delivered[0].DeliveryID == "" || delivered[0].DeliveryID != delivered[1].DeliveryID || delivered[0].SourceInstallID != source || delivered[0].ProjectID != "project-a" {
		t.Fatalf("bad retry delivery %+v", delivered)
	}
	s.store.db.QueryRow(`SELECT status FROM app_event_target_outbox`).Scan(&status)
	if status != "delivered" {
		t.Fatal(status)
	}
}
func TestAppTargetAuthorizationPauseRevisionAndIsolation(t *testing.T) {
	s, source, target := setupEventTargets(t)
	registerTarget(t, s, source, target, 1, true)
	raw := fmt.Sprintf(`{"key":"other","project_id":"project-b","source_install_id":%d,"topic":"customer.signed_up","revision":1,"enabled":true}`, source)
	if w := targetRequest(s, target, "POST", "", raw); w.Code != 403 {
		t.Fatal("cross-project subscription accepted", w.Code)
	}
	if c := publishSignup(t, s, source, "before-pause"); c != 200 {
		t.Fatal(c)
	}
	registerTarget(t, s, source, target, 2, false)
	NewAppEventDispatcher(s).drainAppEventTargets(context.Background())
	var status string
	s.store.db.QueryRow(`SELECT status FROM app_event_target_outbox`).Scan(&status)
	if status != "canceled" {
		t.Fatal(status)
	}
	publishSignup(t, s, source, "while-paused")
	registerTarget(t, s, source, target, 3, true)
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM app_event_target_outbox`).Scan(&n)
	if n != 1 {
		t.Fatal("paused event replayed", n)
	}
	raw = fmt.Sprintf(`{"key":"on-signup","project_id":"project-a","source_install_id":%d,"topic":"different","revision":3,"enabled":true}`, source)
	if w := targetRequest(s, target, "POST", "", raw); w.Code != 409 {
		t.Fatal("same revision mutation", w.Code)
	}
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, target)
	if w := targetRequest(s, target, "GET", "/sources?project_id=project-a", ""); w.Code != 403 {
		t.Fatal("missing permission accepted")
	}
}
func TestPublisherEventIDCannotChangeContent(t *testing.T) {
	s, source, target := setupEventTargets(t)
	registerTarget(t, s, source, target, 1, true)
	publishSignup(t, s, source, "same")
	r := httptest.NewRequest("POST", "/app-events/internal/emit", strings.NewReader(`{"event_id":"same","topic":"customer.signed_up","data":{"id":"different"}}`))
	r.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(source))
	w := httptest.NewRecorder()
	s.handleAppEventEmit(w, r)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAppTargetSourceMustShareProjectAndOwner(t *testing.T) {
	s, source, target := setupEventTargets(t)
	other := seedInstall(t, s, "other-source", "project-b")
	s.store.db.Exec(`UPDATE app_installs SET installed_by=1 WHERE id=?`, other)
	request := func(id int64) *httptest.ResponseRecorder {
		return targetRequest(s, target, "POST", "", fmt.Sprintf(`{"key":"x","project_id":"project-a","source_install_id":%d,"topic":"customer.signed_up","revision":1,"enabled":true}`, id))
	}
	if w := request(other); w.Code != 403 {
		t.Fatal("other project source accepted", w.Code)
	}
	s.store.db.Exec(`UPDATE app_installs SET installed_by=2 WHERE id=?`, source)
	if w := request(source); w.Code != 403 {
		t.Fatal("other owner's source accepted", w.Code)
	}
}
