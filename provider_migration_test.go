package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// seedProvider writes a legacy provider row whose blob is keyed by env
// var name, the way the providers table always stored credentials.
func seedProvider(t *testing.T, s *Server, typeID int64, name, projectID string, blob map[string]string) *Provider {
	t.Helper()
	encoded, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encrypted, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	provider, err := s.store.CreateProvider(1, typeID, "llm", name, encrypted, projectID)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	return provider
}

func seedProviderState(t *testing.T, s *Server, typeID int64, name, projectID string, blob map[string]any) *Provider {
	t.Helper()
	encoded, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encrypted, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	provider, err := s.store.CreateProvider(1, typeID, "llm", name, encrypted, projectID)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	return provider
}

func connectionCredentials(t *testing.T, s *Server, connID int64) map[string]string {
	t.Helper()
	_, encrypted, err := s.store.GetConnection(1, connID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	plaintext, err := Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	creds := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &creds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return creds
}

// ─── translation ─────────────────────────────────────────────────────

func TestCredentialFieldForTemplate(t *testing.T) {
	cases := []struct {
		tmpl  string
		field string
		ok    bool
	}{
		{"{{credentials.api_key}}", "api_key", true},
		{"{{ credentials.token }}", "token", true},
		// Composites have no unambiguous inverse — we cannot tell which
		// part of the stored value was the credential.
		{"Bearer {{credentials.token}}", "", false},
		// config/connection values are derived, not stored, so a provider
		// blob has nothing to hand over.
		{"{{config.base_url}}", "", false},
		{"{{connection.provider_ref}}", "", false},
		// Nested paths describe OAuth state the device flow mints.
		{"{{credentials.nested.access_token}}", "", false},
		{"", "", false},
		{"literal", "", false},
	}
	for _, tc := range cases {
		field, ok := credentialFieldForTemplate(tc.tmpl)
		if ok != tc.ok || field != tc.field {
			t.Errorf("credentialFieldForTemplate(%q) = (%q,%v), want (%q,%v)",
				tc.tmpl, field, ok, tc.field, tc.ok)
		}
	}
}

func TestMigrationTranslatesEnvKeysToCredentialFields(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	provider := seedProvider(t, s, 3, "Anthropic", "", map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-secret",
	})

	result := s.migrateProvidersToConnections()
	if result.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1 (conflicts=%d skipped=%d)",
			result.Migrated, result.Conflicts, result.Skipped)
	}

	connID, err := s.store.connectionForLegacyProvider(1, provider.ID)
	if err != nil || connID == 0 {
		t.Fatalf("no connection stamped with legacy_provider_id=%d: %v", provider.ID, err)
	}
	// The blob was keyed ANTHROPIC_API_KEY; the connection must store it
	// under the catalog's field name so runtime.env can map it back.
	creds := connectionCredentials(t, s, connID)
	if creds["api_key"] != "sk-ant-secret" {
		t.Errorf("credentials = %v, want api_key=sk-ant-secret", creds)
	}
	if _, leaked := creds["ANTHROPIC_API_KEY"]; leaked {
		t.Error("env var name leaked into the connection's credential fields")
	}
}

func TestMigrationProducesIdenticalEnv(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 3, "Anthropic", "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})

	before, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	s.migrateProvidersToConnections()
	after, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("after: %v", err)
	}

	// The whole point: migrating changes where the credential lives, not
	// what apteva-core receives.
	if before["ANTHROPIC_API_KEY"] != after["ANTHROPIC_API_KEY"] {
		t.Errorf("env changed across migration: %q → %q",
			before["ANTHROPIC_API_KEY"], after["ANTHROPIC_API_KEY"])
	}
	if after["ANTHROPIC_API_KEY"] != "sk-ant" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant", after["ANTHROPIC_API_KEY"])
	}
}

// ─── safety rules ────────────────────────────────────────────────────

