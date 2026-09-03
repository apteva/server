package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func restrictedTestPolicy() AccessPolicy {
	policy := defaultAccessPolicy("open")
	policy.Limits = ResourceAccessPolicy{
		ProjectsPerUser: 1, AgentsPerProject: 1, RunningAgentsPerProject: 1,
		DailyModelCalls: 1, DailyTokens: 1000, ConcurrentLLMRequests: 1,
		GlobalConcurrentLLMCalls: 2,
	}
	policy.Capabilities = CapabilityAccessPolicy{}
	policy.WorkspaceLifecycle = WorkspaceLifecyclePolicy{ExpiresAfter: "24h", IdleShutdownAfter: "10m", ResetFromPreset: true}
	return policy
}

func createTestAdmin(t *testing.T, s *Server) *User {
	t.Helper()
	admin, err := s.store.CreateUser("admin@access.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPlatformRole(admin.ID, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	return admin
}

func TestServerSettingsAccessPolicyIsAdminOnlyAndPersists(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	admin := createTestAdmin(t, s)
	user, _ := s.store.CreateUser("user@access.test", "hash")

	bad := httptest.NewRequest(http.MethodPut, "/settings/server", strings.NewReader(`{"public_url":"https://attacker.test"}`))
	bad.Header.Set("X-User-ID", itoa(user.ID))
	badRec := httptest.NewRecorder()
	s.handleServerSettings(badRec, bad)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("ordinary user changed server settings: status=%d body=%s", badRec.Code, badRec.Body.String())
	}
	policy := restrictedTestPolicy()
	raw, _ := json.Marshal(map[string]any{"access_policy": policy})
	req := httptest.NewRequest(http.MethodPut, "/settings/server", bytes.NewReader(raw))
	req.Header.Set("X-User-ID", itoa(admin.ID))
	rec := httptest.NewRecorder()
	s.handleServerSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save policy status=%d body=%s", rec.Code, rec.Body.String())
	}
	loaded, err := s.loadAccessPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Limits.ProjectsPerUser != 1 || loaded.Registration.Mode != "open" || loaded.Capabilities.APIKeys {
		t.Fatalf("saved policy mismatch: %+v", loaded)
	}
	loaded.ManagedLLM = ManagedLLMPolicy{ConnectionID: 42, Models: []string{"model-a"}}
	stored, _ := json.Marshal(loaded)
	if err := s.store.SetSetting(accessPolicySettingKey, string(stored)); err != nil {
		t.Fatal(err)
	}
	badGet := httptest.NewRequest(http.MethodGet, "/settings/server", nil)
	badGet.Header.Set("X-User-ID", itoa(user.ID))
	badGetRec := httptest.NewRecorder()
	s.handleServerSettings(badGetRec, badGet)
	if badGetRec.Code != http.StatusOK || strings.Contains(badGetRec.Body.String(), "connection_id") || !strings.Contains(badGetRec.Body.String(), `"configured":true`) {
		t.Fatalf("ordinary user settings response leaked managed connection: status=%d body=%s", badGetRec.Code, badGetRec.Body.String())
	}
}

