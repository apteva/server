package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createHelperCapabilityTestUser(t *testing.T, s *Server) int64 {
	t.Helper()
	user, err := s.store.CreateUser("helper-capabilities@test.local", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt); err != nil {
		t.Fatalf("activate helper fixture: %v", err)
	}
	return user.ID
}

func putHelperCapabilities(t *testing.T, s *Server, userID int64, ids []int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"mcp_server_ids": ids})
	req := httptest.NewRequest(http.MethodPut, "/platform/helper/capabilities", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", fmt.Sprint(userID))
	rec := httptest.NewRecorder()
	s.handlePlatformHelperCapabilities(rec, req)
	return rec
}

func TestPlatformHelperCapabilitiesAcceptOnlyGlobalAppsAndIntegrations(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	userID := createHelperCapabilityTestUser(t, s)

	globalApp, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "global-app", Description: "Global app",
		Source: "app", Transport: "http", URL: "http://127.0.0.1:5280/api/apps/global/mcp",
	})
	if err != nil {
		t.Fatalf("create global app MCP: %v", err)
	}
	globalIntegration, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "global-integration", Description: "Global integration",
		Source: "local", Transport: "http", ConnectionID: 91,
	})
	if err != nil {
		t.Fatalf("create global integration MCP: %v", err)
	}
	projectApp, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "project-app", Description: "Project app",
		Source: "app", Transport: "http", URL: "http://127.0.0.1:5280/api/apps/project/mcp",
		ProjectID: "project-a",
	})
	if err != nil {
		t.Fatalf("create project app MCP: %v", err)
	}
	globalCustom, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "global-custom", Description: "Global custom",
		Source: "custom", Transport: "stdio", Command: "custom-mcp",
	})
	if err != nil {
		t.Fatalf("create global custom MCP: %v", err)
	}

	rejected := putHelperCapabilities(t, s, userID, []int64{globalApp.ID, projectApp.ID})
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "project-scoped") {
		t.Fatalf("project-scoped capability response=%d body=%s", rejected.Code, rejected.Body.String())
	}
	rejected = putHelperCapabilities(t, s, userID, []int64{globalCustom.ID})
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "not a global app or integration") {
		t.Fatalf("custom capability response=%d body=%s", rejected.Code, rejected.Body.String())
	}

	accepted := putHelperCapabilities(t, s, userID, []int64{globalIntegration.ID, globalApp.ID, globalApp.ID})
	if accepted.Code != http.StatusOK {
		t.Fatalf("global capabilities response=%d body=%s", accepted.Code, accepted.Body.String())
	}
	var response platformHelperCapabilitiesResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Applied || len(response.SelectedMCPServerIDs) != 2 ||
		response.SelectedMCPServerIDs[0] != globalApp.ID ||
		response.SelectedMCPServerIDs[1] != globalIntegration.ID {
		t.Fatalf("unexpected capability response: %+v", response)
	}

	helper, err := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatalf("load helper: %v", err)
	}
	var cfg struct {
		IncludeAptevaServer bool    `json:"include_apteva_server"`
		IncludeChannels     bool    `json:"include_channels"`
		SelectedIDs         []int64 `json:"helper_global_mcp_server_ids"`
		MCPServers          []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(helper.Config), &cfg); err != nil {
		t.Fatalf("decode helper config: %v", err)
	}
	if !cfg.IncludeAptevaServer || !cfg.IncludeChannels {
		t.Fatalf("mandatory helper MCP flags were lost: %s", helper.Config)
	}
	if len(cfg.SelectedIDs) != 2 {
		t.Fatalf("selected IDs=%v", cfg.SelectedIDs)
	}
	names := map[string]string{}
	for _, entry := range cfg.MCPServers {
		names[entry.Name] = entry.URL
	}
	if _, ok := names["environments"]; !ok {
		t.Fatalf("environments missing from helper MCPs: %s", helper.Config)
	}
	if names["global-app"] != globalApp.URL {
		t.Fatalf("global app config=%q want=%q", names["global-app"], globalApp.URL)
	}
	if names["global-integration"] != fmt.Sprintf("http://127.0.0.1:5280/mcp/%d", globalIntegration.ID) {
		t.Fatalf("global integration config=%q", names["global-integration"])
	}
	if _, ok := names["project-app"]; ok {
		t.Fatalf("project-scoped MCP leaked into helper config: %s", helper.Config)
	}

	diskConfig, err := os.ReadFile(filepath.Join(s.agents.InstanceDir(helper.ID), "config.json"))
	if err != nil {
		t.Fatalf("read stopped helper config: %v", err)
	}
	if strings.Contains(string(diskConfig), "project-app") || !strings.Contains(string(diskConfig), "global-app") {
		t.Fatalf("unexpected stopped helper disk config: %s", diskConfig)
	}
}

func TestPlatformHelperCapabilityReconciliationPrunesMissingAndProjectRows(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	userID := createHelperCapabilityTestUser(t, s)
	globalApp, _ := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "global-app", Source: "app", Transport: "http",
		URL: "http://127.0.0.1:5280/api/apps/global/mcp",
	})
	projectApp, _ := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: "project-app", Source: "app", Transport: "http",
		URL: "http://127.0.0.1:5280/api/apps/project/mcp", ProjectID: "project-a",
	})
	helper, err := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	setHelperSelectedGlobalMCPServerIDs(helper, []int64{globalApp.ID, projectApp.ID, 999999})
	changed, err := s.ensurePlatformHelperRuntimeConfig(helper)
	if err != nil {
		t.Fatalf("reconcile helper: %v", err)
	}
	if !changed {
		t.Fatal("expected invalid capability selection to change helper config")
	}
	ids := helperSelectedGlobalMCPServerIDs(helper)
	if len(ids) != 1 || ids[0] != globalApp.ID {
		t.Fatalf("reconciled IDs=%v want=[%d]", ids, globalApp.ID)
	}
	if strings.Contains(helper.Config, "project-app") {
		t.Fatalf("project MCP survived reconciliation: %s", helper.Config)
	}
}

func TestGenericAgentConfigCannotReplacePlatformHelperMCPs(t *testing.T) {
	s := newTestServer(t)
	userID := createHelperCapabilityTestUser(t, s)
	helper, err := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	for name, body := range map[string]string{
		"top-level MCP list":     `{"mcp_servers":[{"name":"project-leak","transport":"http","url":"http://example.test/mcp"}]}`,
		"legacy config envelope": `{"config":"{\"mcp_servers\":[{\"name\":\"project-leak\",\"transport\":\"http\",\"url\":\"http://example.test/mcp\"}]}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPut,
				fmt.Sprintf("/instances/%d/config", helper.ID),
				strings.NewReader(body),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-ID", fmt.Sprint(userID))
			rec := httptest.NewRecorder()
			s.handleUpdateConfig(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("response=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
