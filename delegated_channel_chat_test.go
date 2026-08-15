package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateDelegatedChatCredentialAndEnforceRESTBoundary(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "external-chat-owner@example.com")
	project, err := s.store.CreateProject(user, "Website", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user, "Website Support", "Help customers", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := "sk-" + generateToken(24)
	if _, err := s.store.CreateAPIKey(user, "backend", HashAPIKey(privateKey), privateKey[:11]); err != nil {
		t.Fatal(err)
	}

	body := `{
		"project_id":` + quoteJSON(project.ID) + `,
		"subject_type":"website_user",
		"subject_id":"customer-123",
		"agent_id":` + itoa64(agent.ID) + `,
		"allowed_origins":["https://shop.example"],
		"conversation_directive":"Help this visitor choose a subscription.",
		"expires_in":3600
	}`
	mintReq := httptest.NewRequest(http.MethodPost, "/auth/delegated-users", strings.NewReader(body))
	mintReq.Header.Set("Authorization", "Bearer "+privateKey)
	mint := httptest.NewRecorder()
	s.authMiddleware(s.handleCreateDelegatedUser)(mint, mintReq)
	if mint.Code != http.StatusOK {
		t.Fatalf("mint status=%d body=%s", mint.Code, mint.Body.String())
	}
	var token struct {
		AccessToken     string  `json:"access_token"`
		ProjectID       string  `json:"project_id"`
		AllowedAgentIDs []int64 `json:"allowed_agent_ids"`
		Subject         struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"subject"`
	}
	if err := json.NewDecoder(mint.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.AccessToken, "uk_") || token.ProjectID != project.ID || token.Subject.ID != "customer-123" || len(token.AllowedAgentIDs) != 1 || token.AllowedAgentIDs[0] != agent.ID {
		t.Fatalf("mint response=%+v", token)
	}

	var seenSubject, seenProject, seenScopes string
	next := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		seenSubject = r.Header.Get("X-Apteva-Subject-ID")
		seenProject = r.Header.Get("X-Apteva-Project-ID")
		seenScopes = r.Header.Get("X-Apteva-Scopes")
		w.WriteHeader(http.StatusNoContent)
	})
	authorized := httptest.NewRequest(http.MethodPost, "/apps/channel-chat/chats", strings.NewReader(`{"agent_id":1}`))
	authorized.Header.Set("Authorization", "Bearer "+token.AccessToken)
	authorized.Header.Set("Origin", "https://shop.example")
	rec := httptest.NewRecorder()
	next(rec, authorized)
	if rec.Code != http.StatusNoContent || seenSubject != "customer-123" || seenProject != project.ID || !strings.Contains(seenScopes, "chat.create") {
		t.Fatalf("authorized status=%d subject=%q project=%q scopes=%q body=%s", rec.Code, seenSubject, seenProject, seenScopes, rec.Body.String())
	}

	wrongOrigin := httptest.NewRequest(http.MethodGet, "/apps/channel-chat/chats?agent_id="+itoa64(agent.ID), nil)
	wrongOrigin.Header.Set("Authorization", "Bearer "+token.AccessToken)
	wrongOrigin.Header.Set("Origin", "https://evil.example")
	blocked := httptest.NewRecorder()
	next(blocked, wrongOrigin)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("wrong origin status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	// Network clients cannot spoof another subject through trusted headers.
	spoof := httptest.NewRequest(http.MethodGet, "/apps/channel-chat/chats?agent_id="+itoa64(agent.ID), nil)
	spoof.Header.Set("Authorization", "Bearer "+token.AccessToken)
	spoof.Header.Set("Origin", "https://shop.example")
	spoof.Header.Set("X-Apteva-Subject-ID", "customer-evil")
	seenSubject = ""
	spoofed := httptest.NewRecorder()
	next(spoofed, spoof)
	if spoofed.Code != http.StatusNoContent || seenSubject != "customer-123" {
		t.Fatalf("subject spoof status=%d resolved=%q", spoofed.Code, seenSubject)
	}
}

func TestDelegatedChatCredentialExpiryAndDynamicCORS(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "external-chat-expiry@example.com")
	project, err := s.store.CreateProject(user, "Website", "", "")
	if err != nil {
		t.Fatal(err)
	}
	origins := `["https://chat.example"]`
	scopes := `[{"type":"app_user","app":"channel-chat","actions":["chat.list"],"agent_ids":[7]}]`
	expired := "uk_expired_external_chat"
	if _, err := s.store.CreateAPIKey(user, "expired", HashAPIKey(expired), "uk_expired", APIKeyCreateOptions{
		Kind: "delegated_user", ProjectID: project.ID, Scopes: scopes, AllowedOrigins: origins,
		ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), IssuerApp: "channel-chat",
		SubjectType: "website_user", SubjectID: "expired-user",
	}); err != nil {
		t.Fatal(err)
	}
	next := s.authMiddleware(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/apps/channel-chat/chats?agent_id=7", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	req.Header.Set("Origin", "https://chat.example")
	rec := httptest.NewRecorder()
	next(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired credential status=%d body=%s", rec.Code, rec.Body.String())
	}

	active := "uk_active_external_chat"
	if _, err := s.store.CreateAPIKey(user, "active", HashAPIKey(active), "uk_active", APIKeyCreateOptions{
		Kind: "delegated_user", ProjectID: project.ID, Scopes: scopes, AllowedOrigins: origins,
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), IssuerApp: "channel-chat",
		SubjectType: "website_user", SubjectID: "active-user",
	}); err != nil {
		t.Fatal(err)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "/apps/channel-chat/chats", nil)
	preflight.Header.Set("Origin", "https://chat.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	cors := (*corsConfig)(nil).middlewareWithDynamicOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should be handled by CORS")
	}), s.delegatedAppCORSOriginAllowed)
	corsRec := httptest.NewRecorder()
	cors.ServeHTTP(corsRec, preflight)
	if corsRec.Code != http.StatusNoContent || corsRec.Header().Get("Access-Control-Allow-Origin") != "https://chat.example" {
		t.Fatalf("dynamic CORS status=%d headers=%v", corsRec.Code, corsRec.Header())
	}
}

func TestAgentAppCallReceivesPersistedConversationSubject(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "external-chat-tool-context@example.com")
	project, err := s.store.CreateProject(user, "Website", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user, "Support", "Help customers", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reg, err := s.startApps(mux)
	if err != nil {
		t.Fatal(err)
	}
	s.apps = reg
	t.Cleanup(func() { reg.Stop(500 * time.Millisecond) })
	conversationID := "conv-external-tool-context"
	threadID := "chat-" + conversationID
	if _, err := s.store.db.Exec(`
		INSERT INTO channel_chat_chats
			(id, agent_id, title, project_id, owner_user_id, kind, directive, thread_id,
			 subject_type, subject_id, conversation_key)
		VALUES (?, ?, 'Support', ?, ?, 'direct', '', ?, 'website_user', 'customer-123', 'support')`,
		conversationID, agent.ID, project.ID, user, threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO channel_chat_participants(chat_id, agent_id, is_lead) VALUES (?, ?, 1)`, conversationID, agent.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/crm/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"crm_contacts_list","arguments":{"_apteva_caller_thread":"`+threadID+`"}}}`))
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(agent.ID))
	if err := extractCallerThreadFromMCPRequest(req); err != nil {
		t.Fatal(err)
	}
	s.applyChannelChatSubjectContext(req)
	if got := req.Header.Get("X-Apteva-Subject-Type"); got != "website_user" {
		t.Fatalf("subject type=%q", got)
	}
	if got := req.Header.Get("X-Apteva-Subject-ID"); got != "customer-123" {
		t.Fatalf("subject id=%q", got)
	}
	if got := req.Header.Get("X-Apteva-Conversation-ID"); got != conversationID {
		t.Fatalf("conversation id=%q", got)
	}
}

func quoteJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
