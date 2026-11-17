package main

import (
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallbackAgentAppearanceDirectoryAndDetail(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "Coder", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	icon, color := "code", "accent"
	if err := s.store.updateAgentIdentity(agent.ID, nil, &icon, &color); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "appearance-test"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermInstancesRead}
	installID := seedInstallWithBindings(t, s, "appearance-test", manifest, nil)
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, agent.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/callback/agents?project_id=proj-1", "/apps/callback/agents/" + itoa(agent.ID)} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var got sdk.PlatformInstance
		if path == "/apps/callback/agents?project_id=proj-1" {
			var agents []sdk.PlatformInstance
			if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range agents {
				if candidate.ID == agent.ID {
					got = candidate
				}
			}
		} else if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != agent.ID || got.Icon != "code" || got.IconColor != "accent" || (path == "/apps/callback/agents?project_id=proj-1" && !got.AttachedToCaller) {
			t.Fatalf("%s: identity=%+v", path, got)
		}
	}
}