func TestMigrationIsIdempotent(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 3, "Anthropic", "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})

	first := s.migrateProvidersToConnections()
	second := s.migrateProvidersToConnections()
	third := s.migrateProvidersToConnections()

	if first.Migrated != 1 {
		t.Fatalf("first run migrated = %d, want 1", first.Migrated)
	}
	// It runs on every boot; a second pass must not duplicate the row.
	if second.Migrated != 0 || third.Migrated != 0 {
		t.Errorf("reruns migrated %d/%d, want 0", second.Migrated, third.Migrated)
	}

	var count int
	if err := s.store.db.QueryRow(
		`SELECT count(*) FROM connections WHERE app_slug = 'anthropic-api'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("connection count = %d, want 1", count)
	}
}

// TestMigrationRefusesConflictingCredential is the case that made this
// migration necessary: on a real install a Google provider row and a
// Gemini connection held DIFFERENT keys. Picking either silently changes
// which credential the operator's agents authenticate with.
func TestMigrationRefusesConflictingCredential(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	provider := seedProvider(t, s, 16, "Google", "", map[string]string{
		"GOOGLE_API_KEY": "key-from-provider",
	})
	existing := addConnection(t, s, "gemini", "Gemini", "", map[string]string{
		"api_key": "key-from-connection",
	})

	result := s.migrateProvidersToConnections()
	if result.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1 (migrated=%d)", result.Conflicts, result.Migrated)
	}
	if result.Migrated != 0 {
		t.Error("a conflicting provider must not be migrated")
	}

	// Both rows survive untouched, and neither is stamped — so the
	// provider keeps serving the key until a human resolves it.
	if _, err := s.store.connectionForLegacyProvider(1, provider.ID); err == nil {
		t.Error("conflicting provider should not have been stamped onto a connection")
	}
	creds := connectionCredentials(t, s, existing.ID)
	if creds["api_key"] != "key-from-connection" {
		t.Errorf("existing connection was modified: %v", creds)
	}

	env, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	if env["GOOGLE_API_KEY"] != "key-from-provider" {
		t.Errorf("GOOGLE_API_KEY = %q, want the provider row to keep serving it",
			env["GOOGLE_API_KEY"])
	}
}

// An identical credential is not a conflict — someone already moved the
// key across by hand, so the provider row is simply redundant.
func TestMigrationAcceptsIdenticalExistingConnection(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	provider := seedProvider(t, s, 16, "Google", "", map[string]string{"GOOGLE_API_KEY": "same-key"})
	existing := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "same-key"})
	if err := s.store.UpdateConnectionRuntimeConfig(existing.ID, `{"unrelated":"keep"}`); err != nil {
		t.Fatal(err)
	}

	result := s.migrateProvidersToConnections()
	if result.Conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", result.Conflicts)
	}
	if result.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", result.Migrated)
	}
	connID, err := s.store.connectionForLegacyProvider(1, provider.ID)
	if err != nil {
		t.Errorf("provider should have been stamped: %v", err)
	}
	if connID != existing.ID {
		t.Errorf("stamped connection = %d, want existing %d", connID, existing.ID)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT count(*) FROM connections WHERE app_slug='gemini'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("connection count = %d, want reuse without duplication", count)
	}
	config, _ := s.store.GetConnectionRuntimeConfig(1, existing.ID)
	if config["unrelated"] != "keep" {
		t.Errorf("existing runtime config was discarded: %v", config)
	}
}

func TestMigrationIsNonDestructive(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	provider := seedProvider(t, s, 3, "Anthropic", "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})

	s.migrateProvidersToConnections()

	// Dual-read still serves the provider row; deleting rows is a
	// separate, deliberate step after the connection path is trusted.
	if _, _, err := s.store.GetProvider(1, provider.ID); err != nil {
		t.Errorf("provider row was removed by the migration: %v", err)
	}
}

func TestMigrationScopeIsPreserved(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 3, "Anthropic", "proj-9", map[string]string{"ANTHROPIC_API_KEY": "scoped"})

	if result := s.migrateProvidersToConnections(); result.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", result.Migrated)
	}

	var projectID string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(project_id,'') FROM connections WHERE app_slug = 'anthropic-api'`,
	).Scan(&projectID); err != nil {
		t.Fatalf("query: %v", err)
	}
	// A project-scoped key promoted to global would leak across every
	// project the user owns.
	if projectID != "proj-9" {
		t.Errorf("project_id = %q, want proj-9", projectID)
	}
}

