package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// runtimeEnabledSlugs is every catalog app expected to back an agent
// runtime. Together these replace the provider_types table — each entry
// here corresponds to a row that used to live there.
var runtimeEnabledSlugs = map[string]string{
	"anthropic-api": "anthropic",
	"fireworks":     "fireworks",
	"gemini":        "google",
	"nvidia-nim":    "nvidia",
	"ollama":        "ollama",
	"openai-api":    "openai",
	"openai-codex":  "openai-codex",
	"opencode-go":   "opencode-go",
	"venice-ai":     "venice",
	"xai":           "xai",
}

// TestEmbeddedRuntimeCatalogEntries checks the JSON actually shipped in
// the binary, not a hand-built fixture. The embed is synced from
// integrations/src/apps by the build scripts, so this also catches a
// catalog edit that never made it into server/integrations-catalog.
func TestEmbeddedRuntimeCatalogEntries(t *testing.T) {
	for slug, wantKey := range runtimeEnabledSlugs {
		t.Run(slug, func(t *testing.T) {
			raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
			if err != nil {
				t.Fatalf("read embedded catalog: %v", err)
			}
			var app AppTemplate
			if err := json.Unmarshal(raw, &app); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if app.Runtime == nil {
				t.Fatal("missing runtime block — this app cannot back an agent")
			}
			if app.Runtime.Role != "llm" {
				t.Errorf("role = %q, want llm", app.Runtime.Role)
			}
			if app.Runtime.ProviderKey != wantKey {
				t.Errorf("provider_key = %q, want %q", app.Runtime.ProviderKey, wantKey)
			}
			// A key core has no factory for would be dropped silently by
			// the pool builder, leaving a connected provider invisible.
			if !isLLMKey(app.Runtime.ProviderKey) {
				t.Errorf("provider_key %q is not in the runtime allow-list", app.Runtime.ProviderKey)
			}
			if len(app.Runtime.Env) == 0 {
				t.Error("runtime block declares no env vars")
			}
			if len(app.Auth.Types) == 0 {
				t.Error("no auth types — the credential form would render nothing")
			}
		})
	}
}

// TestEmbeddedRuntimeEnvTemplatesAreResolvable rejects templates that
// could never resolve at runtime. A typo here fails open — the env var is
// simply omitted — so an agent would boot without its API key and the
// first inference would 401 with nothing pointing back at the catalog.
func TestEmbeddedRuntimeEnvTemplatesAreResolvable(t *testing.T) {
	validNamespaces := map[string]bool{"credentials": true, "config": true, "connection": true}

	for slug := range runtimeEnabledSlugs {
		raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
		if err != nil {
			t.Fatalf("%s: read embedded catalog: %v", slug, err)
		}
		var app AppTemplate
		if err := json.Unmarshal(raw, &app); err != nil {
			t.Fatalf("%s: decode: %v", slug, err)
		}

		credentialFields := map[string]bool{}
		for _, field := range app.Auth.CredentialFields {
			credentialFields[field.Name] = true
		}

		for name, tmpl := range app.Runtime.Env {
			if !isEnvVar(name) {
				t.Errorf("%s: env key %q is not UPPER_CASE and would be skipped", slug, name)
			}
			for _, ref := range templateRefs(tmpl) {
				parts := strings.SplitN(ref, ".", 2)
				if len(parts) != 2 || !validNamespaces[parts[0]] {
					t.Errorf("%s: %s references unknown namespace in %q", slug, name, ref)
					continue
				}
				// A credentials.X reference must name a field the connect
				// form actually collects, otherwise the operator fills in
				// the form and the var still never appears. OAuth apps are
				// exempt: their credential keys are minted by the token
				// exchange, not typed by a user.
				if parts[0] == "credentials" && len(credentialFields) > 0 {
					root := strings.SplitN(parts[1], ".", 2)[0]
					if !credentialFields[root] {
						t.Errorf("%s: %s reads credentials.%s but no such credential_field exists",
							slug, name, root)
					}
				}
			}
		}
	}
}

// TestEmbeddedRuntimeProviderKeysAreUnique — two apps claiming the same
// provider_key would make the pool depend on catalog iteration order,
// so whichever credential won would vary between boots.
func TestEmbeddedRuntimeProviderKeysAreUnique(t *testing.T) {
	seen := map[string]string{}
	for slug := range runtimeEnabledSlugs {
		raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		var app AppTemplate
		if err := json.Unmarshal(raw, &app); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if other, clash := seen[app.Runtime.ProviderKey]; clash {
			t.Errorf("provider_key %q claimed by both %s and %s",
				app.Runtime.ProviderKey, other, slug)
		}
		seen[app.Runtime.ProviderKey] = slug
	}
}

// TestRuntimeCatalogCoversEveryLLMKey guards the other direction: every
// provider apteva-core can construct should be reachable from the
// catalog, or a user has no way to configure it once the providers table
// is gone.
func TestRuntimeCatalogCoversEveryLLMKey(t *testing.T) {
	covered := map[string]bool{}
	for _, key := range runtimeEnabledSlugs {
		covered[key] = true
	}
	for _, key := range []string{
		"fireworks", "openai", "openai-codex", "anthropic", "google",
		"ollama", "nvidia", "opencode-go", "venice", "xai",
	} {
		if !covered[key] {
			t.Errorf("provider key %q has no catalog entry — unconfigurable after the fusion", key)
		}
	}
}

// templateRefs extracts the {{...}} references from a template string.
func templateRefs(tmpl string) []string {
	var refs []string
	rest := tmpl
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			return refs
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			return refs
		}
		end += start
		refs = append(refs, strings.TrimSpace(rest[start+2:end]))
		rest = rest[end+2:]
	}
}
