package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ─── fixtures ────────────────────────────────────────────────────────

// runtimeTestServer returns a server with an empty catalog ready for
// Register, a deterministic secret, and one registered user (id 1).
func runtimeTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()
	postJSON(t, s.handleRegister, map[string]string{
		"email": "runtime@test.com", "password": "password123",
	})
	return s
}

// registerRuntimeApp adds a catalog entry declaring a runtime block.
func registerRuntimeApp(s *Server, slug, providerKey string, env map[string]string) *AppTemplate {
	app := &AppTemplate{
		Slug: slug,
		Name: slug,
		Auth: AppAuthConfig{Types: []string{"api_key"}},
		Runtime: &AppRuntimeConfig{
			Role:        "llm",
			ProviderKey: providerKey,
			Env:         env,
		},
	}
	s.catalog.Register(app)
	return app
}

// addConnection creates a connection with the given credentials.
func addConnection(t *testing.T, s *Server, slug, name, projectID string, creds map[string]string) *Connection {
	t.Helper()
	blob, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	encrypted, err := Encrypt(s.secret, string(blob))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	conn, err := s.store.CreateConnection(1, slug, slug, name, "api_key", encrypted, projectID)
	if err != nil {
		t.Fatalf("CreateConnection(%s): %v", slug, err)
	}
	return conn
}

// ─── template rendering ──────────────────────────────────────────────

