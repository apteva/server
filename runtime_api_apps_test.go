package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// runtimeAppsByNameServer is a runtime API server whose environment source
// resolver serves one runnable "telephony" package at 1.2.0 and records each
// lookup, so the tests run apps the project never installed.
func runtimeAppsByNameServer(t *testing.T) (*Server, int64, *[]string) {
	t.Helper()
	s := newRuntimeAPITestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	dir := writeEnvironmentRunnableFixture(t, t.TempDir(), "telephony", "1.2.0", "")
	var mu sync.Mutex
	lookups := []string{}
	s.environments.ResolveEnvironmentSource = func(projectID, name, constraint string) (string, error) {
		mu.Lock()
		lookups = append(lookups, projectID+"|"+name+"|"+constraint)
		mu.Unlock()
		if normalizeAppName(name) != "telephony" {
			return "", errNoTestSource(name)
		}
		if err := validateEnvironmentAppVersion("1.2.0", constraint); err != nil {
			return "", err
		}
		return dir, nil
	}
	owner := seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	return s, owner, &lookups
}

type errNoTestSource string

func (e errNoTestSource) Error() string { return "no source for " + string(e) }

func TestRuntimeAPI_AppsByNameRunWithoutProjectInstall(t *testing.T) {
	s, owner, lookups := runtimeAppsByNameServer(t)
	rec := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-by-name", TTLSeconds: 120,
		Apps: []sdk.RuntimeAppSource{{Name: "telephony", Version: ">=1.2.0"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	runtime, ok := s.environments.Get("rt-by-name")
	if !ok {
		t.Fatal("runtime not live")
	}
	inst, ok := runtime.Install("telephony")
	if !ok || inst.InstallID <= 0 {
		t.Fatalf("telephony copy missing: %+v", inst)
	}
	if got := runtime.SourceInstallID("telephony"); got != 0 {
		t.Fatalf("by-name app claims source install %d", got)
	}
	if got := runtime.SourceApps(); len(got) != 1 || got["telephony"] != ">=1.2.0" {
		t.Fatalf("source apps=%v", got)
	}
	if len(*lookups) == 0 || (*lookups)[0] != "proj-1|telephony|>=1.2.0" {
		t.Fatalf("resolver did not get the project and constraint: %v", *lookups)
	}
	var project string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id = ?`, inst.InstallID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if project != "rt-by-name" {
		t.Fatalf("copy installed under project %q, want the runtime's own", project)
	}
	var projectInstalls int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name='telephony' AND i.project_id='proj-1'`).Scan(&projectInstalls); err != nil {
		t.Fatal(err)
	}
	if projectInstalls != 0 {
		t.Fatalf("by-name runtime installed telephony into the project (%d rows)", projectInstalls)
	}
}

func TestRuntimeAPI_AppsByNameRefusals(t *testing.T) {
	s, owner, _ := runtimeAppsByNameServer(t)
	installed := seedRuntimeAPIInstall(t, s, "crm")
	cases := []struct {
		name string
		req  sdk.RuntimeCreateRequest
		want string
	}{
		{"empty name", sdk.RuntimeCreateRequest{Apps: []sdk.RuntimeAppSource{{Name: "  "}}}, "app name required"},
		{"listed twice", sdk.RuntimeCreateRequest{Apps: []sdk.RuntimeAppSource{{Name: "telephony"}, {Name: "Telephony"}}}, "listed twice"},
		{"version not available", sdk.RuntimeCreateRequest{Apps: []sdk.RuntimeAppSource{{Name: "telephony", Version: ">=2.0.0"}}}, "does not satisfy"},
		{"unknown app", sdk.RuntimeCreateRequest{Apps: []sdk.RuntimeAppSource{{Name: "nope"}}}, "no source for nope"},
	}
	for i, tc := range cases {
		tc.req.ID = "rt-refused-" + itoa(int64(i))
		rec := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", tc.req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: status=%d body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if _, ok := s.environments.Get(tc.req.ID); ok {
			t.Fatalf("%s: refused runtime was created", tc.name)
		}
	}

	if _, _, err := s.addRuntimeAppsByName("proj-1", map[string]string{"telephony": t.TempDir()}, []sdk.RuntimeAppSource{{Name: "telephony"}}); err == nil || !strings.Contains(err.Error(), "also in app_install_ids") {
		t.Fatalf("app from both an install and by name: err=%v", err)
	}

	tooMany := make([]sdk.RuntimeAppSource, maxRuntimeApps)
	for i := range tooMany {
		tooMany[i] = sdk.RuntimeAppSource{Name: "app-" + itoa(int64(i))}
	}
	rec := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-too-many", AppInstallIDs: []int64{installed}, Apps: tooMany})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("apps + installs over the limit: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeAPI_SnapshotRestoresAppsAddedByName(t *testing.T) {
	s, owner, lookups := runtimeAppsByNameServer(t)
	rec := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-snap-src", TTLSeconds: 120, Apps: []sdk.RuntimeAppSource{{Name: "telephony", Version: "1.2.0"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	snap := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-snap-src/snapshots", sdk.RuntimeSnapshotRequest{ID: "snap-by-name"})
	if snap.Code != http.StatusCreated {
		t.Fatalf("snapshot status=%d body=%s", snap.Code, snap.Body.String())
	}
	man, err := s.environments.Snapshots().Get("snap-by-name")
	if err != nil {
		t.Fatal(err)
	}
	if man.SourceApps["telephony"] != "1.2.0" || len(man.SourceInstallIDs) != 0 {
		t.Fatalf("snapshot sources: apps=%v installs=%v", man.SourceApps, man.SourceInstallIDs)
	}
	*lookups = nil
	restored := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-snap-restored", TTLSeconds: 120, SnapshotID: "snap-by-name"})
	if restored.Code != http.StatusCreated {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
	runtime, ok := s.environments.Get("rt-snap-restored")
	if !ok {
		t.Fatal("restored runtime not live")
	}
	if _, ok := runtime.Install("telephony"); !ok {
		t.Fatal("restored runtime lost the app added by name")
	}
	if got := runtime.SourceApps(); got["telephony"] != "1.2.0" {
		t.Fatalf("restored source apps=%v", got)
	}
	if len(*lookups) == 0 || (*lookups)[0] != "proj-1|telephony|1.2.0" {
		t.Fatalf("restore did not resolve with the recorded constraint: %v", *lookups)
	}
}