func TestMigrationSkipsProvidersWithoutCatalogEntry(t *testing.T) {
	s := runtimeTestServer(t)
	// No runtime app registered at all.
	seedProvider(t, s, 8, "Browserbase", "", map[string]string{"BROWSERBASE_API_KEY": "bb"})

	result := s.migrateProvidersToConnections()
	if result.Migrated != 0 {
		t.Errorf("migrated = %d, want 0 for a provider with no catalog counterpart", result.Migrated)
	}
}

func TestMigrationSkipsBlobWithNoMappableField(t *testing.T) {
	s := runtimeTestServer(t)
	// A catalog entry with only row-derived output still cannot manufacture
	// credentials from an unrelated provider blob.
	registerRuntimeApp(s, "openai-codex", "openai-codex", map[string]string{
		"OPENAI_CODEX_PROVIDER_ID": "{{connection.provider_ref}}",
	})
	seedProvider(t, s, 15, "OpenAI Codex", "", map[string]string{"unrelated": "value"})

	result := s.migrateProvidersToConnections()
	if result.Migrated != 0 {
		t.Errorf("migrated = %d, want 0 when nothing maps", result.Migrated)
	}
}

func TestMigrationTranslatesStructuredCodexState(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "openai-codex", "openai-codex", map[string]string{
		"OPENAI_CODEX_ACCESS_TOKEN": "{{credentials.access_token}}",
		"OPENAI_CODEX_ACCOUNT_ID":   "{{credentials.account_id}}",
		"OPENAI_CODEX_PROVIDER_ID":  "{{connection.provider_ref}}",
	})
	provider := seedProviderState(t, s, 15, "OpenAI Codex", "", map[string]any{
		"auth": map[string]any{"provider": "openai-codex"},
		"credentials": map[string]any{
			"access_token":  "codex-access",
			"refresh_token": "codex-refresh",
			"expires_at":    "2099-01-01T00:00:00Z",
		},
		"account":     map[string]any{"chatgpt_account_id": "acct-7"},
		"model_large": "gpt-codex-pinned",
	})

	if result := s.migrateProvidersToConnections(); result.Migrated != 1 {
		t.Fatalf("migration result = %+v", result)
	}
	connID, err := s.store.connectionForLegacyProvider(1, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	creds := connectionCredentials(t, s, connID)
	for key, want := range map[string]string{
		"access_token": "codex-access", "refresh_token": "codex-refresh", "account_id": "acct-7",
	} {
		if creds[key] != want {
			t.Errorf("%s = %q, want %q", key, creds[key], want)
		}
	}
	if _, err := s.store.db.Exec(`DELETE FROM providers WHERE id = ?`, provider.ID); err != nil {
		t.Fatal(err)
	}
	env, err := s.GetAllProviderEnvVars(1)
	if err != nil {
		t.Fatal(err)
	}
	if env["OPENAI_CODEX_ACCESS_TOKEN"] != "codex-access" || env["OPENAI_CODEX_ACCOUNT_ID"] != "acct-7" {
		t.Errorf("post-delete Codex env = %v", env)
	}
	if env["OPENAI_CODEX_PROVIDER_ID"] != fmt.Sprint(provider.ID) {
		t.Errorf("provider ref = %q, want legacy id %d", env["OPENAI_CODEX_PROVIDER_ID"], provider.ID)
	}
	if rec := getRuntimeToken(t, s, provider.ID); rec.Code != 200 {
		t.Fatalf("legacy runtime-token route after cleanup = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMigrationCarriesCatalogConfigFieldsAfterProviderDeletion(t *testing.T) {
	cases := []struct {
		name, slug, key string
		env             map[string]string
		blob            map[string]string
		wantConfig      map[string]any
	}{
		{
			name: "OpenAI", slug: "openai-api", key: "openai",
			env:        map[string]string{"OPENAI_API_KEY": "{{credentials.token}}", "OPENAI_BASE_URL": "{{config.base_url}}"},
			blob:       map[string]string{"OPENAI_API_KEY": "sk-openai", "OPENAI_BASE_URL": "https://gateway.example/v1"},
			wantConfig: map[string]any{"base_url": "https://gateway.example/v1"},
		},
		{
			name: "Ollama", slug: "ollama", key: "ollama",
			env: map[string]string{
				"OLLAMA_HOST": "{{credentials.host}}", "OLLAMA_MODEL": "{{config.model}}",
				"OLLAMA_EMBED_MODEL": "{{config.embed_model}}", "OLLAMA_EMBED_DIM": "{{config.embed_dim}}",
			},
			blob: map[string]string{
				"OLLAMA_HOST": "http://ollama:11434", "OLLAMA_MODEL": "qwen3",
				"OLLAMA_EMBED_MODEL": "nomic-embed-text", "OLLAMA_EMBED_DIM": "768",
			},
			wantConfig: map[string]any{"model": "qwen3", "embed_model": "nomic-embed-text", "embed_dim": "768"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runtimeTestServer(t)
			registerRuntimeApp(s, tc.slug, tc.key, tc.env)
			provider := seedProvider(t, s, 2, tc.name, "", tc.blob)
			if result := s.migrateProvidersToConnections(); result.Migrated != 1 {
				t.Fatalf("migration result = %+v", result)
			}
			connID, _ := s.store.connectionForLegacyProvider(1, provider.ID)
			config, _ := s.store.GetConnectionRuntimeConfig(1, connID)
			for key, want := range tc.wantConfig {
				if config[key] != want {
					t.Errorf("config[%s] = %v, want %v", key, config[key], want)
				}
			}
			if _, err := s.store.db.Exec(`DELETE FROM providers WHERE id = ?`, provider.ID); err != nil {
				t.Fatal(err)
			}
			env, err := s.GetAllProviderEnvVars(1)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.blob {
				if strings.HasPrefix(key, "model_") {
					continue
				}
				if env[key] != want {
					t.Errorf("post-delete env[%s] = %q, want %q", key, env[key], want)
				}
			}
		})
	}
}

func TestCreateMigratedConnectionRollsBackBeforeInsertOnInvalidConfig(t *testing.T) {
	s := runtimeTestServer(t)
	app := &AppTemplate{Slug: "gemini", Name: "Gemini", Auth: AppAuthConfig{Types: []string{"api_key"}}}
	err := s.createMigratedConnection(1, 44, "", "Google", app,
		map[string]string{"api_key": "key"}, map[string]any{"invalid": make(chan int)})
	if err == nil {
		t.Fatal("expected JSON encoding failure")
	}
	var count int
	_ = s.store.db.QueryRow(`SELECT count(*) FROM connections WHERE legacy_provider_id = 44`).Scan(&count)
	if count != 0 {
		t.Errorf("partial connection rows = %d, want 0", count)
	}
}

func TestLegacyProviderConnectionStampIsUniquePerUser(t *testing.T) {
	s := runtimeTestServer(t)
	first := addConnection(t, s, "gemini", "first", "", map[string]string{"api_key": "one"})
	second := addConnection(t, s, "gemini", "second", "", map[string]string{"api_key": "two"})
	if _, err := s.store.db.Exec(`UPDATE connections SET legacy_provider_id = 77 WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE connections SET legacy_provider_id = 77 WHERE id = ?`, second.ID); err == nil {
		t.Fatal("duplicate legacy_provider_id stamp should violate the unique index")
	}
}

func TestMigrationDoesNotAutoExposeTools(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 3, "Anthropic", "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})
	s.migrateProvidersToConnections()

	var autoMCP int
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(auto_mcp,1) FROM connections WHERE app_slug = 'anthropic-api'`).Scan(&autoMCP); err != nil {
		t.Fatalf("query: %v", err)
	}
	// A migration must not hand agents capabilities they did not have
	// yesterday — the provider row exposed no tools.
	if autoMCP != 0 {
		t.Error("migrated connection auto-exposed its REST tools as an MCP server")
	}
}

// ─── settings carried across ─────────────────────────────────────────

// TestMigrationCarriesModelPins is the regression that motivated this:
// p11 Google held model_large/medium/small="gemini-3.7-flash" and the
// first migration dropped them, so the agent silently fell back to
// apteva-core's default model for Google.
func TestMigrationCarriesModelPins(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	provider := seedProvider(t, s, 16, "Google", "", map[string]string{
		"GOOGLE_API_KEY": "key",
		"model_large":    "gemini-3.7-flash",
		"model_medium":   "gemini-3.7-flash",
		"model_small":    "gemini-3.7-flash",
	})

	if result := s.migrateProvidersToConnections(); result.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", result.Migrated)
	}

	connID, err := s.store.connectionForLegacyProvider(1, provider.ID)
	if err != nil {
		t.Fatalf("not stamped: %v", err)
	}
	config, err := s.store.GetConnectionRuntimeConfig(1, connID)
	if err != nil {
		t.Fatalf("GetConnectionRuntimeConfig: %v", err)
	}
	for _, key := range []string{"model_large", "model_medium", "model_small"} {
		if config[key] != "gemini-3.7-flash" {
			t.Errorf("%s = %v, want gemini-3.7-flash", key, config[key])
		}
	}
	// Settings belong in runtime_config, never in the credential blob.
	creds := connectionCredentials(t, s, connID)
	if _, leaked := creds["model_large"]; leaked {
		t.Error("model pin leaked into the encrypted credentials")
	}
}

func TestMigrationCarriesCapabilitiesAndBuiltinTools(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	blob := map[string]string{"GOOGLE_API_KEY": "key", "builtin_tools": `["search"]`}
	provider := seedProvider(t, s, 16, "Google", "", blob)

	s.migrateProvidersToConnections()
	connID, err := s.store.connectionForLegacyProvider(1, provider.ID)
	if err != nil {
		t.Fatalf("not stamped: %v", err)
	}
	config, _ := s.store.GetConnectionRuntimeConfig(1, connID)
	if config["builtin_tools"] != `["search"]` {
		t.Errorf("builtin_tools = %v, want the saved list", config["builtin_tools"])
	}
}

func TestMigratedModelPinsReachThePool(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	seedProvider(t, s, 16, "Google", "", map[string]string{
		"GOOGLE_API_KEY": "key",
		"model_large":    "gemini-3.7-flash",
		"model_medium":   "gemini-3.7-flash",
		"model_small":    "gemini-3.7-flash",
	})
	s.migrateProvidersToConnections()

	// End to end: what the pool hands apteva-core must be unchanged by
	// the migration, which is the only thing that actually matters.
	for _, info := range s.GetProviderPool(1) {
		if info.Type == "google" {
			if info.ModelLarge != "gemini-3.7-flash" {
				t.Errorf("pool ModelLarge = %q, want gemini-3.7-flash", info.ModelLarge)
			}
			return
		}
	}
	t.Fatal("google missing from pool")
}

func TestTranslateProviderRuntimeConfigSkipsSecretsAndBlanks(t *testing.T) {
	s := runtimeTestServer(t)
	blob, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY": "secret-key",
		"model_large":    "pinned",
		"model_small":    "   ",
	})
	encrypted, _ := Encrypt(s.secret, string(blob))

	config, err := translateProviderRuntimeConfig(s.secret, encrypted)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if config["model_large"] != "pinned" {
		t.Errorf("model_large = %v", config["model_large"])
	}
	// A blank tier is "unset", not a model id of "  ".
	if _, present := config["model_small"]; present {
		t.Error("blank tier should be omitted")
	}
	// runtime_config is not encrypted — a credential must never land here.
	if _, leaked := config["GOOGLE_API_KEY"]; leaked {
		t.Fatal("credential leaked into runtime_config")
	}
}