func TestResolveRuntimeTemplateNamespaces(t *testing.T) {
	src := runtimeTemplateSources{
		credentials: map[string]any{
			"api_key": "sk-live-123",
			"nested":  map[string]any{"access_token": "tok-abc"},
		},
		config:     map[string]any{"base_url": "https://example.test/v1", "embed_dim": float64(768)},
		connection: map[string]any{"id": int64(42), "provider_ref": int64(7)},
	}

	cases := []struct {
		name string
		tmpl string
		want string
	}{
		{"credentials", "{{credentials.api_key}}", "sk-live-123"},
		{"nested path", "{{credentials.nested.access_token}}", "tok-abc"},
		{"config", "{{config.base_url}}", "https://example.test/v1"},
		{"connection", "{{connection.provider_ref}}", "7"},
		{"literal passthrough", "no-placeholders", "no-placeholders"},
		{"mixed literal", "Bearer {{credentials.api_key}}", "Bearer sk-live-123"},
		// Numbers must not pick up a float tail — OLLAMA_EMBED_DIM is read
		// by core as an integer string.
		{"integral number", "{{config.embed_dim}}", "768"},
		// Anything unresolvable collapses the WHOLE template to "" so the
		// caller omits the var. A half-substituted value would be worse
		// than an absent one.
		{"unknown namespace", "{{secrets.api_key}}", ""},
		{"unknown key", "{{credentials.missing}}", ""},
		{"traverse through scalar", "{{credentials.api_key.deeper}}", ""},
		{"bare reference", "{{credentials}}", ""},
		{"partial resolve wipes all", "{{credentials.api_key}}/{{credentials.missing}}", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRuntimeTemplate(tc.tmpl, src); got != tc.want {
				t.Errorf("resolveRuntimeTemplate(%q) = %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

func TestRenderRuntimeEnvOmitsUnresolvedAndBadKeys(t *testing.T) {
	src := runtimeTemplateSources{
		credentials: map[string]any{"token": "tok-1"},
		config:      map[string]any{},
		connection:  map[string]any{},
	}
	runtime := &AppRuntimeConfig{
		Role:        "llm",
		ProviderKey: "openai",
		Env: map[string]string{
			"OPENAI_API_KEY":  "{{credentials.token}}",
			"OPENAI_BASE_URL": "{{config.base_url}}", // unset → omitted
			"lowercase_key":   "{{credentials.token}}",
		},
	}

	env := renderRuntimeEnv(runtime, src)

	if env["OPENAI_API_KEY"] != "tok-1" {
		t.Errorf("OPENAI_API_KEY = %q, want tok-1", env["OPENAI_API_KEY"])
	}
	if _, present := env["OPENAI_BASE_URL"]; present {
		t.Error("unresolved template should be omitted, not injected empty")
	}
	if _, present := env["lowercase_key"]; present {
		t.Error("non-env-var key should be skipped")
	}
	if len(env) != 1 {
		t.Errorf("env = %v, want exactly OPENAI_API_KEY", env)
	}
}

func TestBuildRuntimeSourcesProviderRef(t *testing.T) {
	s := runtimeTestServer(t)

	t.Run("falls back to connection id", func(t *testing.T) {
		conn := runtimeConnection{ID: 12}
		src, err := buildRuntimeSources(conn, s.secret)
		if err != nil {
			t.Fatalf("buildRuntimeSources: %v", err)
		}
		if got, _ := lookupRuntimeRef("connection.provider_ref", src); got != "12" {
			t.Errorf("provider_ref = %q, want 12", got)
		}
	})

	t.Run("prefers legacy provider id", func(t *testing.T) {
		conn := runtimeConnection{ID: 12, LegacyProviderID: 7}
		src, err := buildRuntimeSources(conn, s.secret)
		if err != nil {
			t.Fatalf("buildRuntimeSources: %v", err)
		}
		// Cores spawned before the migration hold the OLD id; handing back
		// the connection id instead would silently break token refresh.
		if got, _ := lookupRuntimeRef("connection.provider_ref", src); got != "7" {
			t.Errorf("provider_ref = %q, want 7 (legacy id)", got)
		}
	})

	t.Run("malformed runtime_config is not fatal", func(t *testing.T) {
		conn := runtimeConnection{ID: 3, RuntimeConfig: "{not json"}
		if _, err := buildRuntimeSources(conn, s.secret); err != nil {
			t.Fatalf("bad runtime_config should not fail the boot: %v", err)
		}
	})
}

// ─── ordering ────────────────────────────────────────────────────────

func TestListRuntimeConnectionsOrdering(t *testing.T) {
	s := runtimeTestServer(t)
	const project = "proj-1"

	global := addConnection(t, s, "opencode-go", "global", "", map[string]string{"api_key": "global"})
	first := addConnection(t, s, "opencode-go", "project-1", project, map[string]string{"api_key": "first"})
	second := addConnection(t, s, "opencode-go", "project-2", project, map[string]string{"api_key": "second"})

	conns, err := s.store.ListRuntimeConnections(1, project)
	if err != nil {
		t.Fatalf("ListRuntimeConnections: %v", err)
	}
	if len(conns) != 3 {
		t.Fatalf("expected 3 connections, got %d", len(conns))
	}
	// Project rows before global — a global credential must never
	// displace the selected project's own.
	if conns[0].ID != first.ID || conns[1].ID != second.ID || conns[2].ID != global.ID {
		t.Fatalf("order = %d,%d,%d; want %d,%d,%d",
			conns[0].ID, conns[1].ID, conns[2].ID, first.ID, second.ID, global.ID)
	}

	// is_primary breaks ties WITHIN a scope; it must not promote a global
	// row above a project one.
	if _, err := s.store.db.Exec(
		`UPDATE connections SET is_primary = 0 WHERE user_id = 1`); err != nil {
		t.Fatalf("clear primaries: %v", err)
	}
	if _, err := s.store.db.Exec(
		`UPDATE connections SET is_primary = 1 WHERE id IN (?, ?)`, second.ID, global.ID); err != nil {
		t.Fatalf("set primary: %v", err)
	}

	conns, err = s.store.ListRuntimeConnections(1, project)
	if err != nil {
		t.Fatalf("ListRuntimeConnections: %v", err)
	}
	if conns[0].ID != second.ID {
		t.Errorf("primary project row should sort first, got id=%d", conns[0].ID)
	}
	if conns[2].ID != global.ID {
		t.Errorf("primary global row must stay below project rows, got id=%d at index 2", conns[2].ID)
	}
}

func TestListRuntimeConnectionsSkipsInactive(t *testing.T) {
	s := runtimeTestServer(t)
	active := addConnection(t, s, "gemini", "live", "", map[string]string{"api_key": "k"})
	pending := addConnection(t, s, "anthropic-api", "half-done", "", map[string]string{})
	if err := s.store.UpdateConnectionStatus(pending.ID, "pending"); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}

	conns, err := s.store.ListRuntimeConnections(1)
	if err != nil {
		t.Fatalf("ListRuntimeConnections: %v", err)
	}
	// An abandoned OAuth handshake has no usable credential; injecting it
	// would surface as a 401 at first inference rather than "not connected".
	if len(conns) != 1 || conns[0].ID != active.ID {
		t.Fatalf("expected only the active connection, got %+v", conns)
	}
}

func TestPrimaryUniquenessIsEnforcedByIndex(t *testing.T) {
	s := runtimeTestServer(t)
	addConnection(t, s, "opencode-go", "one", "", map[string]string{"api_key": "1"})
	second := addConnection(t, s, "opencode-go", "two", "", map[string]string{"api_key": "2"})

	// The backfill marks the lowest id primary, so promoting a sibling
	// without demoting the first must be rejected by the partial unique
	// index rather than silently producing two primaries.
	if _, err := s.store.db.Exec(
		`UPDATE connections SET is_primary = 1 WHERE id = ?`, second.ID); err == nil {
		t.Fatal("expected unique index violation for a second primary in the same scope")
	}
}

func TestBackfillMarksLowestIDPrimary(t *testing.T) {
	s := runtimeTestServer(t)
	first := addConnection(t, s, "opencode-go", "one", "", map[string]string{"api_key": "1"})
	addConnection(t, s, "opencode-go", "two", "", map[string]string{"api_key": "2"})

	var primaryID int64
	if err := s.store.db.QueryRow(
		`SELECT id FROM connections WHERE app_slug = 'opencode-go' AND is_primary = 1`,
	).Scan(&primaryID); err != nil {
		t.Fatalf("query primary: %v", err)
	}
	// Reproduces the old providers-table "first wins" as a stated fact
	// rather than an artifact of the sort.
	if primaryID != first.ID {
		t.Errorf("primary = %d, want lowest id %d", primaryID, first.ID)
	}
}

// ─── env resolution ──────────────────────────────────────────────────

func TestRuntimeEnvFromConnections(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	// No runtime block → a plain integration.
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})

	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk-ant-1"})
	addConnection(t, s, "stripe", "Stripe", "", map[string]string{"token": "sk-stripe-live"})

	env, err := s.runtimeEnvFromConnections(1, nil)
	if err != nil {
		t.Fatalf("runtimeEnvFromConnections: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-1" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-1", env["ANTHROPIC_API_KEY"])
	}
	// The whole point of gating on the runtime block: an ordinary
	// integration's credential must never reach a core's environment.
	for name, value := range env {
		if value == "sk-stripe-live" {
			t.Fatalf("non-runtime connection leaked into env as %s", name)
		}
	}
	if len(env) != 1 {
		t.Errorf("env = %v, want only ANTHROPIC_API_KEY", env)
	}
}

func TestRuntimeEnvFirstConnectionWinsPerVar(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "opencode-go", "opencode-go", map[string]string{
		"OPENCODE_GO_API_KEY": "{{credentials.api_key}}",
	})
	const project = "proj-1"
	addConnection(t, s, "opencode-go", "global", "", map[string]string{"api_key": "global-key"})
	addConnection(t, s, "opencode-go", "project", project, map[string]string{"api_key": "project-key"})

	env, err := s.runtimeEnvFromConnections(1, nil, project)
	if err != nil {
		t.Fatalf("runtimeEnvFromConnections: %v", err)
	}
	// The env map and config.json must agree on which credential is in
	// play; if a global row stomped the env while the pool named the
	// project's provider, inference would authenticate as the wrong key.
	if env["OPENCODE_GO_API_KEY"] != "project-key" {
		t.Errorf("OPENCODE_GO_API_KEY = %q, want project-key", env["OPENCODE_GO_API_KEY"])
	}
}

func TestGetAllProviderEnvVarsUnmigratedConnectionDoesNotDisplaceProvider(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})

	// Legacy provider row storing the env var name as a literal key.
	blob, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY":    "from-provider-row",
		"FIREWORKS_API_KEY": "provider-only",
	})
	encrypted, err := Encrypt(s.secret, string(blob))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	// An independently-created Gemini connection. It is NOT the provider
	// row under a new name, so it must not take the key over.
	addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "from-connection"})

	env, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	// Letting the connection win here would silently change which Google
	// key every agent in the project authenticates with — the operator
	// connected an integration, they did not ask to re-key their agents.
	if env["GOOGLE_API_KEY"] != "from-provider-row" {
		t.Errorf("GOOGLE_API_KEY = %q, want the provider row to keep it", env["GOOGLE_API_KEY"])
	}
	// Providers with no connection at all are untouched either way.
	if env["FIREWORKS_API_KEY"] != "provider-only" {
		t.Errorf("FIREWORKS_API_KEY = %q, want provider-only", env["FIREWORKS_API_KEY"])
	}
}

