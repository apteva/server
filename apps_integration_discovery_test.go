package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestGitHubDiscoveryPreservesCodeRepositoryAccess(t *testing.T) {
	for _, origin := range []string{"integration", "app_install"} {
		for _, autoMCP := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/auto_mcp=%t", origin, autoMCP), func(t *testing.T) {
				s := newTestServer(t)
				ensureTestAdmin(t, s)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/user/repos" || r.Header.Get("Authorization") != "Bearer fixture-token" {
						http.Error(w, "incorrect repository request", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `[{"id":42,"full_name":"fixture/project"}]`)
				}))
				defer upstream.Close()
				data, err := os.ReadFile("integrations-catalog/github.json")
				if err != nil {
					t.Fatal(err)
				}
				var github AppTemplate
				if err := json.Unmarshal(data, &github); err != nil {
					t.Fatal(err)
				}
				github.BaseURL = upstream.URL
				s.catalog = NewAppCatalog()
				s.catalog.Register(&github)
				creds, err := Encrypt(s.secret, `{"token":"fixture-token"}`)
				if err != nil {
					t.Fatal(err)
				}
				conn, err := s.store.CreateConnectionExt(ConnectionInput{
					UserID: 1, AppSlug: "github", AppName: "GitHub", Name: "Code GitHub",
					AuthType: "bearer", EncryptedCreds: creds, ProjectID: "proj-1",
					CreatedVia: origin, AutoMCP: &autoMCP,
				})
				if err != nil {
					t.Fatal(err)
				}
				manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "code", Version: "1.0.0",
					Requires: sdk.Requires{
						Permissions:  []sdk.Permission{sdk.PermConnectionsExecute},
						Integrations: []sdk.IntegrationDep{{Role: "github", Kind: "integration", CompatibleSlugs: []string{"github"}}},
					}}
				installID := seedInstallWithBindings(t, s, "code", manifest, map[string]any{"github": conn.ID})
				req := httptest.NewRequest("GET", "/api/apps?project_id=proj-1", nil)
				req.Header.Set("X-User-ID", "1")
				rec := httptest.NewRecorder()
				s.handleListApps(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
				}
				var rows []AppRow
				if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 2 {
					t.Fatalf("want Code and GitHub discovery, got %+v", rows)
				}
				for _, row := range rows {
					switch row.Name {
					case "code":
						if row.InstallID != installID || row.Bindings["github"] != float64(conn.ID) {
							t.Fatalf("Code binding changed: %+v", row)
						}
					case "github":
						hasIssueCard := false
						for _, component := range row.UIComponents {
							if component.Name == "issue-card" && component.Entry == "/ui/IssueCard.mjs" {
								hasIssueCard = true
							}
						}
						if row.Source != "integration" || row.InstallID != 0 || !hasIssueCard {
							t.Fatalf("GitHub discovery lost: %+v", row)
						}
					default:
						t.Fatalf("unexpected app: %s", row.Name)
					}
				}
				var installs int
				if err := s.store.db.QueryRow("SELECT COUNT(*) FROM app_installs").Scan(&installs); err != nil || installs != 1 {
					t.Fatalf("unexpected installation created: count=%d err=%v", installs, err)
				}
				req = httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute", strings.NewReader(`{"tool":"list_repos","input":{}}`))
				req.Header.Set("X-User-ID", "1")
				req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
				rec = httptest.NewRecorder()
				s.handleAppCallback(rec, req)
				var result ExecuteResult
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || !result.Success || !strings.Contains(rec.Body.String(), "fixture/project") {
					t.Fatalf("Code repository access failed: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
				}
			})
		}
	}
}
