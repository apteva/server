package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realAppsWorkspace links the apps from APTEVA_APPS_SRC (default ../apps)
// into a temp workspace next to the app-sdk this server builds against, so
// every app is built with the same restore support.
func realAppsWorkspace(t *testing.T, names ...string) func(string) (string, error) {
	t.Helper()
	appsRoot := os.Getenv("APTEVA_APPS_SRC")
	if appsRoot == "" {
		appsRoot = filepath.Join("..", "apps")
	}
	appsRoot, _ = filepath.Abs(appsRoot)
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/apteva/app-sdk").Output()
	sdkDir := strings.TrimSpace(string(out))
	if err != nil || sdkDir == "" {
		t.Skipf("app-sdk source not available: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sdkDir, filepath.Join(root, "app-sdk")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		src := filepath.Join(appsRoot, "mcp", name)
		if _, err := os.Stat(filepath.Join(src, "apteva.yaml")); err != nil {
			t.Skipf("%s source not found under %s", name, appsRoot)
		}
		if err := os.Symlink(src, filepath.Join(root, "mcp", name)); err != nil {
			t.Fatal(err)
		}
	}
	return func(name string) (string, error) {
		dir := filepath.Join(root, "mcp", name)
		if _, err := os.Stat(filepath.Join(dir, "apteva.yaml")); err != nil {
			return "", err
		}
		return dir, nil
	}
}

// callEnvironmentTool runs one MCP tool as an agent of the environment and
// returns the decoded text result.
func callEnvironmentTool(t *testing.T, s *Server, envID, app, tool string, args map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest(http.MethodPost, "/environment-app-gateway/"+envID+"/agent-1/"+app+"/mcp", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleEnvironmentAppGateway(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s.%s: %d %s", app, tool, rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s.%s: %v %s", app, tool, err, rec.Body.String())
	}
	if env.Error != nil || env.Result.IsError || len(env.Result.Content) == 0 {
		t.Fatalf("%s.%s failed: %s", app, tool, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &out); err != nil {
		var list []any
		if json.Unmarshal([]byte(env.Result.Content[0].Text), &list) == nil {
			return map[string]any{"items": list}
		}
		return map[string]any{"text": env.Result.Content[0].Text}
	}
	return out
}

// TestEnvironmentSnapshotRoundTripRealApps snapshots a runtime with real
// Functions, API, Database and Auth data and restores it into a new runtime:
// everything stays visible under the new ids, the API route still reaches its
// function, and Auth's redirect URIs follow the new public URL.
func TestEnvironmentSnapshotRoundTripRealApps(t *testing.T) {
	requireRealAppEnvironmentTests(t)
	apps := []string{"functions", "api", "database", "auth"}
	resolve := realAppsWorkspace(t, apps...)
	s := newEnvironmentTestServer(t)
	s.environments.ResolveEnvironmentSource = func(_, name, _ string) (string, error) { return resolve(name) }
	spec := func(id string) EnvironmentSpec {
		dirs := map[string]string{}
		for _, name := range apps {
			dirs[name], _ = resolve(name)
		}
		return EnvironmentSpec{ID: id, ProjectID: "proj-roundtrip", GatewayURL: s.localGatewayURL(), CreatorUserID: 1,
			AppSrcDirs: dirs, NetworkMode: EdgeBlock, HealthBudget: 180 * time.Second}
	}

	source, err := s.environments.Create(spec("rt_round_src"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(source.ID)
	call := func(env *Environment, app, tool string, args map[string]any) map[string]any {
		return callEnvironmentTool(t, s, env.ID, app, tool, args)
	}
	call(source, "functions", "functions_create", map[string]any{"name": "hello", "runtime": "node",
		"source": "export default async (event) => ({ hello: event?.name ?? 'world' });"})
	call(source, "api", "api_create", map[string]any{"slug": "shop", "name": "Shop"})
	call(source, "api", "api_route_add", map[string]any{"api_slug": "shop", "method": "GET", "path_pattern": "/hello", "target_kind": "function", "target_ref": "hello"})
	sourceURL := s.runtimePlatformURL(source.ID)
	call(source, "auth", "auth_clients_create", map[string]any{"name": "site", "type": "spa",
		"redirect_uris": []any{sourceURL + "/api/apps/content/callback"}, "allowed_origins": []any{"https://shop.example"}})
	call(source, "database", "db_database_create", map[string]any{"database": "shop"})
	call(source, "database", "db_collection_create", map[string]any{"database": "shop", "collection": "orders", "fields": []any{map[string]any{"name": "total", "type": "number"}}})
	call(source, "database", "db_insert", map[string]any{"database": "shop", "collection": "orders", "records": []any{map[string]any{"id": "o1", "total": 5}}})

	if _, err := s.captureEnvironmentSnapshot(source, CaptureSpec{ID: "rt-round-snap", ProjectID: "proj-roundtrip"}); err != nil {
		t.Fatal(err)
	}
	restored, err := s.environments.CreateFromSnapshot(spec("rt_round_copy"), "rt-round-snap")
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(restored.ID)

	fn := call(restored, "functions", "functions_get", map[string]any{"name": "hello"})
	if fmt.Sprint(fn["name"]) != "hello" && !strings.Contains(fmt.Sprint(fn), "hello") {
		t.Fatalf("function after restore: %v", fn)
	}
	invoked := call(restored, "functions", "functions_invoke", map[string]any{"name": "hello", "event": map[string]any{"name": "restore"}})
	if !strings.Contains(fmt.Sprint(invoked), "restore") {
		t.Fatalf("function invoke after restore: %v", invoked)
	}
	routes := call(restored, "api", "api_route_list", map[string]any{"api_slug": "shop"})
	if !strings.Contains(fmt.Sprint(routes), "hello") {
		t.Fatalf("API routes after restore: %v", routes)
	}
	clients := call(restored, "auth", "auth_clients_list", map[string]any{})
	if got := fmt.Sprint(clients); !strings.Contains(got, s.runtimePlatformURL(restored.ID)+"/api/apps/content/callback") || strings.Contains(got, sourceURL+"/") {
		t.Fatalf("Auth clients after restore: %v", clients)
	}
	count := call(restored, "database", "db_count", map[string]any{"database": "shop", "collection": "orders"})
	if fmt.Sprint(count["count"]) != "1" {
		t.Fatalf("database after restore: %v", count)
	}

	// The API route reaches its function through the restored API.
	api, _ := restored.Install("api")
	resp, err := http.Get(api.SidecarURL + "/gw/shop/hello?name=gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var routed bytes.Buffer
	_, _ = routed.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(routed.String(), `"hello"`) {
		t.Fatalf("API route after restore: %d %s", resp.StatusCode, routed.String())
	}
}