func TestGetAllProviderEnvVarsMigratedConnectionTakesOver(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})

	blob, _ := json.Marshal(map[string]string{"GOOGLE_API_KEY": "from-provider-row"})
	encrypted, _ := Encrypt(s.secret, string(blob))
	provider, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "migrated"})
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = ? WHERE id = ?`, provider.ID, conn.ID); err != nil {
		t.Fatalf("stamp legacy_provider_id: %v", err)
	}

	env, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	// Stamped with the provider's id, this row IS that provider — so it
	// is the one that should serve the key.
	if env["GOOGLE_API_KEY"] != "migrated" {
		t.Errorf("GOOGLE_API_KEY = %q, want the migrated connection to win", env["GOOGLE_API_KEY"])
	}
}

func TestGetAllProviderEnvVarsConnectionOnlyKeyStillApplies(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	blob, _ := json.Marshal(map[string]string{"GOOGLE_API_KEY": "unrelated"})
	encrypted, _ := Encrypt(s.secret, string(blob))
	if _, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk-ant"})

	env, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	// Shadowing is per-variable, not global: a provider row for Google
	// must not stop an Anthropic connection contributing its own key.
	if env["ANTHROPIC_API_KEY"] != "sk-ant" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant", env["ANTHROPIC_API_KEY"])
	}
	if env["GOOGLE_API_KEY"] != "unrelated" {
		t.Errorf("GOOGLE_API_KEY = %q, want unrelated", env["GOOGLE_API_KEY"])
	}
}

// ─── pool resolution ─────────────────────────────────────────────────

func TestGetProviderPoolFromConnectionsOnly(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk-ant"})

	pool := s.GetProviderPool(1)
	if len(pool) == 0 {
		t.Fatal("expected a pool entry with zero provider rows present")
	}
	if pool[0].Type != "anthropic" {
		t.Errorf("pool[0].Type = %q, want anthropic", pool[0].Type)
	}
}

func TestGetProviderPoolUnmigratedConnectionDoesNotClaimProviderKey(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})

	blob, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY": "provider-key",
		"model_large":    "gemini-from-provider",
	})
	encrypted, _ := Encrypt(s.secret, string(blob))
	if _, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "conn-key"})
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID,
		`{"model_large":"gemini-from-connection"}`); err != nil {
		t.Fatalf("UpdateConnectionRuntimeConfig: %v", err)
	}

	googleEntries := poolEntriesFor(s.GetProviderPool(1), "google")
	// Still exactly one entry per provider key — but it is the provider
	// row's, because this connection was never migrated from it.
	if len(googleEntries) != 1 {
		t.Fatalf("expected 1 google entry, got %d", len(googleEntries))
	}
	if googleEntries[0].ModelLarge != "gemini-from-provider" {
		t.Errorf("ModelLarge = %q, want the provider row to keep the key",
			googleEntries[0].ModelLarge)
	}
}

func TestGetProviderPoolMigratedConnectionClaimsProviderKey(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})

	blob, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY": "provider-key",
		"model_large":    "gemini-from-provider",
	})
	encrypted, _ := Encrypt(s.secret, string(blob))
	provider, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "conn-key"})
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = ? WHERE id = ?`, provider.ID, conn.ID); err != nil {
		t.Fatalf("stamp legacy_provider_id: %v", err)
	}
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID,
		`{"model_large":"gemini-from-connection"}`); err != nil {
		t.Fatalf("UpdateConnectionRuntimeConfig: %v", err)
	}

	googleEntries := poolEntriesFor(s.GetProviderPool(1), "google")
	if len(googleEntries) != 1 {
		t.Fatalf("expected 1 google entry, got %d", len(googleEntries))
	}
	if googleEntries[0].ModelLarge != "gemini-from-connection" {
		t.Errorf("ModelLarge = %q, want the migrated connection's value",
			googleEntries[0].ModelLarge)
	}
}

