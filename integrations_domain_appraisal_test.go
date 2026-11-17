package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func domainAppraisalCatalog(t *testing.T, slug string) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func domainAppraisalTool(t *testing.T, app *AppTemplate, name string) *AppToolDef {
	t.Helper()
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app.Tools[i]
		}
	}
	t.Fatalf("missing %s.%s", app.Slug, name)
	return nil
}

func TestDomainAppraisalEmbeddedCatalogs(t *testing.T) {
	if _, err := integrationsCatalogFS.ReadFile("integrations-catalog/humbleworth.json"); !os.IsNotExist(err) {
		t.Fatalf("HumbleWorth must use the Replicate integration: %v", err)
	}
	for _, slug := range []string{"replicate", "bishopi", "estibot", "domainindex", "atom"} {
		t.Run(slug, func(t *testing.T) {
			app := domainAppraisalCatalog(t, slug)
			if app.Slug != slug || len(app.Auth.CredentialFields) == 0 {
				t.Fatalf("incomplete catalog: %s", slug)
			}
			embedded, _ := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
			source, err := os.ReadFile("../integrations/src/apps/" + slug + ".json")
			if err != nil || !bytes.Equal(embedded, source) {
				t.Fatalf("embedded catalog differs from source: %v", err)
			}
			if slug == "atom" {
				if app.Kind != "remote_mcp" || app.MCP == nil || app.MCP.URL != "https://mcp.atom.com/mcp" || len(app.Tools) != 0 {
					t.Fatal("Atom must discover tools through the hosted MCP")
				}
			} else if len(app.Tools) == 0 {
				t.Fatal("missing REST tools")
			}
		})
	}
}

func TestDomainAppraisalHumbleWorthRequest(t *testing.T) {
	app := domainAppraisalCatalog(t, "replicate")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/predictions" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected prediction request: %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		input, ok := body["input"].(map[string]any)
		if !ok || input["domains"] != "example.com,startup.net" || body["version"] != "a925db842c707850e4ca7b7e86b217692b0353a9ca05eb028802c4a85db93843" || body["webhook"] != "https://example.org/callback" {
			t.Errorf("incorrect Replicate body: %#v", body)
		}
		if _, ok := body["domains"]; ok {
			t.Error("flat domain input leaked into the body")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"prediction-123","status":"starting","output":null}`)
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	result, err := executeIntegrationTool(app, domainAppraisalTool(t, app, "appraise_domains"), map[string]string{"token": "test-token"}, map[string]any{"domains": "example.com,startup.net", "webhook": "https://example.org/callback"}, "")
	if err != nil || !result.Success || requests != 1 {
		t.Fatalf("prediction failed: %v %#v", err, result)
	}
	raw, _ := json.Marshal(result.Data)
	if !strings.Contains(string(raw), `"status":"starting"`) {
		t.Fatalf("lost prediction status: %s", raw)
	}
}

func TestDomainAppraisalRESTRequestsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		slug, tool, path, response string
		input                      map[string]any
		success                    bool
	}{
		{"bishopi", "appraise_domain", "/sales/valuation/", `{"confidence":0.7,"comparables":[{"domain":"sample.com","price":500}]}`, map[string]any{"domain": "example.com"}, true},
		{"domainindex", "appraise_domains", "/api.php", `{"example.com":{"price":500}}`, map[string]any{"domains": "example.com,startup.net"}, true},
		{"estibot", "appraise_domains", "/api", `{"success":true,"results":[],"cache":true,"not_found":["startup.net"]}`, map[string]any{"domains": "example.com>>startup.net", "mode": "live"}, true},
		{"estibot", "appraise_domains", "/api", `{"success":false,"message":"Invalid API key.","results":[]}`, map[string]any{"domains": "example.com>>startup.net", "mode": "live"}, false},
	} {
		t.Run(tc.slug+tc.response, func(t *testing.T) {
			app := domainAppraisalCatalog(t, tc.slug)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != tc.path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				q := r.URL.Query()
				switch tc.slug {
				case "bishopi":
					if r.Header.Get("Authorization") != "Api-Key test-key" || q.Get("domain") != "example.com" {
						t.Error("wrong Bishopi header or domain query")
					}
				case "domainindex":
					if q.Get("action") != "appraise" || q.Get("mode") != "json" || q.Get("key") != "test-key" || q.Get("domain") != "example.com,startup.net" {
						t.Errorf("wrong DomainIndex query: %v", q)
					}
				case "estibot":
					if q.Get("a") != "appraise" || q.Get("k") != "test-key" || q.Get("d") != "example.com>>startup.net" || q.Get("t") != "live" || len(q["t"]) != 1 {
						t.Errorf("wrong EstiBot query: %v", q)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			tool := domainAppraisalTool(t, app, tc.tool)
			tool.RateLimit = nil
			result, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "test-key"}, tc.input, "")
			if err != nil || result.Success != tc.success {
				t.Fatalf("incorrect response status: %v %#v", err, result)
			}
			if tc.success {
				var expected any
				json.Unmarshal([]byte(tc.response), &expected)
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(result.Data)
				if string(want) != string(got) {
					t.Fatalf("lost provider fields: %s != %s", got, want)
				}
			}
		})
	}
}

func TestDomainAppraisalAtomOAuth(t *testing.T) {
	app := domainAppraisalCatalog(t, "atom")
	s := newTestServer(t)
	if err := validateOAuthSetupClient(app, "registered-client", ""); err != nil {
		t.Fatalf("public clients must not require a secret: %v", err)
	}
	if err := validateOAuthSetupClient(app, "", ""); err == nil {
		t.Fatal("one-time registered client ID must be required")
	}
	authURL, err := url.Parse(s.localOAuthAuthorizeURL(app, "test-state", "test-challenge", "registered-client"))
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	if authURL.Scheme+"://"+authURL.Host+authURL.Path != "https://www.atom.com/oauth/authorize" || q.Get("client_id") != "registered-client" || q.Get("code_challenge") != "test-challenge" || q.Get("code_challenge_method") != "S256" || q.Get("state") != "test-state" || q.Get("resource") != "https://mcp.atom.com/mcp" || q.Get("scope") != "domains:read offline_access" {
		t.Fatalf("incorrect Atom authorization URL: %s", authURL)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Method != "POST" || r.Form.Get("client_id") != "registered-client" || r.Form.Get("code_verifier") != "test-verifier" || r.Form.Get("code") != "test-code" || r.Form.Has("client_secret") || r.Header.Get("Authorization") != "" {
			t.Errorf("incorrect public-client token exchange: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"test-token","refresh_token":"test-refresh","expires_in":3600}`)
	}))
	defer srv.Close()
	app.Auth.OAuth2.TokenURL = srv.URL
	tokens, err := s.exchangeOAuthCode(app, "test-code", "test-verifier", 1, "registered-client", "", nil)
	if err != nil || tokens["access_token"] != "test-token" || tokens["refresh_token"] != "test-refresh" {
		t.Fatalf("Atom token exchange failed: %v %v", err, tokens)
	}
}
