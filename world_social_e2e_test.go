package main

// world_social_e2e_test.go — the test that actually exercises the World
// system with a REAL app. It installs the real social sidecar from local
// source into a World (real build, real isolated SQLite, real migrations),
// drives its post_create tool over MCP, and asserts the real DB write.
//
// Gated: skips unless the social source tree is present (it builds the app),
// and under -short. Run with:  go test -run TestWorld_RealSocial -v

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// findAppSource locates an app's local working-copy dir, or skips.
func findAppSource(t *testing.T, app string) string {
	t.Helper()
	for _, c := range []string{
		filepath.Join("..", "apps", "mcp", app),
		filepath.Join("..", "app-"+app),
	} {
		if fi, err := os.Stat(filepath.Join(c, "apteva.yaml")); err == nil && !fi.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skipf("%s source not found (need apps/mcp/%s); skipping real-app world test", app, app)
	return ""
}

// newWorldTestServer builds the minimum Server needed to install + run an
// app in a World: a store, a secret, a real loopback listener (so in-world
// sidecars' gateway callbacks don't hang), a LocalSupervisor, and an empty
// installed-apps registry. No core, no production routes.
func newWorldTestServer(t *testing.T) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)

	s := &Server{
		store:          store,
		secret:         secret,
		port:           strconv.Itoa(port),
		dataDir:        dataDir,
		agents: NewAgentManager(filepath.Join(dataDir, "agents"), ""),
		// Stable cache OUTSIDE t.TempDir: Go's module cache is written
		// read-only, which breaks TempDir's RemoveAll cleanup; keeping it
		// here also reuses the build cache across runs. Per-install data
		// dirs under it are removed by World.Stop.
		localApps:      NewLocalSupervisor(filepath.Join(os.TempDir(), "apteva-world-test-appcache")),
		installedApps:  NewInstalledAppsRegistry(),
		broadcaster:    NewTelemetryBroadcaster(),
		instanceSecret: "world-e2e-secret",
		worlds:         NewWorldManager(filepath.Join(dataDir, "worlds")),
	}
	s.worlds.server = s

	// A catalog with a synthetic twitter-api app so the integration callback
	// can resolve the app + tool. The per-world interceptor short-circuits
	// before any real request is built, so a minimal tool def suffices.
	cat := NewAppCatalog()
	cat.apps["twitter-api"] = &AppTemplate{
		Slug:  "twitter-api",
		Name:  "Twitter",
		Tools: []AppToolDef{{Name: "post_tweet", Method: "POST", Path: "/2/tweets"}},
	}
	s.catalog = cat

	// A user so the dev-token callback (installed_by → user) resolves to a
	// real owner the seeded connection belongs to.
	if _, err := store.CreateUser("world-e2e@example.com", "x"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Mount the app-callback route so in-world sidecars' platform calls
	// (ExecuteIntegrationTool → /api/apps/callback/integrations/:id/execute)
	// reach the real handler → the per-world interceptor.
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/apps/callback/", s.authMiddleware(s.handleAppCallback))
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", apiMux))
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go httpServer.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { _ = httpServer.Close() })
	return s
}

// seedConnection inserts a world-scoped integration connection owned by the
// given install, and returns its id. Mirrors the real connections schema.
func seedConnection(t *testing.T, s *Server, projectID string, ownerInstallID int64) int64 {
	t.Helper()
	enc, err := Encrypt(s.secret, "{}")
	if err != nil {
		t.Fatalf("encrypt creds: %v", err)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO connections (user_id, app_slug, app_name, name, auth_type, encrypted_credentials, status, project_id, source, provider_id, external_id, created_via, owner_app_install_id, auto_mcp)
		 VALUES (1, 'twitter-api', 'Twitter', 'twitter', 'oauth2', ?, 'active', ?, 'local', 0, '', 'app_install', ?, 0)`,
		enc, projectID, ownerInstallID)
	if err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// callMCP issues one JSON-RPC call to an app's /mcp endpoint and returns the
// raw "result" (or fails on a top-level error).
func callMCP(t *testing.T, mcpURL, token, method string, params any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", mcpURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp %s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("mcp %s: bad response %q: %v", method, string(raw), err)
	}
	if env.Error != nil {
		t.Fatalf("mcp %s error: %s", method, env.Error.Message)
	}
	return env.Result
}

func TestWorld_RealSocial_DBWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("real-app world test builds the social sidecar")
	}
	srcDir := findAppSource(t, "social")
	s := newWorldTestServer(t)

	world, err := s.worlds.Create(WorldSpec{
		ID:           "e2e-social",
		GatewayURL:   s.localGatewayURL(),
		AppSrcDirs:   map[string]string{"social": srcDir},
		Mode:         EdgeBlock,
		HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create world with real social: %v", err)
	}
	defer world.Stop()

	inst, ok := world.Install("social")
	if !ok {
		t.Fatal("social install missing from world")
	}
	t.Logf("social installed: id=%d port=%d db=%s", inst.InstallID, inst.Port, inst.DBPath)
	token := fmt.Sprintf("dev-%d", inst.InstallID) // the install's dev token (set by installLocalSource)

	// 1. The real sidecar serves MCP — proves it booted in the World.
	res := callMCP(t, inst.SidecarURL+"/mcp", token, "tools/list", map[string]any{})
	if !bytes.Contains(res, []byte("post_create")) {
		t.Fatalf("tools/list missing post_create: %s", res)
	}

	// 2. Its isolated DB ran migrations — proves real, isolated persistence.
	dbPath, ok := world.AppDBPath("social")
	if !ok {
		t.Fatal("no AppDBPath for social")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("social db not at %s: %v", dbPath, err)
	}
	assertTableExists(t, dbPath, "posts")
	assertTableExists(t, dbPath, "social_accounts")

	// 3. Seed a destination account directly in the isolated DB, then drive
	//    post_create over MCP and assert the REAL row landed. project_id must
	//    match the sidecar's APTEVA_PROJECT_ID (= the world id).
	seedSocialAccount(t, dbPath, world.ID, 1)
	_ = callMCP(t, inst.SidecarURL+"/mcp", token, "tools/call", map[string]any{
		"name": "post_create",
		"arguments": map[string]any{
			"body":               "hello from the world test",
			"social_account_ids": []any{1},
		},
	})

	got := countRows(t, dbPath, `SELECT COUNT(*) FROM posts WHERE body = 'hello from the world test'`)
	if got != 1 {
		t.Fatalf("expected 1 real post row written by the in-world sidecar, got %d", got)
	}
	t.Logf("✓ real social sidecar wrote %d post row(s) to its isolated DB inside the World", got)
}

func assertTableExists(t *testing.T, dbPath, table string) {
	t.Helper()
	if countRows(t, dbPath, fmt.Sprintf(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='%s'`, table)) != 1 {
		t.Fatalf("table %q not found in %s (migrations didn't run?)", table, dbPath)
	}
}

