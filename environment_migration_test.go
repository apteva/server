package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestLegacyEnvironmentMigrationImportsStoppedDefinitions(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	var payload struct {
		MigrationID string `json:"migration_id"`
		Definitions []struct {
			ID           string `json:"id"`
			DesiredState string `json:"desired_state"`
			Spec         struct {
				AppInstallIDs []int64                `json:"app_install_ids"`
				NetworkMode   sdk.RuntimeNetworkMode `json:"network_mode"`
				Seeds         []sdkRuntimeSeedStep   `json:"seeds"`
			} `json:"spec"`
		} `json:"definitions"`
	}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/import/legacy" || r.Header.Get("Authorization") != "Bearer app-token" {
			t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, map[string]any{"imported": len(payload.Definitions)})
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 91, AppName: "environments", ProjectID: "proj-1", SidecarURL: sidecar.URL, Token: "app-token"})
	appID := seedRunningInstall(t, s, "crm", "proj-1", sdk.Manifest{Name: "crm"}, nil)
	if err := s.store.CreateEnvironmentRecord(EnvironmentRecord{ID: "legacy-one", ProjectID: "proj-1", Name: "Legacy one", Mode: "block", Status: "stopped", CreatedBy: 1, SpecJSON: `{"app_install_ids":[` + itoa(appID) + `],"network_mode":"record","seed_plan":[{"app":"crm","tool":"contact_create","input":{"name":"Ada"}}]}`}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/environments/migrate-to-app", strings.NewReader(`{"project_id":"proj-1"}`))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleEnvironmentByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if payload.MigrationID == "" || len(payload.Definitions) != 1 {
		t.Fatalf("payload=%#v", payload)
	}
	got := payload.Definitions[0]
	if got.ID != "legacy-one" || got.DesiredState != "stopped" || got.Spec.NetworkMode != sdk.RuntimeNetworkRecord || len(got.Spec.AppInstallIDs) != 1 || len(got.Spec.Seeds) != 1 {
		t.Fatalf("definition=%#v", got)
	}
}
