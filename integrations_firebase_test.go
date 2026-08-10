package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func embeddedFirebaseCatalog(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/firebase-cloud-messaging.json")
	if err != nil {
		t.Fatalf("read embedded Firebase catalog: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode embedded Firebase catalog: %v", err)
	}
	return &app
}

func firebaseTool(t *testing.T, app *AppTemplate, name string) *AppToolDef {
	t.Helper()
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app.Tools[i]
		}
	}
	t.Fatalf("Firebase tool %q is missing", name)
	return nil
}

func TestEmbeddedFirebaseCatalogOAuthAndRoutes(t *testing.T) {
	app := embeddedFirebaseCatalog(t)
	if app.Slug != "firebase-cloud-messaging" {
		t.Fatalf("slug=%q", app.Slug)
	}
	if len(app.Auth.Types) != 1 || app.Auth.Types[0] != "oauth2" {
		t.Fatalf("auth types=%v", app.Auth.Types)
	}
	if app.Auth.OAuth2 == nil {
		t.Fatal("OAuth config is missing")
	}
	wantScopes := map[string]bool{
		"https://www.googleapis.com/auth/firebase":           true,
		"https://www.googleapis.com/auth/firebase.messaging": true,
		"https://www.googleapis.com/auth/cloud-platform":     true,
	}
	for _, scope := range app.Auth.OAuth2.Scopes {
		delete(wantScopes, scope)
	}
	if len(wantScopes) != 0 {
		t.Fatalf("missing OAuth scopes=%v", wantScopes)
	}
	if app.HealthCheck == nil || app.HealthCheck.Tool != "list_projects" {
		t.Fatalf("health check=%+v", app.HealthCheck)
	}
	if len(app.Tools) != 8 {
		t.Fatalf("tool count=%d want=8", len(app.Tools))
	}

	wantRoutes := map[string]string{
		"list_projects":      "GET /v1beta1/projects",
		"list_android_apps":  "GET /v1beta1/projects/{project_id}/androidApps",
		"create_android_app": "POST /v1beta1/projects/{project_id}/androidApps",
		"get_android_config": "GET /v1beta1/projects/{project_id}/androidApps/{app_id}/config",
		"get_operation":      "GET /v1beta1/operations/{operation_id}",
		"list_android_sha":   "GET /v1beta1/projects/{project_id}/androidApps/{app_id}/sha",
		"add_android_sha":    "POST /v1beta1/projects/{project_id}/androidApps/{app_id}/sha",
		"send_message":       "POST /v1/projects/{project_id}/messages:send",
	}
	seen := make(map[string]bool, len(app.Tools))
	for i := range app.Tools {
		tool := &app.Tools[i]
		if seen[tool.Name] {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		if got := tool.Method + " " + tool.Path; got != wantRoutes[tool.Name] {
			t.Fatalf("route %s=%q want=%q", tool.Name, got, wantRoutes[tool.Name])
		}
	}
	if got := firebaseTool(t, app, "send_message").BaseURL; got != "https://fcm.googleapis.com" {
		t.Fatalf("send base URL=%q", got)
	}
}

func TestFirebaseOAuthExecutionUsesManagementAndFCMRequests(t *testing.T) {
	type capturedRequest struct {
		method        string
		uri           string
		authorization string
		body          string
	}
	requests := make([]capturedRequest, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, capturedRequest{
			method:        r.Method,
			uri:           r.RequestURI,
			authorization: r.Header.Get("Authorization"),
			body:          string(body),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	app := embeddedFirebaseCatalog(t)
	app.BaseURL = upstream.URL
	listProjects := firebaseTool(t, app, "list_projects")
	sendMessage := firebaseTool(t, app, "send_message")
	sendMessage.BaseURL = upstream.URL
	credentials := map[string]string{"access_token": "oauth-access-token"}

	if _, err := executeIntegrationTool(app, listProjects, credentials, map[string]any{
		"pageSize":    10,
		"pageToken":   "next-page",
		"showDeleted": false,
	}, ""); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if _, err := executeIntegrationTool(app, sendMessage, credentials, map[string]any{
		"project_id": "firebase-project",
		"message": map[string]any{
			"fid":  "installation-id",
			"data": map[string]any{"type": "test"},
		},
		"validate_only": true,
	}, ""); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("request count=%d want=2", len(requests))
	}
	if requests[0].method != http.MethodGet ||
		requests[0].uri != "/v1beta1/projects?pageSize=10&pageToken=next-page&showDeleted=false" {
		t.Fatalf("management request=%+v", requests[0])
	}
	if requests[0].authorization != "Bearer oauth-access-token" {
		t.Fatalf("management authorization=%q", requests[0].authorization)
	}
	if requests[1].method != http.MethodPost ||
		requests[1].uri != "/v1/projects/firebase-project/messages:send" {
		t.Fatalf("FCM request=%+v", requests[1])
	}
	if requests[1].authorization != "Bearer oauth-access-token" {
		t.Fatalf("FCM authorization=%q", requests[1].authorization)
	}
	if strings.Contains(requests[1].body, "project_id") {
		t.Fatalf("path parameter leaked into request body: %s", requests[1].body)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(requests[1].body), &body); err != nil {
		t.Fatalf("decode FCM body: %v", err)
	}
	message, _ := body["message"].(map[string]any)
	if message["fid"] != "installation-id" || body["validate_only"] != true {
		t.Fatalf("FCM body=%v", body)
	}
}
