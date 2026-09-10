package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
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

// A stale registry icon must not override the canonical manifest icon used
// by the Apps page. Keep both the resolved URL and the theme rendering style.
func TestTemplateLogosUseMarketplaceManifestIdentity(t *testing.T) {
	registryCacheMu.Lock()
	previous := registryCache
	registryCache = registryCacheEntry{registry: &CuratedRegistry{Apps: []RegistryEntry{
		{Name: "storage", DisplayName: "Storage", Icon: "https://example.test/stale.png", ManifestURL: "https://example.test/storage/apteva.yaml"},
		{Name: "fallback", DisplayName: "Fallback", Icon: "https://example.test/fallback.svg", IconStyle: "monochrome"},
	}}, fetched: time.Now()}
	registryCacheMu.Unlock()
	t.Cleanup(func() {
		registryCacheMu.Lock()
		registryCache = previous
		registryCacheMu.Unlock()
	})
	url := "https://example.test/storage/apteva.yaml"
	manifestCacheMu.Lock()
	old, existed := manifestCache[url]
	manifestCache[url] = manifestCacheEntry{manifest: &sdk.Manifest{Icon: "/ui/icon.svg", IconStyle: "monochrome"}, fetched: time.Now()}
	manifestCacheMu.Unlock()
	t.Cleanup(func() {
		manifestCacheMu.Lock()
		if existed {
			manifestCache[url] = old
		} else {
			delete(manifestCache, url)
		}
		manifestCacheMu.Unlock()
	})
	s := &Server{}
	template := AgentTemplate{Requirements: []Requirement{
		{Kind: "app", Slug: "storage", Required: true},
		{Kind: "app", Slug: "fallback"},
		{Kind: "app", Slug: "unknown"},
	}}
	s.resolveTemplateLogos(&template)
	if len(template.ResolvedLogos) != 3 {
		t.Fatalf("logos=%+v", template.ResolvedLogos)
	}
	logo := template.ResolvedLogos[0]
	if logo.IconURL != "https://example.test/storage/ui/icon.svg" || logo.IconStyle != "monochrome" {
		t.Fatalf("manifest identity lost: %+v", logo)
	}
	if fallback := template.ResolvedLogos[1]; fallback.IconURL != "https://example.test/fallback.svg" || fallback.IconStyle != "monochrome" {
		t.Fatalf("registry fallback lost: %+v", fallback)
	}
	if unknown := template.ResolvedLogos[2]; unknown.Label != "unknown" || unknown.IconURL != "" {
		t.Fatalf("missing app fallback lost: %+v", unknown)
	}
	encoded, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"icon_style":"monochrome"`) {
		t.Fatalf("icon style missing from API JSON: %s", encoded)
	}
}
