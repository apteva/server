package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetPlatformUpdateGrantRevocationAndReplay(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.dataDir = t.TempDir()
	token := "fup_" + strings.Repeat("b", 64)
	digest := sha256.Sum256([]byte(token))
	path := filepath.Join(s.dataDir, "fleet-update-grant")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(digest[:])), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	archive := filepath.Join(s.dataDir, "fleet-update-jobs")
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	job := fmt.Sprintf(`{"id":"one","state":"rolled_back","target_version":"1.2.3","previous_version":"1.2.2","backup":"private-path","request":{"request_id":%q,"agent_policy":"preserve","home":"private-path"}}`, id)
	if err := os.WriteFile(filepath.Join(archive, id+".json"), []byte(job), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(method, suffix, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/fleet-platform-update"+suffix, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.handleFleetPlatformUpdate(w, r)
		return w
	}
	query := "?request_id=" + id
	if w := call(http.MethodGet, query, "ordinary-admin-key", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("ordinary key got %d", w.Code)
	}
	if w := call(http.MethodGet, query, token, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "rolled_back") || strings.Contains(w.Body.String(), "private-path") {
		t.Fatalf("lookup: %d %s", w.Code, w.Body.String())
	}
	req := fmt.Sprintf(`{"request_id":%q,"version":"1.2.3","agent_policy":"preserve"}`, id)
	if w := call(http.MethodPost, "", token, req); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "rolled_back") {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "", token, strings.Replace(req, "preserve", "restart", 1)); w.Code != http.StatusConflict {
		t.Fatalf("mismatched replay got %d", w.Code)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if w := call(http.MethodPost, "", token, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key got %d", w.Code)
	}
	w := httptest.NewRecorder()
	r := authedRequest(t, http.MethodPost, "/platform-update", "", map[string]string{"version": "1.2.3"})
	r.Header.Set("X-Apteva-Operator-ID", "1")
	s.handlePlatformUpdate(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("dashboard or admin key could bypass revoked grant: %d", w.Code)
	}
}
