package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuiltinAgentTemplatesExposeHighlightsAndAreReadOnly(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES(2,'templates@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}

	list, err := s.store.ListAgentTemplates(2)
	if err != nil {
		t.Fatal(err)
	}
	var github *AgentTemplate
	for i := range list {
		if list[i].ID == "github-helper" {
			github = &list[i]
			break
		}
	}
	if github == nil || len(github.Highlights) < 2 {
		t.Fatalf("github template is missing capability highlights: %+v", github)
	}

	req := authedRequest(t, http.MethodPut, "/agent-templates/github-helper", "", map[string]any{
		"name": "Changed", "directive": "Changed globally.", "mode": "learn",
	})
	req.Header.Set("X-User-ID", "2")
	w := httptest.NewRecorder()
	s.handleAgentTemplateByID(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("builtin update status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestUserAgentTemplateHighlightsRoundTrip(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	req := authedRequest(t, http.MethodPost, "/agent-templates", "", map[string]any{
		"name": "Release notes", "directive": "Prepare release notes.", "mode": "cautious",
		"highlights": []string{"Summarize reviewed changes", "Prepare a publishable draft"},
	})
	w := httptest.NewRecorder()
	s.handleCreateAgentTemplate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created AgentTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Highlights) != 2 || created.Highlights[0] != "Summarize reviewed changes" {
		t.Fatalf("highlights=%v", created.Highlights)
	}
}
