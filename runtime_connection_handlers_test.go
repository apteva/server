package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func patchJSON(t *testing.T, s *Server, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(http.MethodPatch, path, reader)
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// ─── primary selection ───────────────────────────────────────────────

func TestSetConnectionPrimaryDemotesSibling(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "opencode-go", "opencode-go", map[string]string{
		"OPENCODE_GO_API_KEY": "{{credentials.api_key}}",
	})
	first := addConnection(t, s, "opencode-go", "one", "", map[string]string{"api_key": "1"})
	second := addConnection(t, s, "opencode-go", "two", "", map[string]string{"api_key": "2"})

	// Creation made the first one primary; promoting the second has to
	// demote it in the same transaction or the partial unique index
	// rejects the write.
	rec := patchJSON(t, s, s.handleSetConnectionPrimary,
		fmt.Sprintf("/connections/%d/primary", second.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var primaries []int64
	rows, err := s.store.db.Query(
		`SELECT id FROM connections WHERE app_slug = 'opencode-go' AND is_primary = 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		primaries = append(primaries, id)
	}
	if len(primaries) != 1 || primaries[0] != second.ID {
		t.Fatalf("primaries = %v, want exactly [%d]", primaries, second.ID)
	}
	_ = first
}

func TestSetConnectionPrimaryChangesPoolCredential(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "opencode-go", "opencode-go", map[string]string{
		"OPENCODE_GO_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "opencode-go", "one", "", map[string]string{"api_key": "key-one"})
	second := addConnection(t, s, "opencode-go", "two", "", map[string]string{"api_key": "key-two"})

	env, err := s.runtimeEnvFromConnections(1, nil)
	if err != nil {
		t.Fatalf("runtimeEnvFromConnections: %v", err)
	}
	if env["OPENCODE_GO_API_KEY"] != "key-one" {
		t.Fatalf("before: got %q, want key-one", env["OPENCODE_GO_API_KEY"])
	}

	rec := patchJSON(t, s, s.handleSetConnectionPrimary,
		fmt.Sprintf("/connections/%d/primary", second.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// The whole point of the control: the operator's pick is what the
	// next agent boots with.
	env, err = s.runtimeEnvFromConnections(1, nil)
	if err != nil {
		t.Fatalf("runtimeEnvFromConnections: %v", err)
	}
	if env["OPENCODE_GO_API_KEY"] != "key-two" {
		t.Errorf("after: got %q, want key-two", env["OPENCODE_GO_API_KEY"])
	}
}

func TestSetConnectionPrimaryRejectsNonRuntimeConnection(t *testing.T) {
	s := runtimeTestServer(t)
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})
	conn := addConnection(t, s, "stripe", "Stripe", "", map[string]string{"token": "sk"})

	rec := patchJSON(t, s, s.handleSetConnectionPrimary,
		fmt.Sprintf("/connections/%d/primary", conn.ID), nil)
	// There is no pool for an ordinary integration to be primary in;
	// recording the flag anyway would be a lie the UI could read back.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetConnectionPrimaryIsScopedPerProject(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "opencode-go", "opencode-go", map[string]string{
		"OPENCODE_GO_API_KEY": "{{credentials.api_key}}",
	})
	global := addConnection(t, s, "opencode-go", "global", "", map[string]string{"api_key": "g"})
	scoped := addConnection(t, s, "opencode-go", "scoped", "proj-1", map[string]string{"api_key": "p"})

	// Both are primary — in different scopes. Promoting within one scope
	// must not disturb the other.
	rec := patchJSON(t, s, s.handleSetConnectionPrimary,
		fmt.Sprintf("/connections/%d/primary", scoped.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var globalPrimary int
	if err := s.store.db.QueryRow(
		`SELECT is_primary FROM connections WHERE id = ?`, global.ID).Scan(&globalPrimary); err != nil {
		t.Fatalf("query: %v", err)
	}
	if globalPrimary != 1 {
		t.Error("promoting a project row must not demote the global one")
	}
}

// ─── runtime config ──────────────────────────────────────────────────

func TestRuntimeConfigPatchMerges(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "k"})
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID,
		`{"model_large":"gemini-3-pro","model_capabilities":{"gemini-3-pro":{}}}`); err != nil {
		t.Fatalf("seed runtime_config: %v", err)
	}

	rec := patchJSON(t, s, s.handleConnectionRuntimeConfig,
		fmt.Sprintf("/connections/%d/runtime-config", conn.ID),
		map[string]any{"model_small": "gemini-3-flash"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["model_small"] != "gemini-3-flash" {
		t.Errorf("model_small = %v, want gemini-3-flash", got["model_small"])
	}
	// A merge, not a replace: setting one tier must not discard the
	// capabilities blob the Codex hydration writes.
	if got["model_large"] != "gemini-3-pro" {
		t.Errorf("model_large = %v, want the untouched existing value", got["model_large"])
	}
	if _, ok := got["model_capabilities"]; !ok {
		t.Error("model_capabilities was dropped by a partial patch")
	}
}

func TestRuntimeConfigNullClearsKey(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "k"})
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID, `{"model_large":"pinned"}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := patchJSON(t, s, s.handleConnectionRuntimeConfig,
		fmt.Sprintf("/connections/%d/runtime-config", conn.ID),
		map[string]any{"model_large": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// null means "back to the provider default" — an empty string would
	// be indistinguishable from a real (if silly) model ID.
	if _, present := got["model_large"]; present {
		t.Error("null should remove the key, not store an empty value")
	}
}

func TestRuntimeConfigFeedsPool(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "gemini", "google", map[string]string{
		"GOOGLE_API_KEY": "{{credentials.api_key}}",
	})
	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "k"})

	rec := patchJSON(t, s, s.handleConnectionRuntimeConfig,
		fmt.Sprintf("/connections/%d/runtime-config", conn.ID),
		map[string]any{"model_large": "gemini-3-pro"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	pool := s.GetProviderPool(1)
	var found bool
	for _, info := range pool {
		if info.Type == "google" {
			found = true
			if info.ModelLarge != "gemini-3-pro" {
				t.Errorf("ModelLarge = %q, want the value just saved", info.ModelLarge)
			}
		}
	}
	if !found {
		t.Fatal("google entry missing from pool")
	}
}

// ─── models + usage endpoints ────────────────────────────────────────

func getWithUser(t *testing.T, handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestConnectionModelsRejectsNonRuntime(t *testing.T) {
	s := runtimeTestServer(t)
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})
	conn := addConnection(t, s, "stripe", "Stripe", "", map[string]string{"token": "sk"})

	rec := getWithUser(t, s.handleConnectionModels,
		fmt.Sprintf("/connections/%d/models", conn.ID))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// The providers path found its key by scanning the blob for a *_API_KEY
// suffix. This one reads the catalog's runtime.env, so a connection whose
// credential field is named something else still resolves — but one with
// no key at all must say so rather than calling upstream unauthenticated.
func TestConnectionModelsRequiresAKey(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	conn := addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{})

	rec := getWithUser(t, s.handleConnectionModels,
		fmt.Sprintf("/connections/%d/models", conn.ID))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a credential-less connection", rec.Code)
	}
}

func TestConnectionUsageUnsupportedProvider(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	conn := addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk"})

	rec := getWithUser(t, s.handleConnectionUsage,
		fmt.Sprintf("/connections/%d/usage", conn.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var snapshot ProviderUsageSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A provider with no quota endpoint reports supported=false rather
	// than erroring, so the card renders a neutral state.
	if snapshot.Supported {
		t.Error("anthropic has no subscription quota; want supported=false")
	}
}

// ─── list endpoint ───────────────────────────────────────────────────

func TestListRuntimeConnectionsEndpoint(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})
	addConnection(t, s, "anthropic-api", "Anthropic", "", map[string]string{"api_key": "sk-ant"})
	addConnection(t, s, "stripe", "Stripe", "", map[string]string{"token": "sk-stripe"})

	req := httptest.NewRequest(http.MethodGet, "/connections/runtime", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListRuntimeConnections(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out []runtimeConnectionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected only the runtime-backed connection, got %+v", out)
	}
	if out[0].ProviderKey != "anthropic" || out[0].Scope != "global" || !out[0].IsPrimary {
		t.Errorf("summary = %+v, want anthropic/global/primary", out[0])
	}
	// The list drives a settings screen; it must never carry secrets.
	if bytes.Contains(rec.Body.Bytes(), []byte("sk-ant")) {
		t.Fatal("credential leaked into the runtime connection list")
	}
}