func TestGetProviderPoolFallsBackWhenMigratedCopyIsUnreadable(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	blob, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY": "provider-key", "model_large": "provider-model",
	})
	encrypted, _ := Encrypt(s.secret, string(blob))
	provider, err := s.store.CreateProvider(1, 16, "llm", "Google", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "copy"})
	if _, err := s.store.db.Exec(`UPDATE connections
		SET legacy_provider_id = ?, encrypted_credentials = 'not-valid-ciphertext'
		WHERE id = ?`, provider.ID, conn.ID); err != nil {
		t.Fatal(err)
	}

	entries := poolEntriesFor(s.GetProviderPool(1), "google")
	if len(entries) != 1 || entries[0].ModelLarge != "provider-model" {
		t.Fatalf("pool = %+v, want readable legacy fallback", entries)
	}
}

func TestMigrationConflictCannotMoveLegacyDefaultBehindConnection(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 16, "Google", "", map[string]string{
		"GOOGLE_API_KEY": "legacy-google", "model_large": "google-model",
	})
	// The same-app mismatch makes Google deliberately remain in providers.
	addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "different-google"})
	// This unrelated connection must not jump ahead of the unresolved legacy
	// default merely because connections are assembled first internally.
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "anthropic"})
	if result := s.migrateProvidersToConnections(); result.Conflicts != 1 {
		t.Fatalf("migration result = %+v", result)
	}

	pool := s.GetProviderPool(1)
	if len(pool) < 2 || pool[0].Type != "google" {
		t.Fatalf("pool order = %+v, want unresolved legacy Google first", pool)
	}
}

