package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUILayoutSurfacePatchPreservesOtherSurfacesAndRejectsStaleRevision(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("widgets@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(user.ID, "Widgets", "", "")
	if err != nil {
		t.Fatal(err)
	}

	patch := func(surface, value, revision string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"value": json.RawMessage(value)})
		req := httptest.NewRequest(http.MethodPatch, "/ui-layout/projects/"+project.ID+"/surfaces/"+surface, bytes.NewReader(body))
		req.Header.Set("X-User-ID", itoa(user.ID))
		if revision != "" {
			req.Header.Set("If-Match", `"`+revision+`"`)
		}
		rec := httptest.NewRecorder()
		s.handleUILayoutSurface(rec, req)
		return rec
	}

	first := patch("dashboard.home", `[{"id":"inbox","component":"native:inbox","size":"half"}]`, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first patch status=%d body=%s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil || firstBody.Revision != 1 {
		t.Fatalf("first response=%s err=%v", first.Body.String(), err)
	}

	second := patch("dashboard.agent_card", `[{"id":"tasks","component":"tasks:agent-tasks","size":"half"}]`, "1")
	if second.Code != http.StatusOK {
		t.Fatalf("second patch status=%d body=%s", second.Code, second.Body.String())
	}
	layout, revision := s.store.GetUserUILayoutWithRevision(user.ID)
	if revision != 2 || !bytes.Contains(layout, []byte("native:inbox")) || !bytes.Contains(layout, []byte("tasks:agent-tasks")) {
		t.Fatalf("layout=%s revision=%d", layout, revision)
	}

	stale := patch("dashboard.home", `[]`, "1")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale patch status=%d body=%s", stale.Code, stale.Body.String())
	}
	layout, revision = s.store.GetUserUILayoutWithRevision(user.ID)
	if revision != 2 || !bytes.Contains(layout, []byte("native:inbox")) {
		t.Fatalf("stale write changed layout=%s revision=%d", layout, revision)
	}
}
