package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestCallbackAgentDirectoryDiscoversOnlyExplicitlyAttachedOwnedHelper(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	other, _ := s.store.CreateUser("foreign-setup-helper@test.local", "hash")
	helper, _ := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	foreign, _ := s.store.GetOrCreatePlatformHelper(other.ID, platformHelperSystemPrompt)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "setup-chat"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermInstancesRead}
	installID := seedInstallWithBindings(t, s, "setup-chat", manifest, nil)
	s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID)
	list := func() []sdk.PlatformInstance {
		r := helperLifecycleRequest(http.MethodGet, "/apps/callback/agents?project_id=proj-1", userID, "")
		r.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		w := httptest.NewRecorder()
		s.handleAppCallback(w, r)
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		var out []sdk.PlatformInstance
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if len(list()) != 0 {
		t.Fatal("unbound Helper was discoverable")
	}
	for _, id := range []int64{helper.ID, foreign.ID} {
		s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, id)
	}
	out := list()
	if len(out) != 1 || out[0].ID != helper.ID || !out[0].AttachedToCaller {
		t.Fatalf("directory: %+v", out)
	}
	ordinary, _ := s.store.ListAgents(userID, "")
	if len(ordinary) != 0 {
		t.Fatal("Helper leaked into ordinary agent roster")
	}
	setPlatformHelperActivated(helper, false)
	s.store.UpdateAgent(helper)
	if len(list()) != 0 {
		t.Fatal("inactive Helper was discoverable")
	}
	setPlatformHelperActivated(helper, true)
	s.store.UpdateAgent(helper)
	s.store.db.Exec(`UPDATE app_installs SET project_id='proj-1' WHERE id=?`, installID)
	if len(list()) != 0 {
		t.Fatal("project install discovered global Helper")
	}
}