func TestMigratedProjectProviderStaysAheadOfOlderGlobalAfterCleanup(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 16, "Google", "", map[string]string{"GOOGLE_API_KEY": "global-first-id"})
	seedProvider(t, s, 3, "Anthropic", "project-1", map[string]string{"ANTHROPIC_API_KEY": "project-later-id"})
	if result := s.migrateProvidersToConnections(); result.Migrated != 2 {
		t.Fatalf("migration result = %+v", result)
	}
	if _, err := s.store.db.Exec(`DELETE FROM providers`); err != nil {
		t.Fatal(err)
	}
	pool := s.GetProviderPool(1, "project-1")
	if len(pool) < 2 || pool[0].Type != "anthropic" {
		t.Fatalf("pool order = %+v, want project-scoped Anthropic first", pool)
	}
}

func poolEntriesFor(pool []ProviderInfo, providerKey string) []ProviderInfo {
	var out []ProviderInfo
	for _, info := range pool {
		if info.Type == providerKey {
			out = append(out, info)
		}
	}
	return out
}

func TestGetProviderPoolRejectsUnknownProviderKey(t *testing.T) {
	s := runtimeTestServer(t)
	// Catalog JSON ships separately from the binary; a provider_key core
	// has no factory for must not reach config.json.
	registerRuntimeApp(s, "rogue-llm", "definitely-not-a-provider", map[string]string{
		"ROGUE_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "rogue-llm", "Rogue", "", map[string]string{"api_key": "x"})

	for _, info := range s.GetProviderPool(1) {
		if info.Type == "definitely-not-a-provider" {
			t.Fatal("unknown provider key reached the pool")
		}
	}
}

func TestGetProviderPoolCodexSortsLast(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	registerRuntimeApp(s, "openai-codex", "openai-codex", map[string]string{
		"OPENAI_CODEX_ACCESS_TOKEN": "{{credentials.access_token}}",
	})
	// Created first, so only the explicit Codex ordering can put it last.
	codex := addConnection(t, s, "openai-codex", "Codex", "", map[string]string{
		"access_token": "tok", "account_id": "acct",
	})
	// Pin all three tiers plus capabilities so the pool builder considers
	// the catalog hydrated and makes no upstream call from a unit test.
	if err := s.store.UpdateConnectionRuntimeConfig(codex.ID,
		`{"model_large":"gpt-5.5","model_medium":"gpt-5.5","model_small":"gpt-5.5","model_capabilities":{}}`); err != nil {
		t.Fatalf("UpdateConnectionRuntimeConfig: %v", err)
	}
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk-ant"})

	pool := s.GetProviderPool(1)
	if len(pool) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(pool))
	}
	// pool[0] is the default provider; Codex is a subscription fallback.
	if pool[0].Type != "anthropic" {
		t.Errorf("pool[0].Type = %q, want anthropic to be the default", pool[0].Type)
	}
}

