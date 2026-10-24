package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAppMCPManagedCredentialFollowsActiveGeneration(t *testing.T) {
	s := &Server{store: newTestStore(t), secret: []byte("01234567890123456789012345678901")}
	id := seedAppWithTools(t, s, "managed-mcp", "proj-1", []string{"lookup"})
	manager := s.appRuntimeManager()
	t.Cleanup(manager.Close)
	register := func() string {
		t.Helper()
		if err := s.registerAppMCP(id); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := s.store.db.QueryRow(`SELECT url FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(id)).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	authenticate := func(raw string) int {
		t.Helper()
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		parsed.Path = strings.TrimPrefix(parsed.Path, "/api")
		request := httptest.NewRequest(http.MethodPost, parsed.String(), nil)
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })(response, request)
		return response.Code
	}
	legacy := register()
	if got := authenticate(legacy); got != 204 {
		t.Fatalf("unmanaged MCP status %d", got)
	}
	var previous string
	for _, generation := range []int64{1, 2} {
		name := "generation-one"
		if generation == 2 {
			name = "generation-two"
		}
		encrypted, err := Encrypt(s.secret, name+"-credential")
		if err != nil {
			t.Fatal(err)
		}
		deployment := runtimeDeployment{ID: name, InstallID: id, Generation: generation, Status: "active", CredentialEncrypted: encrypted}
		if err = manager.saveDeployment(deployment); err != nil {
			t.Fatal(err)
		}
		if _, err = s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1,runtime_active_deployment=? WHERE id=?`, name, id); err != nil {
			t.Fatal(err)
		}
		current := register()
		if got := authenticate(current); got != 204 {
			t.Fatalf("managed generation %d MCP status %d", generation, got)
		}
		parsed, err := url.Parse(current)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []struct{ path, address string }{
			{"/apps/managed-mcp/mcp", "203.0.113.10:12345"},
			{"/instances", "127.0.0.1:12345"},
		} {
			request := httptest.NewRequest(http.MethodPost, scope.path+"?"+parsed.RawQuery, nil)
			request.RemoteAddr = scope.address
			response := httptest.NewRecorder()
			s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("runtime query escaped loopback app scope: %s %s status %d", scope.path, scope.address, response.Code)
			}
		}
		if got := authenticate(legacy); got == 204 {
			t.Fatal("retired installation credential accepted")
		}
		if previous != "" {
			if got := authenticate(previous); got == 204 {
				t.Fatal("prior generation credential admitted new MCP requests")
			}
		}
		previous = current
	}
}
