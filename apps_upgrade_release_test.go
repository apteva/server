package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpgradeExplicitReleaseRejectsAnotherAppBeforeChanges(t *testing.T) {
	s := newTestServer(t)
	s.store.SetPlatformRole(1, PlatformAdmin)
	id := runtimeFixtureInstall(t, s, "release-fixture")
	s.store.db.Exec(`UPDATE app_installs SET source='git' WHERE id=?`, id)
	wrong := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "different-app", Version: "2.0.0", Runtime: sdk.Runtime{Kind: "source", Port: 8080, Source: &sdk.SourceSpec{Repo: "github.com/example/app"}}}
	raw, _ := yaml.Marshal(wrong)
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(raw) }))
	defer release.Close()
	body, _ := json.Marshal(map[string]string{"manifest_url": release.URL + "/selected-release.yaml"})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/apps/installs/%d/upgrade", id), strings.NewReader(string(body)))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleUpgradeApp(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "different app") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var pending string
	s.store.db.QueryRow(`SELECT pending_manifest_json FROM app_installs WHERE id=?`, id).Scan(&pending)
	if pending != "" {
		t.Fatal("rejected release modified install")
	}
}
