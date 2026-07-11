package main

import "testing"

func ensureTestAdmin(t *testing.T, s *Server) int64 {
	t.Helper()
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id,email,password_hash,role) VALUES (1,'admin@test.local','hash','admin')`); err != nil {
		t.Fatalf("seed test admin: %v", err)
	}
	if _, err := s.store.db.Exec(`UPDATE users SET role='admin' WHERE id=1`); err != nil {
		t.Fatalf("promote test admin: %v", err)
	}
	return 1
}

func testPrivateAPIKey(t *testing.T, s *Server) string {
	t.Helper()
	ensureTestAdmin(t, s)
	raw := "sk_test_private_key"
	if _, err := s.store.CreateAPIKey(1, "test", HashAPIKey(raw), "sk_test"); err != nil {
		t.Fatalf("seed test api key: %v", err)
	}
	return raw
}
