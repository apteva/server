package main

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

	sdk "github.com/apteva/app-sdk"
)

// addInstallOf inserts another installation of an app that already has one.
func addInstallOf(t *testing.T, s *Server, existing int64, projectID, key string) int64 {
	t.Helper()
	var appID int64
	if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, existing).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	res, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by, installation_key, installation_name)
		VALUES (?, ?, 'running', 1, ?, ?)`, appID, projectID, key, key)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func dynamicCallerManifest(name string) sdk.Manifest {
	m := officialManifest(name, true)
	m.Requires.Permissions = []sdk.Permission{sdk.PermAppsCall}
	return m
}

// Without a project, findInstalledApp falls back to any default install. A
// dynamic caller must not reach another project's install that way.
func TestResolveDynamicTarget_NoProjectReachesOnlyGlobalInstalls(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "", officialManifest("functions", true), nil)
	projectTarget := seedRunningInstall(t, s, "tables", "proj-B", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if ok || id != 0 {
		t.Fatalf("call without a project reached project install: id=%d", id)
	}
	if !strings.Contains(msg, "only global installs") {
		t.Fatalf("message = %q", msg)
	}
	if id, msg, ok := s.resolveDynamicTarget(caller, "tables", "proj-B"); !ok || id != projectTarget {
		t.Fatalf("call in the project: id=%d ok=%v msg=%q", id, ok, msg)
	}

	globalTarget := addInstallOf(t, s, projectTarget, "", "default")
	if id, msg, ok := s.resolveDynamicTarget(caller, "tables", ""); !ok || id != globalTarget {
		t.Fatalf("global install without a project: id=%d ok=%v msg=%q", id, ok, msg)
	}
}

// A dynamic caller may pin an unbound install only in the call's project or
// a global one; a bound caller keeps its bindings.
func TestAuthorizeAppCallback_DynamicPinStaysInCallProject(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	globalCaller := seedRunningInstall(t, s, "functions", "", dynamicCallerManifest("functions"), nil)
	projectCaller := seedRunningInstall(t, s, "runner", "proj-B", dynamicCallerManifest("runner"), nil)
	projectTarget := seedRunningInstall(t, s, "tables", "proj-B", sdk.Manifest{Name: "tables"}, nil)
	globalTarget := addInstallOf(t, s, projectTarget, "", "default")
	s.installedApps.Add(&InstalledApp{InstallID: projectTarget, AppName: "tables", ProjectID: "proj-B", SidecarURL: "http://127.0.0.1:1"})
	s.installedApps.Add(&InstalledApp{InstallID: globalTarget, AppName: "tables", SidecarURL: "http://127.0.0.1:1"})

	w := httptest.NewRecorder()
	if _, ok := s.authorizeAppCallback(w, globalCaller, "tables", "", projectTarget); ok || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "another project") {
		t.Fatalf("pin without a project into a project install: ok=%v %d %s", ok, w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	if target, ok := s.authorizeAppCallback(w, globalCaller, "tables", "", globalTarget); !ok || target.targetInstallID != globalTarget {
		t.Fatalf("pin of a global install: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	if target, ok := s.authorizeAppCallback(w, projectCaller, "tables", "", projectTarget); !ok || target.targetInstallID != projectTarget {
		t.Fatalf("pin inside the caller's project: %d %s", w.Code, w.Body.String())
	}

	// A bound global caller pinning a project install still needs a role on
	// that project, as before.
	seedProject(t, s, "proj-B")
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members (project_id, user_id, role) VALUES ('proj-B', 1, 'owner')`); err != nil {
		t.Fatal(err)
	}
	bound := seedRunningInstall(t, s, "reports", "", sdk.Manifest{
		Name: "reports",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "tables"}},
		},
	}, map[string]any{"tables": projectTarget})
	w = httptest.NewRecorder()
	if target, ok := s.authorizeAppCallback(w, bound, "tables", "", projectTarget); !ok || target.targetInstallID != projectTarget {
		t.Fatalf("bound caller pin: %d %s", w.Code, w.Body.String())
	}
}

