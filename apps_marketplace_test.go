package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
)

func TestMarketplaceSupportsSearchCategoryAndPagination(t *testing.T) {
	s := newTestServer(t)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"schema":"apteva-app-registry/v1","apps":[
			{"name":"media-alpha","display_name":"Media Alpha","version":"1.0.0","description":"video rendering","author":"Apteva","repo":"github.com/apteva/apps","tags":["video"],"category":"media","official":true},
			{"name":"media-beta","display_name":"Media Beta","version":"1.0.0","description":"image and video tools","author":"Apteva","repo":"github.com/apteva/apps","tags":["image","video"],"category":"media","official":true},
			{"name":"crm-sync","display_name":"CRM Sync","version":"1.0.0","description":"contacts","author":"Apteva","repo":"github.com/apteva/apps","tags":["crm"],"category":"business","official":true},
			{"name":"deploy","display_name":"Deploy","version":"1.0.0","description":"hosting","author":"Apteva","repo":"github.com/apteva/apps","tags":["hosting"],"category":"devops","official":true}
		]}`))
	}))
	defer registry.Close()

	req := authedRequest(t, http.MethodGet,
		"/apps/marketplace?registry_url="+url.QueryEscape(registry.URL)+"&q=video&category=media&page=2&page_size=1",
		"", nil)
	rec := httptest.NewRecorder()
	s.handleMarketplace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Apps       []RegistryEntry `json:"apps"`
		Total      int             `json:"total"`
		Page       int             `json:"page"`
		PageSize   int             `json:"page_size"`
		Categories map[string]int  `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 2 || out.Page != 2 || out.PageSize != 1 {
		t.Fatalf("unexpected paging metadata: %+v", out)
	}
	if len(out.Apps) != 1 || out.Apps[0].Name != "media-beta" {
		t.Fatalf("unexpected page apps: %+v", out.Apps)
	}
	if out.Categories["media"] != 2 {
		t.Fatalf("unexpected categories: %+v", out.Categories)
	}
}

func TestMarketplaceOmitsInternalFrameworkApps(t *testing.T) {
	s := newTestServer(t)
	appRegistry := framework.NewRegistry(s.store.db, slog.Default())
	if err := appRegistry.Load(channelchat.New(&serverResolver{srv: s})); err != nil {
		t.Fatalf("load channel-chat: %v", err)
	}
	s.apps = appRegistry
	t.Cleanup(func() { appRegistry.Stop(100 * time.Millisecond) })

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"schema":"apteva-app-registry/v1","apps":[
			{"name":"channel-chat","display_name":"Channel Chat","version":"1.0.0","description":"internal chat bridge","category":"channels"},
			{"name":"tasks","display_name":"Tasks","version":"1.0.0","description":"task management","category":"productivity"}
		]}`))
	}))
	defer registry.Close()

	req := authedRequest(t, http.MethodGet,
		"/apps/marketplace?registry_url="+url.QueryEscape(registry.URL),
		"", nil)
	rec := httptest.NewRecorder()
	s.handleMarketplace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Apps       []RegistryEntry `json:"apps"`
		Total      int             `json:"total"`
		Categories map[string]int  `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Apps) != 1 || out.Apps[0].Name != "tasks" {
		t.Fatalf("internal app leaked into marketplace: %+v", out)
	}
	if out.Categories["channels"] != 0 {
		t.Fatalf("internal category count leaked: %+v", out.Categories)
	}
}
