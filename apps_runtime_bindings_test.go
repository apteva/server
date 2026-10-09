package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func liveBindingTestInstall(t *testing.T, s *Server, handler http.HandlerFunc) int64 {
	t.Helper()
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	manifest := sdk.Manifest{Name: "live-carrier-test", Version: "1.0.0"}
	manifest.Requires.Integrations = []sdk.IntegrationDep{{Role: "carrier", Kind: "integration", Mode: "multiple", CompatibleSlugs: []string{"carrier-a", "carrier-b"}}}
	manifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: liveBindingsPath, Method: "POST"}}
	id := seedRunningInstall(t, s, manifest.Name, "", manifest, map[string]any{"carrier": map[string]any{"ids": []any{10, 11}, "default_id": 11}})
	sidecar := httptest.NewServer(handler)
	t.Cleanup(sidecar.Close)
	s.installedApps.Add(&InstalledApp{InstallID: id, AppName: manifest.Name, Manifest: manifest, SidecarURL: sidecar.URL, Token: "live-test-token"})
	_, err := s.store.db.Exec(`UPDATE app_installs SET local_pid=12345,sidecar_url_override=? WHERE id=?`, sidecar.URL, id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func liveReply(t *testing.T, w http.ResponseWriter, r *http.Request, ready bool) string {
	t.Helper()
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString(r.Header.Get("X-Apteva-Runtime-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("live-test-token"))
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) || r.Header.Get("Authorization") != "Bearer live-test-token" {
		t.Error("runtime request not authenticated")
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	writeJSON(w, map[string]any{"ready": ready, "change_id": body["change_id"], "draining_connections": []int64{11}, "retained_work": map[bool]int{true: 0, false: 1}[ready]})
	return body["phase"].(string)
}
func TestLiveBindingsDrainRetainsAuthorizationAndRuntime(t *testing.T) {
	s := newTestServer(t)
	var ready atomic.Bool
	id := liveBindingTestInstall(t, s, func(w http.ResponseWriter, r *http.Request) { liveReply(t, w, r, ready.Load()) })
	response := putBindings(s, id, map[string]any{"carrier": map[string]any{"ids": []any{10, 12}, "default_id": 10}})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	var payload map[string]any
	json.Unmarshal(response.Body.Bytes(), &payload)
	if payload["respawned"] != false || payload["pending"] != true {
		t.Fatalf("unexpected update: %s", response.Body)
	}
	effective := readBindings(t, s, id)
	for _, conn := range []int64{10, 11, 12} {
		if !appBindingContains(effective["carrier"], conn) {
			t.Fatalf("effective authorization missing %d", conn)
		}
	}
	_, defaultID := appBindingIDs(effective["carrier"])
	if defaultID != 10 {
		t.Fatal("desired default delayed")
	}
	if _, bound := installBoundConnection(s, id, 11); !bound {
		t.Fatal("existing carrier context revoked")
	}
	var pid int
	s.store.db.QueryRow(`SELECT local_pid FROM app_installs WHERE id=?`, id).Scan(&pid)
	if pid != 12345 {
		t.Fatal("sidecar restarted")
	}
	if overlap := putBindings(s, id, map[string]any{"carrier": nil}); overlap.Code != 409 {
		t.Fatal("overlapping update accepted")
	}
	status := httptest.NewRecorder()
	s.handleGetLiveBindings(status, httptest.NewRequest("GET", "/apps/installs/"+strconv.FormatInt(id, 10)+"/bindings", nil))
	if status.Code != 200 {
		t.Fatal(status.Code)
	}
	lock := s.bindingUpdateLock(id)
	lock.Lock()
	defer lock.Unlock()
	ready.Store(true)
	change := liveBindingChange{ID: id}
	if err := s.store.db.QueryRow(`SELECT change_id,previous_json,desired_json,phase FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&change.ChangeID, &change.Previous, &change.Desired, &change.Phase); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advanceLiveBindings(change); err != nil {
		t.Fatal(err)
	}
	if _, bound := installBoundConnection(s, id, 11); bound {
		t.Fatal("finished drain retained authorization")
	}
	var pending int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&pending)
	if pending != 0 {
		t.Fatal("change not finished")
	}
	s.store.db.QueryRow(`SELECT local_pid FROM app_installs WHERE id=?`, id).Scan(&pid)
	if pid != 12345 {
		t.Fatal("sidecar restarted on commit")
	}
}
func TestLiveBindingsFailureRetainsEffectiveBindings(t *testing.T) {
	s := newTestServer(t)
	id := liveBindingTestInstall(t, s, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "temporary runtime failure", 503) })
	response := putBindings(s, id, map[string]any{"carrier": nil})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	if !appBindingContains(readBindings(t, s, id)["carrier"], 11) {
		t.Fatal("failed prepare removed context")
	}
	var pending int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&pending)
	if pending != 1 {
		t.Fatal("unknown outcome not durable")
	}
	lock := s.bindingUpdateLock(id)
	lock.Lock()
	defer lock.Unlock()
	s.store.db.Exec(`DELETE FROM app_runtime_binding_updates WHERE install_id=?`, id)
}
func TestLiveBindingsExplicitDenialNeverRestarts(t *testing.T) {
	s := newTestServer(t)
	id := liveBindingTestInstall(t, s, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unsupported role", 422) })
	response := putBindings(s, id, map[string]any{"carrier": nil})
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body)
	}
	if !appBindingContains(readBindings(t, s, id)["carrier"], 11) {
		t.Fatal("denied change persisted")
	}
	var pid, pending int
	s.store.db.QueryRow(`SELECT local_pid FROM app_installs WHERE id=?`, id).Scan(&pid)
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&pending)
	if pid != 12345 || pending != 0 {
		t.Fatal("denial restarted or left a pending change")
	}
}
func TestLiveBindingsCommitRecoveryIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	var failCommit atomic.Bool
	failCommit.Store(true)
	id := liveBindingTestInstall(t, s, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		if request["phase"] == "commit" && failCommit.Load() {
			http.Error(w, "lost commit", 503)
			return
		}
		writeJSON(w, map[string]any{"ready": true, "change_id": request["change_id"]})
	})
	response := putBindings(s, id, map[string]any{"carrier": map[string]any{"ids": []any{10}, "default_id": 10}})
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	lock := s.bindingUpdateLock(id)
	lock.Lock()
	defer lock.Unlock()
	change := liveBindingChange{ID: id}
	if err := s.store.db.QueryRow(`SELECT change_id,previous_json,desired_json,phase FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&change.ChangeID, &change.Previous, &change.Desired, &change.Phase); err != nil {
		t.Fatal(err)
	}
	if change.Phase != "commit" {
		t.Fatal("lost acknowledgement did not retain commit phase")
	}
	if appBindingContains(readBindings(t, s, id)["carrier"], 11) {
		t.Fatal("finished work retained old binding")
	}
	failCommit.Store(false)
	if _, err := s.advanceLiveBindings(change); err != nil {
		t.Fatal(err)
	}
	var pending int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&pending)
	if pending != 0 {
		t.Fatal("commit not recovered")
	}
}
func TestLiveBindingsRequiresExplicitAuthenticatedOptIn(t *testing.T) {
	manifest := &sdk.Manifest{}
	for _, route := range []sdk.RouteSpec{{Prefix: liveBindingsPath, NoAuth: true, Method: "POST"}, {Prefix: liveBindingsPath, Method: "GET"}, {Prefix: "/_runtime/", Method: "POST"}} {
		manifest.Provides.HTTPRoutes = []sdk.RouteSpec{route}
		if supportsLiveBindings(manifest) {
			t.Fatal("invalid opt-in accepted")
		}
	}
	manifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: liveBindingsPath, Method: "POST"}}
	if !supportsLiveBindings(manifest) {
		t.Fatal("valid opt-in rejected")
	}
}