// ─── catalog endpoint ────────────────────────────────────────────────

func TestRuntimeCatalogForRole(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})
	s.catalog.Register(&AppTemplate{
		Slug: "voyage", Name: "Voyage",
		Runtime: &AppRuntimeConfig{Role: "embeddings", ProviderKey: "voyage",
			Env: map[string]string{"VOYAGE_API_KEY": "{{credentials.api_key}}"}},
	})

	entries := s.runtimeCatalogForRole("llm")
	if len(entries) != 2 {
		t.Fatalf("expected 2 llm entries, got %d (%+v)", len(entries), entries)
	}
	// Sorted by provider key for a stable picker order.
	if entries[0].ProviderKey != "anthropic" || entries[1].ProviderKey != "google" {
		t.Errorf("provider keys = %s,%s; want anthropic,google",
			entries[0].ProviderKey, entries[1].ProviderKey)
	}
	if len(entries[0].EnvVars) != 1 || entries[0].EnvVars[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("EnvVars = %v, want [ANTHROPIC_API_KEY]", entries[0].EnvVars)
	}
}

func TestCatalogEndpointRuntimeRoleFilter(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})

	req := httptest.NewRequest(http.MethodGet, "/integrations/catalog?runtime_role=llm", nil)
	rec := httptest.NewRecorder()
	s.handleListCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var entries []RuntimeCatalogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if len(entries) != 1 || entries[0].Slug != "anthropic-api" {
		t.Fatalf("entries = %+v, want only anthropic-api", entries)
	}
	// The credential form needs auth types; the old provider-types form
	// had to guess input types from env var names instead.
	if len(entries[0].AuthTypes) == 0 {
		t.Error("expected auth_types on a runtime catalog entry")
	}
}

func TestCatalogEndpointWithoutRuntimeRoleIsUnchanged(t *testing.T) {
	s := runtimeTestServer(t)
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})

	req := httptest.NewRequest(http.MethodGet, "/integrations/catalog", nil)
	rec := httptest.NewRecorder()
	s.handleListCatalog(rec, req)

	var apps []AppSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
		t.Fatalf("decode AppSummary list: %v", err)
	}
	if len(apps) != 1 || apps[0].Slug != "stripe" {
		t.Fatalf("apps = %+v, want the unfiltered summary shape", apps)
	}
}

// ─── pool position (default-provider regression) ─────────────────────