func TestOpenRegistrationProvisioningAndResourceLimits(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	admin := createTestAdmin(t, s)
	policy := restrictedTestPolicy()
	if _, err := s.saveAccessPolicy(admin.ID, policy); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"new@access.test","password":"long-enough"}`))
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	s.handleRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	user, err := s.store.GetUserByEmail("new@access.test")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := s.store.ListProjectsForUser(user.ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%v err=%v", projects, err)
	}
	if projects[0].ExpiresAt == nil {
		t.Fatal("provisioned project has no expiration")
	}

	projectReq := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(`{"name":"second"}`))
	projectReq.Header.Set("X-User-ID", itoa(user.ID))
	projectRec := httptest.NewRecorder()
	s.handleCreateProject(projectRec, projectReq)
	if projectRec.Code != http.StatusConflict {
		t.Fatalf("second project status=%d body=%s", projectRec.Code, projectRec.Body.String())
	}

	createAgent := func(name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"name": name, "project_id": projects[0].ID, "start": false})
		r := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(body))
		r.Header.Set("X-User-ID", itoa(user.ID))
		w := httptest.NewRecorder()
		s.handleCreateInstance(w, r)
		return w
	}
	if first := createAgent("one"); first.Code != http.StatusOK {
		t.Fatalf("first agent status=%d body=%s", first.Code, first.Body.String())
	}
	if second := createAgent("two"); second.Code != http.StatusConflict {
		t.Fatalf("second agent status=%d body=%s", second.Code, second.Body.String())
	}

	keyReq := httptest.NewRequest(http.MethodPost, "/auth/keys", strings.NewReader(`{"name":"blocked"}`))
	keyReq.Header.Set("X-User-ID", itoa(user.ID))
	keyRec := httptest.NewRecorder()
	s.handleCreateKey(keyRec, keyReq)
	if keyRec.Code != http.StatusForbidden {
		t.Fatalf("disabled API key status=%d", keyRec.Code)
	}
}

func TestAgentLimitIsAtomicAcrossConcurrentCreates(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	admin := createTestAdmin(t, s)
	policy := restrictedTestPolicy()
	if _, err := s.saveAccessPolicy(admin.ID, policy); err != nil {
		t.Fatal(err)
	}
	user, _ := s.store.CreateUser("concurrent@access.test", "hash")
	project, _ := s.store.CreateProject(user.ID, "Private", "", "")

	const attempts = 8
	statuses := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"name": fmt.Sprintf("agent-%d", i), "project_id": project.ID, "start": false})
			req := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(body))
			req.Header.Set("X-User-ID", itoa(user.ID))
			rec := httptest.NewRecorder()
			s.handleCreateInstance(rec, req)
			statuses <- rec.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("created %d agents with a limit of one", successes)
	}
}

func TestManagedLLMGatewayUsesProtectedConnectionAndAccountsUsage(t *testing.T) {
	s := newTestServer(t)
	admin := createTestAdmin(t, s)
	var upstreamAuth string
	var upstreamBody map[string]any
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	s.managedLLMTransport = upstream.Client().Transport
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "test-llm", Name: "Test LLM", BaseURL: upstream.URL,
		Auth:    AppAuthConfig{Headers: map[string]string{"Authorization": "Bearer {{token}}"}},
		Runtime: &AppRuntimeConfig{Role: "llm", ProviderKey: "managed"},
	})
	encrypted, _ := Encrypt(s.secret, `{"token":"upstream-secret"}`)
	connection, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: admin.ID, AppSlug: "test-llm", AppName: "Test LLM", Name: "hosted",
		AuthType: "bearer", EncryptedCreds: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pool := s.GetProviderPool(admin.ID); len(pool) != 0 {
		t.Fatalf("unselected managed connection entered provider pool: %+v", pool)
	}
	policy := restrictedTestPolicy()
	policy.ManagedLLM = ManagedLLMPolicy{ConnectionID: connection.ID, Path: "/chat/completions", Models: []string{"model-a"}}
	if _, err := s.saveAccessPolicy(admin.ID, policy); err != nil {
		t.Fatal(err)
	}
	user, _ := s.store.CreateUser("gateway-user@test", "hash")
	project, _ := s.store.CreateProject(user.ID, "Private", "", "")
	agent, _ := s.store.CreateAgent(user.ID, "agent", "", "cautious", "{}", project.ID)
	if pool := s.GetProviderPool(user.ID, project.ID); len(pool) != 1 || pool[0].Type != "managed" {
		t.Fatalf("managed provider pool=%+v", pool)
	}
	agent.CoreAPIKey = "core_gateway_test"
	agent.Status = "running"
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/llm/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.CoreAPIKey)
	rec := httptest.NewRecorder()
	s.handleManagedLLMChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamAuth != "Bearer upstream-secret" {
		t.Fatalf("upstream auth=%q", upstreamAuth)
	}
	if _, ok := upstreamBody["stream_options"]; !ok {
		t.Fatal("gateway did not request streaming usage")
	}
	usage, err := s.store.ManagedLLMUsageForDay(user.ID, project.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Calls != 1 || usage.InputTokens != 12 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}

	second := httptest.NewRequest(http.MethodPost, "/llm/chat/completions", strings.NewReader(body))
	second.Header.Set("Authorization", "Bearer "+agent.CoreAPIKey)
	secondRec := httptest.NewRecorder()
	s.handleManagedLLMChat(secondRec, second)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
	conn, _, _ := s.store.GetConnectionAny(connection.ID)
	if conn.CredentialManagement != "platform" || conn.CredentialExportPolicy != "never" {
		t.Fatalf("connection not protected: %+v", conn)
	}
}

func TestCompleteProjectDeletionRemovesRowsAndRuntimeDirectory(t *testing.T) {
	s := newTestServer(t)
	user, _ := s.store.CreateUser("delete-project@test", "hash")
	project, _ := s.store.CreateProject(user.ID, "Delete", "", "")
	agent, _ := s.store.CreateAgent(user.ID, "agent", "", "cautious", "{}", project.ID)
	_, _ = s.store.db.Exec(`INSERT INTO telemetry(agent_id,thread_id,type,time,data) VALUES(?, 'main','llm.done',CURRENT_TIMESTAMP,'{}')`, agent.ID)
	encrypted, _ := Encrypt(s.secret, `{}`)
	_, _ = s.store.CreateConnection(user.ID, "test", "Test", "test", "api_key", encrypted, project.ID)
	dir := s.agents.instanceDir(agent.ID)
	if err := os.WriteFile(filepath.Join(dir, "private.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteProjectCompletely(project.ID); err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"project":    `SELECT COUNT(*) FROM projects WHERE id=?`,
		"agent":      `SELECT COUNT(*) FROM agents WHERE project_id=?`,
		"connection": `SELECT COUNT(*) FROM connections WHERE project_id=?`,
	} {
		var count int
		if err := s.store.db.QueryRow(query, project.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s remains count=%d err=%v", name, count, err)
		}
	}
	var telemetry int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE agent_id=?`, agent.ID).Scan(&telemetry)
	if telemetry != 0 {
		t.Fatalf("telemetry remains=%d", telemetry)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory still exists: %v", err)
	}
}
