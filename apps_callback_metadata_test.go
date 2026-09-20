package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestAppMetadataCacheInvalidation(t *testing.T) {
	s, caller, target := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	initial, err := s.appMetadata(caller)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := s.appMetadata(caller)
	if err != nil || warm != initial {
		t.Fatal("warm metadata was not reused")
	}
	if !installHasPermission(s, caller, sdk.PermAppsCall) || installBoundAppID(s, caller, "parallel-target") != target {
		t.Fatal("initial grant missing")
	}
	mutated := bindingsForInstall(s, caller)
	mutated["parallel-target"] = nil
	if installBoundAppID(s, caller, "parallel-target") != target {
		t.Fatal("binding editor mutated shared cache")
	}
	for _, tc := range []struct {
		name, sql string
		args      []any
		check     func()
	}{
		{"permission revocation", `UPDATE app_installs SET permissions_json='[]' WHERE id=?`, []any{caller}, func() {
			if installHasPermission(s, caller, sdk.PermAppsCall) {
				t.Fatal("revoked permission cached")
			}
			w := runTestBatch(s, caller, context.Background(), `{"calls":[{"tool":"get"}]}`)
			if w.Code != 403 {
				t.Fatalf("revoked callback status=%d", w.Code)
			}
		}},
		{"binding change", `UPDATE app_installs SET integration_bindings='{}' WHERE id=?`, []any{caller}, func() {
			if installBoundAppID(s, caller, "parallel-target") != 0 {
				t.Fatal("revoked binding cached")
			}
		}},
		{"scope change", `UPDATE app_installs SET project_id='other-project' WHERE id=?`, []any{caller}, func() {
			if installProjectID(s, caller) != "other-project" {
				t.Fatal("stale scope")
			}
		}},
		{"manifest upgrade", `UPDATE app_installs SET manifest_json='{"name":"upgraded"}' WHERE id=?`, []any{caller}, func() {
			m, err := installManifest(s, caller)
			if err != nil || m.Name != "upgraded" {
				t.Fatal("stale manifest")
			}
		}},
		{"status change", `UPDATE app_installs SET status='error' WHERE id=?`, []any{target}, func() {
			m, err := s.appMetadata(target)
			if err != nil || m.status != "error" {
				t.Fatal("stale status")
			}
		}},
		{"role revocation", `UPDATE users SET role='user' WHERE id=1`, nil, func() {}},
		{"uninstall", `DELETE FROM app_installs WHERE id=?`, []any{caller}, func() {
			if installHasPermission(s, caller, sdk.PermAppsCall) {
				t.Fatal("uninstalled access cached")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := s.appMetadata(target)
			if _, err := s.store.db.Exec(tc.sql, tc.args...); err != nil {
				t.Fatal(err)
			}
			after, err := s.appMetadata(target)
			if err != nil || before == after {
				t.Fatal("generation did not invalidate cache")
			}
			tc.check()
		})
	}
}

func TestAppMetadataMembershipRevocation(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, statement := range []string{
		`UPDATE users SET role='user' WHERE id=1`,
		`INSERT INTO users(id,email,password_hash,role) VALUES(2,'owner@example.test','x','user')`,
		`INSERT INTO projects(id,user_id,name) VALUES('cache-project',2,'Cache project')`,
		`INSERT INTO project_members(project_id,user_id,role) VALUES('cache-project',1,'viewer')`,
	} {
		if _, err := s.store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, caller); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if _, ok := s.appCallProject(w, caller, "cache-project"); !ok {
		t.Fatal(w.Body.String())
	}
	before, _ := s.appMetadata(caller)
	if _, err := s.store.db.Exec(`DELETE FROM project_members WHERE project_id='cache-project' AND user_id=1`); err != nil {
		t.Fatal(err)
	}
	after, _ := s.appMetadata(caller)
	if before == after {
		t.Fatal("membership failed to invalidate")
	}
	w = httptest.NewRecorder()
	if _, ok := s.appCallProject(w, caller, "cache-project"); ok || w.Code != 403 {
		t.Fatal("revoked member retained access")
	}
}

func TestAppMetadataInvalidationRollsBackWithTransaction(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	before, _ := s.appMetadata(caller)
	tx, err := s.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, caller); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, _ := s.appMetadata(caller)
	if before != after || !installHasPermission(s, caller, sdk.PermAppsCall) {
		t.Fatal("rolled back mutation changed authorization")
	}
}

func TestAppSingleDirectResultPreservesArbitraryJSON(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sdk.HeaderAppResultFormat) != sdk.AppResultJSON {
			t.Error("direct mode not negotiated")
		}
		w.Header().Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
		_, _ = w.Write([]byte(`{"error":{"message":"data"},"result":{"content":[]}}`))
	})
	r := httptest.NewRequest("POST", "/apps/callback/apps/parallel-target/call", nil)
	body, _ := json.Marshal(map[string]any{"tool": "read"})
	r = httptest.NewRequest("POST", r.URL.String(), bytes.NewReader(body))
	r.Header.Set("X-Apteva-App-Install-ID", itoa(caller))
	r.Header.Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
	r.Header.Set(sdk.HeaderAppCallResult, "inner")
	w := httptest.NewRecorder()
	s.handleAppCallback(w, r)
	if w.Code != 200 || w.Header().Get(sdk.HeaderAppResultFormat) != sdk.AppResultJSON || w.Body.String() != `{"error":{"message":"data"},"result":{"content":[]}}` {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
