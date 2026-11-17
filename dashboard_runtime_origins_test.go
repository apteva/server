package main

import (
	"reflect"
	"testing"
)

func TestDashboardManagedOriginsFollowCommittedGeneration(t *testing.T) {
	s := newTestServer(t)
	id := seedCORSAppInstall(t, s, "remote-csp", "")
	m := &appRuntimeManager{s: s}
	put := func(d runtimeDeployment) {
		t.Helper()
		if err := m.saveDeployment(d); err != nil {
			t.Fatal(err)
		}
	}
	d := runtimeDeployment{ID: "remote-one", InstallID: id, Generation: 1, Status: "active", Placement: runtimePlacement{Type: "backend"}, Execution: runtimeExecution{BrowserEndpoint: "https://VOICE.example:443/runtime-browser", Endpoint: "https://private-control.example", Endpoints: map[string]string{"media": "https://media.example/stream", "signaling": "https://voice.example/rtc", "callbacks": "https://callbacks.example"}}}
	put(d)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1,runtime_active_deployment=?,status='running',permissions_json='[]' WHERE id=?`, d.ID, id); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://media.example", "https://voice.example"}
	check := func(want []string) {
		t.Helper()
		got, err := s.dashboardConnectOrigins()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("origins=%v want=%v err=%v", got, want, err)
		}
	}
	check(want) // No app-specific registration or dashboard permission needed.
	candidate := d
	candidate.ID = "remote-two"
	candidate.Generation = 2
	candidate.Status = "prepared"
	candidate.Execution = runtimeExecution{BrowserEndpoint: "https://next.example/browser"}
	put(candidate)
	check(want)
	candidate.Status = "active"
	put(candidate)
	check(want) // Not committed, even if backend says ready/active.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET runtime_active_deployment=? WHERE id=?`, candidate.ID, id); err != nil {
		t.Fatal(err)
	}
	check([]string{"https://next.example"}) // Old origin disappears atomically.
	for _, status := range []string{"pending", "stopped", "failed"} {
		s.store.db.Exec(`UPDATE app_installs SET status=? WHERE id=?`, status, id)
		check([]string{})
	}
	s.store.db.Exec(`UPDATE app_installs SET status='running' WHERE id=?`, id)
	candidate.Status = "stopped"
	put(candidate)
	check([]string{})
	candidate.Status = "active"
	candidate.Placement = runtimePlacement{Type: "local"}
	put(candidate)
	check([]string{})
}

func TestDashboardManagedOriginsValidateEndpointsAndReplicas(t *testing.T) {
	s := newTestServer(t)
	id := seedCORSAppInstall(t, s, "remote-csp-validation", "")
	m := &appRuntimeManager{s: s}
	d := runtimeDeployment{ID: "remote-valid", InstallID: id, Generation: 3, Status: "active", Placement: runtimePlacement{Type: "backend"}, Execution: runtimeExecution{BrowserEndpoint: "https://valid.example/browser"}}
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1,runtime_active_deployment=?,status='running',permissions_json='[]' WHERE id=?`, d.ID, id)
	for i, invalid := range []string{"https://user:secret@bad.example/", "https://bad.example/?token=x", "https://bad.example/#x", "http://public.example/", "https://*.example/"} {
		replica := d
		replica.ID = "replica"
		replica.Replica = 1
		replica.Execution = runtimeExecution{BrowserEndpoint: invalid, Endpoints: map[string]string{"media": "https://replica.example/media"}}
		if err := m.saveDeployment(replica); err != nil {
			t.Fatal(err)
		}
		got, err := s.dashboardConnectOrigins()
		if err != nil || !reflect.DeepEqual(got, []string{"https://replica.example", "https://valid.example"}) {
			t.Fatalf("case %d: %v %v", i, got, err)
		}
	}
	// A forged cross-install active pointer cannot authorize another deployment.
	other := seedCORSAppInstall(t, s, "remote-other", "")
	s.store.db.Exec(`UPDATE app_installs SET runtime_active_deployment='',status='stopped' WHERE id=?`, id)
	s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1,runtime_active_deployment=?,status='running' WHERE id=?`, d.ID, other)
	got, err := s.dashboardConnectOrigins()
	if err != nil || len(got) != 0 {
		t.Fatalf("cross-install origins %v %v", got, err)
	}
}