func TestCreateAppEventSubscriptionPinsSourceInstall(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	stable := seedRunningInstall(t, s, "telephony", "proj-1", sdk.Manifest{Name: "telephony"}, nil)
	candidate := addInstallOf(t, s, stable, "proj-1", "candidate")
	globalInstall := addInstallOf(t, s, stable, "", "default")
	other := seedRunningInstall(t, s, "crm", "proj-1", sdk.Manifest{Name: "crm"}, nil)

	create := func(fields map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"name": "Calls", "slug": "telephony:call.*", "agent_id": 42, "source": "app_event"}
		for k, v := range fields {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(raw))
		req.Header.Set("X-User-ID", itoa64(userID))
		rec := httptest.NewRecorder()
		s.handleCreateSubscription(rec, req)
		return rec
	}

	rec := create(map[string]any{"source_install_id": candidate})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.SourceInstallID != candidate || created.ProjectID != "proj-1" {
		t.Fatalf("created = install %d project %q", created.SourceInstallID, created.ProjectID)
	}
	stored, err := s.store.GetSubscription(userID, created.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored: %v", err)
	}
	event := func(install int64) AppEvent {
		return AppEvent{App: "telephony", ProjectID: "proj-1", InstallID: install, Topic: "call.ended", Data: json.RawMessage(`{}`)}
	}
	if !appSubscriptionMatches(stored, event(candidate)) {
		t.Fatal("event from the pinned install did not match")
	}
	if appSubscriptionMatches(stored, event(stable)) {
		t.Fatal("event from another install of the app matched")
	}

	for name, tc := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"other app":              {map[string]any{"source_install_id": other}, "not an installation of telephony"},
		"other project":          {map[string]any{"source_install_id": candidate, "project_id": "proj-2"}, "in another project"},
		"global without project": {map[string]any{"source_install_id": globalInstall}, "project_id is required"},
		"unknown install":        {map[string]any{"source_install_id": 99999}, "not found"},
		"negative":               {map[string]any{"source_install_id": -1}, "must be positive"},
	} {
		rec := create(tc.fields)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec = create(map[string]any{"source_install_id": globalInstall, "project_id": "proj-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("global source with project: %d %s", rec.Code, rec.Body.String())
	}
}

// Manifest event subscribers bound to one install of the source app receive
// only that install's events in its project; unbound subscribers and other
// projects keep today's delivery.
func TestManifestEventSubscribersFollowBindings(t *testing.T) {
	s := newBusServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	stable := seedRunningInstall(t, s, "telephony", "proj-1", sdk.Manifest{Name: "telephony"}, nil)
	candidate := addInstallOf(t, s, stable, "proj-1", "candidate")
	otherProject := addInstallOf(t, s, stable, "proj-2", "default")

	type delivery struct {
		subscriber string
		source     int64
	}
	received := make(chan delivery, 16)
	newSubscriber := func(name, projectID string, bindings map[string]any) {
		manifest := sdk.Manifest{Name: name, Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "telephony", Events: []string{"call.*"}}}}}
		id := seedRunningInstall(t, s, name, projectID, manifest, bindings)
		sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				SourceInstallID int64 `json:"source_install_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			received <- delivery{subscriber: name, source: body.SourceInstallID}
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(sidecar.Close)
		s.installedApps.Add(&InstalledApp{InstallID: id, AppName: name, ProjectID: projectID, SidecarURL: sidecar.URL, Manifest: manifest})
	}
	newSubscriber("bound", "proj-1", map[string]any{"telephony": stable})
	newSubscriber("unbound", "proj-1", nil)
	newSubscriber("bound-global", "", map[string]any{"telephony": stable})

	collect := func(source int64, project string) map[string]bool {
		s.dispatchAppEventToSubscribers(AppEvent{App: "telephony", ProjectID: project, InstallID: source, Topic: "call.ended", Data: json.RawMessage(`{}`)})
		got := map[string]bool{}
		deadline := time.After(400 * time.Millisecond)
		for {
			select {
			case d := <-received:
				if d.source != source {
					t.Fatalf("%s got source %d, want %d", d.subscriber, d.source, source)
				}
				got[d.subscriber] = true
			case <-deadline:
				return got
			}
		}
	}

	if got := collect(stable, "proj-1"); !got["bound"] || !got["unbound"] || !got["bound-global"] {
		t.Fatalf("bound install's event: %v", got)
	}
	if got := collect(candidate, "proj-1"); got["bound"] || got["bound-global"] || !got["unbound"] {
		t.Fatalf("other install's event: %v", got)
	}
	// The global subscriber chose no install in proj-2, so it still hears it.
	if got := collect(otherProject, "proj-2"); !got["bound-global"] || got["bound"] || got["unbound"] {
		t.Fatalf("other project's event: %v", got)
	}
}

func TestStreamHandler_FiltersBySourceInstall(t *testing.T) {
	s := newBusServer(t)
	seedProject(t, s, "p1")
	// Replay starts after "since"; seq 1 is the cursor the client resumes from.
	s.appBus.Publish("telephony", "p1", 9, "call.ended", json.RawMessage(`{"n":0}`))
	s.appBus.Publish("telephony", "p1", 7, "call.ended", json.RawMessage(`{"n":1}`))
	s.appBus.Publish("telephony", "p1", 8, "call.ended", json.RawMessage(`{"n":2}`))
	s.appBus.Publish("telephony", "p1", 7, "call.ended", json.RawMessage(`{"n":3}`))

	mux := http.NewServeMux()
	mux.HandleFunc("/app-events/", func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		s.handleAppEventStream(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/app-events/telephony?project_id=p1&since=1&source_install_id=7", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.appBus.Publish("telephony", "p1", 8, "call.ended", json.RawMessage(`{"n":4}`))
		s.appBus.Publish("telephony", "p1", 7, "call.ended", json.RawMessage(`{"n":5}`))
	}()
	br := bufio.NewReader(resp.Body)
	for _, want := range []string{`{"n":1}`, `{"n":3}`, `{"n":5}`} {
		frame, err := readSSEFrame(br, time.Second)
		if err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		var ev AppEvent
		if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.InstallID != 7 || string(ev.Data) != want {
			t.Fatalf("frame = install %d data %s, want install 7 data %s", ev.InstallID, ev.Data, want)
		}
	}

	bad := httptest.NewRequest("GET", "/app-events/telephony?project_id=p1&source_install_id=x", nil)
	bad.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppEventStream(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid source_install_id: %d", rec.Code)
	}
}

// An app installed globally publishes events for each project. Subscribers in
// a project, bound to that global install or not bound at all, keep
// receiving them.
func TestManifestEventSubscribersOfGlobalSource(t *testing.T) {
	s := newBusServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	globalSource := seedRunningInstall(t, s, "telephony", "", sdk.Manifest{Name: "telephony"}, nil)

	received := make(chan string, 16)
	newSubscriber := func(name, projectID string, bindings map[string]any) {
		manifest := sdk.Manifest{Name: name, Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "telephony", Events: []string{"call.*"}}}}}
		id := seedRunningInstall(t, s, name, projectID, manifest, bindings)
		sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received <- name
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(sidecar.Close)
		s.installedApps.Add(&InstalledApp{InstallID: id, AppName: name, ProjectID: projectID, SidecarURL: sidecar.URL, Manifest: manifest})
	}
	newSubscriber("follows-global", "proj-1", map[string]any{"telephony": globalSource})
	newSubscriber("unbound", "proj-1", nil)
	newSubscriber("global-subscriber", "", map[string]any{"telephony": globalSource})

	collect := func(project string) map[string]bool {
		s.dispatchAppEventToSubscribers(AppEvent{App: "telephony", ProjectID: project, InstallID: globalSource, Topic: "call.ended", Data: json.RawMessage(`{}`)})
		got := map[string]bool{}
		deadline := time.After(400 * time.Millisecond)
		for {
			select {
			case name := <-received:
				got[name] = true
			case <-deadline:
				return got
			}
		}
	}
	if got := collect("proj-1"); !got["follows-global"] || !got["unbound"] || !got["global-subscriber"] {
		t.Fatalf("global install's event for proj-1: %v", got)
	}
	// Project subscribers only hear their own project, as before.
	if got := collect("proj-2"); !got["global-subscriber"] || got["follows-global"] || got["unbound"] {
		t.Fatalf("global install's event for proj-2: %v", got)
	}
}

// A subscription pinned to a global install hears that install's events for
// the subscription's project.
func TestAppEventSubscriptionPinnedToGlobalSource(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	globalSource := seedRunningInstall(t, s, "telephony", "", sdk.Manifest{Name: "telephony"}, nil)
	projectInstall := addInstallOf(t, s, globalSource, "proj-1", "default")

	raw, _ := json.Marshal(map[string]any{"name": "Calls", "slug": "telephony:call.*", "agent_id": 42, "source": "app_event",
		"project_id": "proj-1", "source_install_id": globalSource})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(raw))
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleCreateSubscription(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	sub, err := s.store.GetSubscription(userID, created.ID)
	if err != nil || sub == nil {
		t.Fatalf("stored: %v", err)
	}
	event := func(install int64, project string) AppEvent {
		return AppEvent{App: "telephony", ProjectID: project, InstallID: install, Topic: "call.ended", Data: json.RawMessage(`{}`)}
	}
	if !appSubscriptionMatches(sub, event(globalSource, "proj-1")) {
		t.Fatal("global install's event for the project did not match")
	}
	if appSubscriptionMatches(sub, event(globalSource, "proj-2")) {
		t.Fatal("global install's event for another project matched")
	}
	if appSubscriptionMatches(sub, event(projectInstall, "proj-1")) {
		t.Fatal("project install's event matched a subscription pinned to the global install")
	}
}
