package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTemplateOAuthInviteCreatesSeparateConnectionWithRequiredStaticCredentials(t *testing.T) {
	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	s.port = "5280"
	if _, err := s.store.CreateUser("invite@test.local", "hash"); err != nil {
		t.Fatal(err)
	}

	required := true
	optional := false
	app := &AppTemplate{
		Slug: "google-ads-test",
		Name: "Google Ads Test",
		Auth: AppAuthConfig{
			Types: []string{"bearer", "oauth2"},
			CredentialFields: []CredentialField{
				{Name: "developer_token", Label: "Developer token", Source: "user", Required: &required},
				{Name: "manager_customer_id", Label: "Manager customer ID", Source: "user", Required: &optional},
				{Name: "refresh_token", Label: "Refresh token", Source: "oauth", Hidden: true, Required: &optional},
			},
			OAuth2: &OAuthConfig{
				AuthorizeURL:     "https://accounts.example.test/oauth",
				TokenURL:         "https://accounts.example.test/token",
				ClientIDRequired: true,
			},
		},
	}
	s.catalog = NewAppCatalog()
	s.catalog.apps[app.Slug] = app

	sourceCredentials, _ := json.Marshal(map[string]string{
		"client_id":           "oauth-client",
		"client_secret":       "oauth-secret",
		"developer_token":     "developer-secret",
		"manager_customer_id": "1234567890",
		"refresh_token":       "source-refresh-token",
	})
	encrypted, err := Encrypt(s.secret, string(sourceCredentials))
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         1,
		AppSlug:        app.Slug,
		AppName:        app.Name,
		Name:           "Configured Google Ads",
		AuthType:       "oauth2",
		EncryptedCreds: encrypted,
		ProjectID:      "project-a",
		Status:         "active",
	})
	if err != nil {
		t.Fatal(err)
	}

	createBody, _ := json.Marshal(map[string]any{
		"app_slug":               "ignored-by-template",
		"project_id":             "wrong-project",
		"template_connection_id": source.ID,
		"ttl_seconds":            3600,
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/invites", bytes.NewReader(createBody))
	createReq.Header.Set("X-User-ID", "1")
	createRec := httptest.NewRecorder()
	s.handleCreateInvite(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create invite status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	payload, err := s.verifyInvite(created.Token)
	if err != nil {
		t.Fatal(err)
	}
	if payload.TemplateConnID != source.ID || payload.App != app.Slug || payload.Proj != "project-a" {
		t.Fatalf("template invite was not locked to source connection: %#v", payload)
	}

	fulfillReq := httptest.NewRequest(
		http.MethodPost,
		"/connect/"+created.Token+"/fulfill",
		strings.NewReader(`{}`),
	)
	fulfillRec := httptest.NewRecorder()
	s.handleFulfillInvite(fulfillRec, fulfillReq)
	if fulfillRec.Code != http.StatusOK {
		t.Fatalf("fulfill status=%d body=%s", fulfillRec.Code, fulfillRec.Body.String())
	}
	var fulfilled struct {
		Status       string `json:"status"`
		ConnectionID int64  `json:"connection_id"`
		RedirectURL  string `json:"redirect_url"`
	}
	if err := json.Unmarshal(fulfillRec.Body.Bytes(), &fulfilled); err != nil {
		t.Fatal(err)
	}
	if fulfilled.Status != "redirect" || fulfilled.ConnectionID == 0 || fulfilled.ConnectionID == source.ID {
		t.Fatalf("unexpected fulfill response: %#v", fulfilled)
	}
	if !strings.Contains(fulfilled.RedirectURL, "client_id=oauth-client") {
		t.Fatalf("OAuth client was not reused: %s", fulfilled.RedirectURL)
	}

	connection, encryptedNew, err := s.store.GetConnection(1, fulfilled.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.ProjectID != "project-a" || connection.Status != "pending" || connection.AutoMCP {
		t.Fatalf("unexpected new connection: %#v", connection)
	}
	plaintext, err := Decrypt(s.secret, encryptedNew)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]string
	if err := json.Unmarshal([]byte(plaintext), &copied); err != nil {
		t.Fatal(err)
	}
	if copied["developer_token"] != "developer-secret" {
		t.Fatalf("required developer token was not copied: %#v", copied)
	}
	if copied["client_id"] != "oauth-client" || copied["client_secret"] != "oauth-secret" {
		t.Fatalf("OAuth app credentials were not preserved: %#v", copied)
	}
	if _, ok := copied["manager_customer_id"]; ok {
		t.Fatalf("optional manager routing must not be copied: %#v", copied)
	}
	if _, ok := copied["refresh_token"]; ok {
		t.Fatalf("source OAuth credentials must not be copied: %#v", copied)
	}
}

func TestTemplateInviteRejectsConnectionUpdateCombination(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.store.CreateUser("invite-combination@test.local", "hash"); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{
		"app_slug":"google-ads",
		"connection_id":12,
		"template_connection_id":13
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/invites", body)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleCreateInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
