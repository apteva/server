package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateConnectionRejectsRetiredHostedProviderSource(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	req := authedRequest(t, http.MethodPost, "/connections", "", map[string]any{
		"source":   "composio",
		"app_slug": "composio",
		"name":     "Hosted",
	})
	rec := httptest.NewRecorder()
	s.handleCreateConnection(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemoveLegacyHostedIntegrationProviderPurgesOnlyHostedRows(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)

	if _, err := s.store.db.Exec(`
		INSERT INTO provider_types (id,type,name,description,fields,requires_credentials)
		VALUES (9,'integrations','Legacy hosted integrations','','[]',1)`); err != nil {
		t.Fatal(err)
	}
	providerResult, err := s.store.db.Exec(`
		INSERT INTO providers (user_id,provider_type_id,type,name,encrypted_data,status)
		VALUES (1,9,'integrations','Legacy hosted integrations','encrypted','active')`)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := providerResult.LastInsertId()
	hostedResult, err := s.store.db.Exec(`
		INSERT INTO connections (user_id,app_slug,app_name,name,auth_type,encrypted_credentials,status,source,provider_id)
		VALUES (1,'legacy-toolkit','Legacy toolkit','Legacy toolkit','oauth2','encrypted','active','composio',?)`, providerID)
	if err != nil {
		t.Fatal(err)
	}
	hostedConnectionID, _ := hostedResult.LastInsertId()
	localResult, err := s.store.db.Exec(`
		INSERT INTO connections (user_id,app_slug,app_name,name,auth_type,encrypted_credentials,status,source)
		VALUES (1,'composio','Composio','Composio','api_key','encrypted','active','local')`)
	if err != nil {
		t.Fatal(err)
	}
	localConnectionID, _ := localResult.LastInsertId()
	if _, err := s.store.db.Exec(`
		INSERT INTO mcp_servers (user_id,name,source,connection_id,provider_id)
		VALUES (1,'legacy-hosted','remote',?,?)`, hostedConnectionID, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`
		INSERT INTO subscriptions (id,user_id,connection_id,name,webhook_path)
		VALUES ('legacy-sub',1,?,'Legacy subscription','legacy-hook')`, hostedConnectionID); err != nil {
		t.Fatal(err)
	}

	if err := s.store.removeLegacyHostedIntegrationProvider(); err != nil {
		t.Fatal(err)
	}

	for name, query := range map[string]string{
		"provider type": `SELECT COUNT(*) FROM provider_types WHERE id=9`,
		"provider":      `SELECT COUNT(*) FROM providers WHERE id=` + itoa64(providerID),
		"connection":    `SELECT COUNT(*) FROM connections WHERE id=` + itoa64(hostedConnectionID),
		"mcp server":    `SELECT COUNT(*) FROM mcp_servers WHERE provider_id=` + itoa64(providerID),
		"subscription":  `SELECT COUNT(*) FROM subscriptions WHERE id='legacy-sub'`,
	} {
		var count int
		if err := s.store.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("%s count: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s was not removed", name)
		}
	}
	var localCount int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE id=? AND app_slug='composio' AND source='local'`, localConnectionID).Scan(&localCount); err != nil {
		t.Fatal(err)
	}
	if localCount != 1 {
		t.Fatal("ordinary catalog integration connection was removed")
	}
}