func countRows(t *testing.T, dbPath, query string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func seedSocialAccount(t *testing.T, dbPath, projectID string, connID int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open rw %s: %v", dbPath, err)
	}
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO social_accounts (id, project_id, platform, connection_id, external_account_id, display_name, status)
		 VALUES (1, ?, 'twitter', ?, 'acct-x', 'Test Account', 'active')`,
		projectID, connID)
	if err != nil {
		t.Fatalf("seed social_account: %v", err)
	}
}

// TestWorld_RealSocial_InterceptorMocksTweet is the full-loop proof: the real
// social sidecar publishes a tweet via ExecuteIntegrationTool, the call hits
// the per-world interceptor (NOT the real Twitter), and social records the
// target as published from the mocked response — all inside the World.
func TestWorld_RealSocial_InterceptorMocksTweet(t *testing.T) {
	if testing.Short() {
		t.Skip("real-app world test builds the social sidecar")
	}
	srcDir := findAppSource(t, "social")
	s := newWorldTestServer(t)

	world, err := s.worlds.Create(WorldSpec{
		ID:         "e2e-social-tweet",
		GatewayURL: s.localGatewayURL(),
		AppSrcDirs: map[string]string{"social": srcDir},
		Mode:       EdgeBlock,
		// The interceptor that answers social's tweet, keyed to this world.
		IntegrationFixtures: []IntegrationFixture{{
			App:    "twitter-api",
			Tool:   "post_tweet",
			Status: 200,
			Data:   map[string]any{"data": map[string]any{"id": "mocked-tweet-1"}},
		}},
		HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create world: %v", err)
	}
	defer world.Stop()

	inst, ok := world.Install("social")
	if !ok {
		t.Fatal("social install missing")
	}
	token := fmt.Sprintf("dev-%d", inst.InstallID)
	dbPath, _ := world.AppDBPath("social")

	// Seed a world-scoped twitter connection owned by the social install,
	// plus the social_account that points at it.
	connID := seedConnection(t, s, world.ID, inst.InstallID)
	seedSocialAccount(t, dbPath, world.ID, connID)

	// Publish — inline (no schedule_at), so social calls post_tweet now.
	_ = callMCP(t, inst.SidecarURL+"/mcp", token, "tools/call", map[string]any{
		"name": "post_create",
		"arguments": map[string]any{
			"body":               "launch day!",
			"social_account_ids": []any{1},
		},
	})

	// The target is 'published' ONLY if the integration call succeeded —
	// and it can only succeed via the interceptor (there is no real Twitter,
	// and the edge would block a real api.twitter.com call).
	status := scalarString(t, dbPath, `SELECT status FROM post_targets ORDER BY id DESC LIMIT 1`)
	if status != "published" {
		t.Fatalf("expected post_target status 'published' (via interceptor), got %q", status)
	}
	gotID := scalarString(t, dbPath, `SELECT COALESCE(platform_post_id,'') FROM post_targets ORDER BY id DESC LIMIT 1`)
	t.Logf("✓ real social published via the per-world interceptor — platform_post_id=%q (mocked, no real Twitter call)", gotID)
}

func scalarString(t *testing.T, dbPath, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}
