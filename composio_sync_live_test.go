//go:build live

package main

import (
	"encoding/json"
	"os"
	"testing"
)

// Run with:
//   COMPOSIO_KEY=ak_... go test -tags live -run TestComposioSync_Live -v -count=1
//
// Not a normal unit test — exercises syncComposioProviderData against the
// real Composio API using a fresh SQLite DB. Prints the resulting
// connections + mcp_servers so we can sanity-check end to end.
func TestComposioSync_Live(t *testing.T) {
	key := os.Getenv("COMPOSIO_KEY")
	if key == "" {
		t.Skip("COMPOSIO_KEY not set")
	}

	s := newTestServer(t)
	s.secret = testSecret()
	s.mcpManager = NewMCPManager()
	s.catalog = NewAppCatalog()
	s.port = "0"

	user, err := s.store.CreateUser("live@test.local", "hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	data, _ := json.Marshal(map[string]string{"COMPOSIO_API_KEY": key})
	enc, err := Encrypt(s.secret, string(data))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	p, err := s.store.CreateProvider(user.ID, 9, "integrations", "Composio", enc, "live-project")
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	t.Logf("provider id=%d project=live-project", p.ID)

	s.syncComposioProviderData(user.ID, p.ID, "live-project")

	conns, _ := s.store.ListConnections(user.ID, "live-project")
	t.Logf("=== CONNECTIONS (%d) ===", len(conns))
	for _, c := range conns {
		t.Logf("  id=%d slug=%s status=%s external=%s auth_type=%s",
			c.ID, c.AppSlug, c.Status, c.ExternalID, c.AuthType)
	}

	rows, _ := s.store.ListMCPServers(user.ID, "live-project")
	t.Logf("=== MCP SERVERS (%d) ===", len(rows))
	for _, r := range rows {
		t.Logf("  id=%d name=%s source=%s transport=%s upstream=%s url=%s tools=%d",
			r.ID, r.Name, r.Source, r.Transport, r.UpstreamID, r.URL, r.ToolCount)
	}

	// Also hit Composio directly to see what's there after reconcile.
	client := NewComposioClient(key)
	upstream, err := client.ListComposioMCPServers()
	if err != nil {
		t.Logf("ListComposioMCPServers error: %v", err)
	} else {
		t.Logf("=== COMPOSIO UPSTREAM MCP SERVERS (%d) ===", len(upstream))
		for _, srv := range upstream {
			t.Logf("  id=%s name=%s toolkits=%v allowed=%d url=%s",
				srv.ID, srv.Name, srv.ToolkitSlugs, len(srv.AllowedTools), srv.URL)
		}
	}
}
