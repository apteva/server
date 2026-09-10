package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readOnlyTestKey(t *testing.T, s *Server, access string) (string, int64) {
	t.Helper()
	user, err := s.store.CreateUser("readonly@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	raw := "sk-readonly-test"
	key, err := s.store.CreateAPIKey(user.ID, "inspection", HashAPIKey(raw), "sk-readonly", APIKeyCreateOptions{Access: access})
	if err != nil {
		t.Fatal(err)
	}
	return raw, key.ID
}

func TestReadOnlyAPIKeyEnforcement(t *testing.T) {
	s := newTestServer(t)
	raw, _ := readOnlyTestKey(t, s, APIKeyReadOnly)
	if err := s.store.CreateSession("ambient-admin", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"/agents", "/instances", "/projects", "/projects/test", "/agents/1103", "/instances/1103/status", "/agents/1103/config", "/agents/1103/threads", "/agents/1103/events", "/telemetry", "/telemetry/stream", "/auth/keys", "/settings/server"}
	denied := []string{"/agents/1103/start", "/agents/1103/stop", "/agents/1103/pause", "/agents/1103/control", "/agents/1103/threads/main/resume", "/agents/1103/foo/status", "/agents/1103/../1104/status", "/agents/1103/%2e%2e/status", "/apps/test/mcp", "/apps/test/run", "/connections/1/auth/runtime-token", "/providers/1/auth/runtime-token", "/connections/1/credentials", "/platform/snapshot", "/integrations/catalog/reload", "/projects/test/setup/apply", "/future-get-action"}
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		for _, p := range append(append([]string{}, allowed...), denied...) {
			for _, carrier := range []string{"Authorization", "X-API-Key"} {
				t.Run(method+" "+p+" "+carrier, func(t *testing.T) {
					req := httptest.NewRequest(method, p, nil)
					token := raw
					if carrier == "Authorization" {
						token = "Bearer " + raw
					}
					req.Header.Set(carrier, token)
					req.AddCookie(&http.Cookie{Name: cookieName, Value: "ambient-admin"})
					req.Header.Set("X-User-ID", "999")
					req.Header.Set("X-API-Key-Access", "read_write")
					called := false
					w := httptest.NewRecorder()
					s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
						called = true
						if getUserID(r) != 1 || !isReadOnlyAPIKey(r) {
							t.Error("incorrect principal")
						}
						w.WriteHeader(204)
					})(w, req)
					want := method == "GET"
					found := false
					for _, a := range allowed {
						if p == a {
							found = true
						}
					}
					want = want && found
					if called != want || (!want && w.Code != 403) {
						t.Fatalf("called=%v status=%d wantAllowed=%v", called, w.Code, want)
					}
				})
			}
		}
	}
	for _, target := range []string{"/telemetry/stream?api_key=" + raw, "/app-events/chat?api_key=" + raw} {
		w := httptest.NewRecorder()
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(w, httptest.NewRequest("GET", target, nil))
		want := 204
		if strings.HasPrefix(target, "/app-events/") {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("query auth %s: %d", target, w.Code)
		}
	}
	req := httptest.NewRequest("GET", "/agents/1103/events", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	s.authMiddleware(func(http.ResponseWriter, *http.Request) { t.Fatal("upgrade allowed") })(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestReadOnlyAPIKeyLifecycle(t *testing.T) {
	s := newTestServer(t)
	raw, id := readOnlyTestKey(t, s, APIKeyReadOnly)
	user, access, err := s.store.getPrivateAPIKeyPrincipal(HashAPIKey(raw))
	if err != nil || user.ID != 1 || access != APIKeyReadOnly {
		t.Fatalf("principal: %v %s", err, access)
	}
	if _, err := s.store.GetUserByAPIKey(HashAPIKey(raw)); err == nil {
		t.Fatal("restricted key accepted by unrestricted lookup")
	}
	keys, err := s.store.ListAPIKeys(1)
	if err != nil || len(keys) != 1 || keys[0].Access != APIKeyReadOnly {
		t.Fatalf("list: %+v %v", keys, err)
	}
	for _, column := range []string{"expires_at", "revoked_at"} {
		_, err := s.store.db.Exec("UPDATE api_keys SET "+column+" = '2000-01-01' WHERE id = ?", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.store.getPrivateAPIKeyPrincipal(HashAPIKey(raw)); err == nil {
			t.Fatal("inactive key accepted")
		}
		if _, err := s.store.db.Exec("UPDATE api_keys SET "+column+" = NULL WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.DeleteAPIKey(1, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.getPrivateAPIKeyPrincipal(HashAPIKey(raw)); err == nil {
		t.Fatal("deleted key accepted")
	}
}

func TestReadOnlyAPIKeyCreateValidationAndLegacyDefault(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{"email": "owner@test.local", "password": "password123"})
	for _, body := range []string{`{"access":"read_only"}`, `{"access":"read_write"}`, `{}`} {
		req := httptest.NewRequest("POST", "/auth/keys", strings.NewReader(body))
		req.Header.Set("X-User-ID", "1")
		w := httptest.NewRecorder()
		s.handleCreateKey(w, req)
		if w.Code != 200 {
			t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
		}
		data := decodeJSON(t, w)
		want := APIKeyReadWrite
		if strings.Contains(body, APIKeyReadOnly) {
			want = APIKeyReadOnly
		}
		if data["access"] != want {
			t.Fatalf("access=%v", data["access"])
		}
		raw := data["key"].(string)
		rw := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/agents", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(rw, r)
		expected := 204
		if want == APIKeyReadOnly {
			expected = 403
		}
		if rw.Code != expected {
			t.Fatalf("mutation %d", rw.Code)
		}
	}
	for _, body := range []string{`{"access":"READ"}`, `{"kind":"public_client","access":"read_only"}`, `{"access":1}`, `{`} {
		r := httptest.NewRequest("POST", "/auth/keys", strings.NewReader(body))
		r.Header.Set("X-User-ID", "1")
		w := httptest.NewRecorder()
		s.handleCreateKey(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid body %s: %d", body, w.Code)
		}
	}
	// An old insert that does not know about access retains its original rights.
	_, err := s.store.db.Exec("INSERT INTO api_keys(user_id,name,key_hash,key_prefix) VALUES(1,'legacy',?,'sk-old')", HashAPIKey("sk-old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetUserByAPIKey(HashAPIKey("sk-old")); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyAPIKeyAgentConfigAndOwnership(t *testing.T) {
	s := newTestServer(t)
	raw, _ := readOnlyTestKey(t, s, APIKeyReadOnly)
	secretConfig := `{"model":"test-model","api_key":"SECRET","providers":[{"api_key":"SECRET"}],"mcp_servers":[{"env":{"TOKEN":"SECRET"}}],"env":{"KEY":"SECRET"}}`
	agent, err := s.store.CreateAgent(1, "test", "Observe", "cautious", secretConfig, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/instances", "/instances/" + itoa(agent.ID), "/instances/" + itoa(agent.ID) + "/config"} {
		handler := s.handleListInstances
		if strings.HasSuffix(target, "/config") {
			handler = s.handleUpdateConfig
		} else if target != "/instances" {
			handler = s.handleInstance
		}
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		s.authMiddleware(handler)(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET") || (target != "/instances" && !strings.Contains(w.Body.String(), "test-model")) {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body.String())
		}
	}
	saved, err := s.store.GetAgentByID(agent.ID)
	if err != nil || saved.Config != secretConfig {
		t.Fatal("stored config was changed")
	}
	// Read-only access never elevates ownership, even for an allowed endpoint.
	other, err := s.store.CreateUser("other@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.CreateAPIKey(other.ID, "other", HashAPIKey("sk-other"), "sk-other", APIKeyCreateOptions{Access: APIKeyReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/instances/"+itoa(agent.ID), nil)
	r.Header.Set("Authorization", "Bearer sk-other")
	w := httptest.NewRecorder()
	s.authMiddleware(s.handleInstance)(w, r)
	if w.Code != 404 {
		t.Fatalf("ownership: %d", w.Code)
	}
}

func TestReadOnlyAPIKeyMigrationAndUnknownAccess(t *testing.T) {
	s := newTestServer(t)
	raw, id := readOnlyTestKey(t, s, APIKeyReadWrite)
	// Recreate the pre-feature schema with an existing key, then run migration.
	if _, err := s.store.db.Exec("ALTER TABLE api_keys DROP COLUMN access"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetUserByAPIKey(HashAPIKey(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec("UPDATE api_keys SET access='unknown' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/agents", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	s.authMiddleware(func(http.ResponseWriter, *http.Request) { t.Fatal("unknown access accepted") })(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestReadOnlyAPIKeyTelemetryOwnership(t *testing.T) {
	s := newTestServer(t)
	raw, _ := readOnlyTestKey(t, s, APIKeyReadOnly)
	own, err := s.store.CreateAgent(1, "Own", "Observe", "cautious", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateUser("private@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.store.CreateAgent(other.ID, "Private", "Observe", "cautious", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/telemetry", s.handleQueryTelemetry},
		{"/telemetry/stats", s.handleTelemetryStats},
		{"/telemetry/timeline", s.handleTelemetryTimeline},
	} {
		for _, id := range []int64{own.ID, foreign.ID} {
			r := httptest.NewRequest("GET", route.path+"?instance_id="+itoa(id), nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			w := httptest.NewRecorder()
			s.authMiddleware(route.handler)(w, r)
			want := 200
			if id == foreign.ID {
				want = 404
			}
			if w.Code != want {
				t.Fatalf("%s agent %d: %d want %d", route.path, id, w.Code, want)
			}
		}
	}
}