// TestGetProviderPoolMigratedGroupKeepsPosition reproduces the field
// report that broke unpinned agents: Venice existed only as a connection
// (low id) while Google was migrated from a provider row (fresh high id),
// so iteration order put Venice at pool[0] — and pool[0] IS the default
// for every agent without a default_provider pin. Migrated groups must
// keep the position their provider id gave them: first.
func TestGetProviderPoolMigratedGroupKeepsPosition(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "venice-ai", "venice", map[string]string{
		"VENICE_API_KEY": "{{credentials.token}}",
	})
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})

	// Venice connected long ago (low connection id, never a provider).
	addConnection(t, s, "venice-ai", "Venice", "", map[string]string{"token": "v"})
	// Google migrated from provider row 11 — fresh, high connection id.
	google := addConnection(t, s, "gemini", "Google", "", map[string]string{"api_key": "g"})
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = 11 WHERE id = ?`, google.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	pool := s.GetProviderPool(1)
	if len(pool) < 2 {
		t.Fatalf("pool = %+v, want venice and google", pool)
	}
	if pool[0].Type != "google" {
		t.Errorf("pool[0] = %q, want google (the migrated provider keeps its slot)", pool[0].Type)
	}
}

func TestGetProviderPoolNeverMigratedGroupsFollowConnectionOrder(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "venice-ai", "venice", map[string]string{
		"VENICE_API_KEY": "{{credentials.token}}",
	})
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "venice-ai", "Venice", "", map[string]string{"token": "v"})
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "a"})

	pool := s.GetProviderPool(1)
	if len(pool) < 2 {
		t.Fatalf("pool = %+v", pool)
	}
	// No legacy anchors → the order the operator connected the apps,
	// the natural analog of provider-add order.
	if pool[0].Type != "venice" || pool[1].Type != "anthropic" {
		t.Errorf("order = %s,%s; want venice,anthropic", pool[0].Type, pool[1].Type)
	}
}

// TestUnpinnedAgentDefaultsToMigratedProvider is the user-visible half:
// an agent with no default_provider must keep booting on the provider
// that was pool[0] before the fusion, not on whichever connection has
// the lowest id.
func TestUnpinnedAgentDefaultsToMigratedProvider(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "venice-ai", "venice", map[string]string{
		"VENICE_API_KEY": "{{credentials.token}}",
	})
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "venice-ai", "Venice", "", map[string]string{"token": "v"})
	google := addConnection(t, s, "gemini", "Google", "", map[string]string{"api_key": "g"})
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = 11 WHERE id = ?`, google.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	pool := s.GetProviderPool(1)
	providers := buildAgentCoreProviderConfigs(pool, `{}`)

	// The realtime companion of the default provider is also marked
	// default by design; only the text default matters here.
	var defaults []string
	for _, p := range providers {
		name, _ := p["name"].(string)
		if isDefault, _ := p["default"].(bool); isDefault && !isRealtimeProviderType(name) {
			defaults = append(defaults, name)
		}
	}
	if len(defaults) != 1 || defaults[0] != "google" {
		t.Errorf("unpinned text default = %v, want [google]", defaults)
	}
}

// An explicit pin must keep beating position, whatever the order.
func TestPinnedAgentIgnoresPoolPosition(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "venice-ai", "venice", map[string]string{
		"VENICE_API_KEY": "{{credentials.token}}",
	})
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "venice-ai", "Venice", "", map[string]string{"token": "v"})
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "a"})

	providers := buildAgentCoreProviderConfigs(s.GetProviderPool(1),
		`{"default_provider":"anthropic"}`)
	for _, p := range providers {
		name, _ := p["name"].(string)
		isDefault, _ := p["default"].(bool)
		if name == "anthropic" && !isDefault {
			t.Error("pinned provider not marked default")
		}
		if name == "venice" && isDefault {
			t.Error("pool[0] stole the default from an explicit pin")
		}
	}
}

func TestRuntimeConnectionsEndpointMatchesPoolOrder(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "venice-ai", "venice", map[string]string{
		"VENICE_API_KEY": "{{credentials.token}}",
	})
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "venice-ai", "Venice", "", map[string]string{"token": "v"})
	google := addConnection(t, s, "gemini", "Google", "", map[string]string{"api_key": "g"})
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = 11 WHERE id = ?`, google.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/connections/runtime", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListRuntimeConnections(rec, req)

	var out []runtimeConnectionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) < 2 {
		t.Fatalf("out = %+v", out)
	}
	// The dashboard falls back to its first row exactly where the server
	// falls back to pool[0]; they must not disagree about "first".
	if out[0].ProviderKey != "google" {
		t.Errorf("endpoint first = %q, want google to match the pool", out[0].ProviderKey)
	}
}
