package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// Service-to-service callbacks must retain their authorized project across all
// transports without acquiring operator or delegated application-user identity.
func TestAppCallbackServiceProjectHeader(t *testing.T) {
	for _, app := range []string{"functions", "graphql"} {
		for _, transport := range []string{"single", "single_json", "batch_fallback", "batch_fast"} {
			t.Run(app+"/"+transport, func(t *testing.T) {
				s := newTestServer(t)
				s.installedApps = NewInstalledAppsRegistry()
				var callerID int64
				var forwarded atomic.Int32
				targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					forwarded.Add(1)
					if r.Header.Get("X-Apteva-Project-ID") != "proj-1" {
						t.Errorf("%s: missing or incorrect trusted project header: %q", r.URL.Path, r.Header.Get("X-Apteva-Project-ID"))
					}
					if r.Header.Get(sdk.HeaderBoundCallerInstallID) != strconv.FormatInt(callerID, 10) || r.Header.Get(sdk.HeaderBoundCallerAppName) != app {
						t.Error("authenticated service identity changed")
					}
					if r.Header.Get("X-Apteva-Operator-ID") != "" || r.Header.Get("X-Apteva-Subject-Type") != "" || r.Header.Get("X-Apteva-Subject-ID") != "" || r.Header.Get(sdk.HeaderTrustedPrincipal) != "" {
						t.Error("service call gained operator or delegated-user authority")
					}
					if r.Header.Get("Authorization") != "Bearer fixture-target-token" {
						t.Error("target authentication changed")
					}
					if r.Method == http.MethodHead {
						if transport == "batch_fast" {
							w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
							w.WriteHeader(http.StatusNoContent)
						} else {
							http.NotFound(w, r)
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == sdk.InternalAppBatchPath {
						var body sdk.InternalAppBatchRequest
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						results := make([]sdk.AppCallResult, len(body.Calls))
						for i, call := range body.Calls {
							var input map[string]any
							if err := json.Unmarshal(call.Input, &input); err != nil {
								t.Fatal(err)
							}
							if input["_project_id"] != "proj-1" {
								t.Error("batch body scope diverged from trusted header")
							}
							results[i] = sdk.AppCallResult{ID: call.ID, Status: 200, Format: "json", Result: json.RawMessage(`{"ok":true}`)}
						}
						w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
						_ = json.NewEncoder(w).Encode(sdk.InternalAppBatchResponse{Results: results})
						return
					}
					var rpc struct {
						Params struct {
							Arguments map[string]any `json:"arguments"`
						} `json:"params"`
					}
					if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
						t.Fatal(err)
					}
					if rpc.Params.Arguments["_project_id"] != "proj-1" {
						t.Error("MCP body scope diverged from trusted header")
					}
					if r.Header.Get(sdk.HeaderAppResultFormat) == sdk.AppResultJSON {
						w.Header().Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
						_, _ = io.WriteString(w, `{"ok":true}`)
					} else {
						_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
					}
				}))
				defer targetHTTP.Close()
				targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "database"}
				targetID := seedInstallWithBindings(t, s, "database", targetManifest, nil)
				s.installedApps.Add(&InstalledApp{InstallID: targetID, AppName: "database", ProjectID: "proj-1", Manifest: targetManifest, SidecarURL: targetHTTP.URL, Token: "fixture-target-token"})
				callerManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: app, Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: "database"}}}}
				callerID = seedInstallWithBindings(t, s, app, callerManifest, map[string]any{"database": targetID})
				path := "/apps/callback/apps/database/call"
				body := `{"tool":"db_get","input":{"_project_id":"proj-1","database":"fixture","collection":"records","key":{"id":"1"}}}`
				if strings.HasPrefix(transport, "batch") {
					path = "/apps/callback/apps/database/batch"
					body = `{"result_mode":"json","calls":[{"id":"read","tool":"db_get","input":{"_project_id":"proj-1","database":"fixture","collection":"records","key":{"id":"1"}}}]}`
				}
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(callerID, 10))
				req.Header.Set("X-Apteva-Project-ID", "forged-header-must-not-be-forwarded")
				if transport == "single_json" {
					req.Header.Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
				}
				rec := httptest.NewRecorder()
				s.handleAppCallback(rec, req)
				if rec.Code != http.StatusOK || forwarded.Load() == 0 {
					t.Fatalf("status=%d calls=%d body=%s", rec.Code, forwarded.Load(), rec.Body.String())
				}
				before := forwarded.Load()
				forged := strings.ReplaceAll(body, `"_project_id":"proj-1"`, `"_project_id":"other-project"`)
				req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(forged))
				req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(callerID, 10))
				rec = httptest.NewRecorder()
				s.handleAppCallback(rec, req)
				if rec.Code < 400 || forwarded.Load() != before {
					t.Fatalf("cross-project override reached target: status=%d calls=%d", rec.Code, forwarded.Load())
				}
			})
		}
	}
}

// Project scope is independent of optional operator metadata. In particular,
// an empty validated scope must clear an existing header rather than retain it.
func TestAppCallbackProjectHeaderIndependentOfOperator(t *testing.T) {
	for _, principal := range []struct {
		name     string
		value    *appCallbackPrincipal
		operator string
	}{
		{name: "no principal"},
		{name: "service principal", value: &appCallbackPrincipal{installID: 7, userID: 42}},
		{name: "operator session", value: &appCallbackPrincipal{installID: 7, userID: 42, userSession: true}, operator: "42"},
		{name: "invalid operator", value: &appCallbackPrincipal{installID: 7, userSession: true}},
	} {
		for _, project := range []string{"verified-project", ""} {
			t.Run(principal.name+"/"+project, func(t *testing.T) {
				ctx := context.Background()
				if principal.value != nil {
					ctx = context.WithValue(ctx, appCallbackPrincipalKey{}, *principal.value)
				}
				request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
				request.Header.Set("X-Apteva-Project-ID", "untrusted-existing-project")
				request.Header.Set(sdk.HeaderBoundCallerInstallID, "7")
				request.Header.Set(sdk.HeaderBoundCallerAppName, "functions")
				setAppCallbackOperatorHeaders(request, ctx, project)
				if got := request.Header.Get("X-Apteva-Project-ID"); got != project {
					t.Errorf("project=%q, want %q", got, project)
				}
				if got := request.Header.Get("X-Apteva-Operator-ID"); got != principal.operator {
					t.Errorf("operator=%q, want %q", got, principal.operator)
				}
				if request.Header.Get(sdk.HeaderBoundCallerInstallID) != "7" || request.Header.Get(sdk.HeaderBoundCallerAppName) != "functions" {
					t.Error("bound caller changed")
				}
				for _, name := range []string{"X-Apteva-Subject-Type", "X-Apteva-Subject-ID", sdk.HeaderTrustedPrincipal} {
					if request.Header.Get(name) != "" {
						t.Errorf("fabricated %s", name)
					}
				}
			})
		}
	}
}
