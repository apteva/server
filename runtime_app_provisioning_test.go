package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestRuntimeAppProvisioningUsesOwnedClone(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesCall)
	other := seedRuntimeAPIInstall(t, s, "other-owner", sdk.PermRuntimesManage)
	database := seedRuntimeAPIInstall(t, s, "database")
	api := seedRuntimeAPIInstall(t, s, "api")
	token, err := s.appInstallToken(database)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := s.environments.Create(EnvironmentSpec{ID: "rt-provision", ProjectID: "proj-1", RuntimeOwnerInstallID: owner})
	if err != nil {
		t.Fatal(err)
	}
	const number = "9007199254740993"
	type forwardedRequest struct{ path, project, caller, auth, body string }
	requests := make(chan forwardedRequest, 1)
	var seen forwardedRequest
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- forwardedRequest{r.URL.Path, r.Header.Get("X-Apteva-Project-ID"), r.Header.Get("X-Apteva-Caller-Agent"), r.Header.Get("Authorization"), string(body)}
		for _, header := range []string{"X-Apteva-Operator-ID", "X-Apteva-Subject-ID", sdk.HeaderBoundCallerInstallID} {
			if r.Header.Get(header) != "" {
				t.Errorf("unexpected forwarded identity %s", header)
			}
		}
		switch r.URL.Path {
		case "/mcp":
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`)
		case "/operations/collection_create":
			http.Error(w, "schema conflict", http.StatusConflict)
		default:
			io.WriteString(w, `{"exact":`+number+`}`)
		}
	}))
	defer sidecar.Close()
	runtime.installs["database"] = &localInstall{InstallID: database, ProjectID: runtime.ID, SidecarURL: sidecar.URL}
	runtime.installs["api"] = &localInstall{InstallID: api, ProjectID: runtime.ID}
	t.Cleanup(func() {
		// The HTTP fixtures do not have supervisor processes to remove.
		delete(runtime.installs, "database")
		delete(runtime.installs, "api")
	})
	const path = "/apps/callback/runtimes/rt-provision/apps/database/call"
	call := func(install int64, tool, alias string, input map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{"tool": tool, "agent_alias": alias, "input": input})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("X-Apteva-App-Install-ID", itoa(install))
		req.Header.Set("X-Apteva-Project-ID", "proj-1")
		req.Header.Set("X-Apteva-Caller-Agent", "forged-agent")
		req.Header.Set("X-Apteva-Operator-ID", "999")
		req.Header.Set("X-Apteva-Subject-ID", "forged-subject")
		req.Header.Set(sdk.HeaderBoundCallerInstallID, "999")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		seen = forwardedRequest{}
		select {
		case seen = <-requests:
		default:
		}
		return rec
	}
	rec := call(owner, "db_insert", "", map[string]any{
		"_project_id": runtime.ID,
		"number":      json.Number(number),
		"refs":        []any{map[string]any{"$runtime_install_id": "api"}},
	})
	if rec.Code != 200 || seen.path != "/operations/insert" || seen.project != runtime.ID || seen.caller != "" || seen.auth != "Bearer "+token {
		t.Fatalf("operator dispatch failed: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Result struct {
			Content []struct{ Type, Text string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || len(result.Result.Content) != 1 ||
		result.Result.Content[0].Type != "text" || result.Result.Content[0].Text != `{"exact":`+number+`}` {
		t.Fatalf("SDK result lost exact JSON: %s", rec.Body.String())
	}
	var forwarded struct {
		Number int64
		Refs   []int64
	}
	if err := json.Unmarshal([]byte(seen.body), &forwarded); err != nil || forwarded.Number != 9007199254740993 ||
		len(forwarded.Refs) != 1 || forwarded.Refs[0] != api || strings.Contains(seen.body, "_project_id") {
		t.Fatalf("wrong seed payload: %s", seen.body)
	}

	for _, operation := range []string{"database_create", "access_grant", "get"} {
		if rec = call(owner, "db_"+operation, "", nil); rec.Code != 200 || seen.path != "/operations/"+operation {
			t.Fatalf("wrong dispatch for %s: HTTP %d, path %q", operation, rec.Code, seen.path)
		}
	}
	for _, field := range []string{"project_id", "_project_id"} {
		if rec = call(owner, "db_get", "", map[string]any{field: "proj-1"}); rec.Code != http.StatusBadRequest || seen.path != "" {
			t.Fatalf("accepted source-project override %s: %d", field, rec.Code)
		}
	}
	if rec = call(other, "db_get", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign owner got status %d", rec.Code)
	}
	if rec = call(owner, "db_get", "", map[string]any{"id": map[string]any{"$runtime_install_id": "parent"}}); rec.Code != 400 {
		t.Fatalf("unresolved clone reference got status %d", rec.Code)
	}
	if rec = call(owner, "db_collection_create", "", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "HTTP 409: schema conflict") {
		t.Fatalf("sidecar failure was lost: %d %s", rec.Code, rec.Body.String())
	}
	if rec = call(owner, "db_database_drop", "", nil); rec.Code != 200 || seen.path != "/mcp" {
		t.Fatal("unlisted tool reached the operator surface")
	}
	if _, err := s.store.db.Exec("UPDATE app_installs SET permissions_json=? WHERE id=?", `["platform.runtimes.call"]`, owner); err != nil {
		t.Fatal(err)
	}
	if rec = call(owner, "db_get", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("call-only grant reached operator route: %d", rec.Code)
	}
	if err := runtime.AttachAgent(&EnvironmentAgent{Alias: "reader", AgentID: 42}); err != nil {
		t.Fatal(err)
	}
	if rec = call(owner, "db_get", "reader", nil); rec.Code != 200 || seen.path != "/mcp" || seen.caller != "42" || seen.project != runtime.ID {
		t.Fatalf("agent did not use scoped MCP: %d %s", rec.Code, rec.Body.String())
	}
	runtime.installs["database"].ProjectID = "proj-1"
	if rec = call(owner, "db_get", "reader", nil); rec.Code != http.StatusBadRequest || seen.path != "" {
		t.Fatalf("non-clone installation reached a sidecar: %d %s", rec.Code, rec.Body.String())
	}
	delete(runtime.installs, "database")
	if rec = call(owner, "db_get", "reader", nil); rec.Code != http.StatusNotFound || seen.path != "" {
		t.Fatalf("missing clone reached a sidecar: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeOperatorOperationPropagatesCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer sidecar.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := callRuntimeOperatorOperation(ctx, &localInstall{SidecarURL: sidecar.URL, ProjectID: "rt-cancel"}, "test-token", "/operations/get", nil)
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("sidecar request did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("call returned %v, want cancellation", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("sidecar request was not canceled")
	}
}
