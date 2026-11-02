package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The embedded WordPress entry must resolve its {{site_url}} base_url and
// tool paths to real /wp-json/wp/v2 routes, authenticating with the
// connection's application password.
func TestWordPressCatalogResolvesRESTRoutes(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "1")
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/wordpress.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:abcd efgh ijkl"))
	for _, tc := range []struct {
		tool, method, uri, body string
		input                   map[string]any
	}{
		{"list_posts", "GET", "/wp-json/wp/v2/posts?per_page=5&status=draft", "", map[string]any{"per_page": 5, "status": "draft"}},
		{"get_page", "GET", "/wp-json/wp/v2/pages/7", "", map[string]any{"page_id": 7}},
		{"update_post", "POST", "/wp-json/wp/v2/posts/42", `{"title":"Hello"}`, map[string]any{"post_id": 42, "title": "Hello"}},
		{"delete_category", "DELETE", "/wp-json/wp/v2/categories/3?force=true", "", map[string]any{"category_id": 3, "force": true}},
		{"get_current_user", "GET", "/wp-json/wp/v2/users/me", "", map[string]any{}},
		{"search", "GET", "/wp-json/wp/v2/search?search=pricing", "", map[string]any{"search": "pricing"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.tool {
					tool = &app.Tools[i]
					break
				}
			}
			if tool == nil {
				t.Fatalf("missing tool %s", tc.tool)
			}
			seen := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				if r.Method != tc.method || r.URL.RequestURI() != tc.uri {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.RequestURI(), tc.method, tc.uri)
				}
				if got := r.Header.Get("Authorization"); got != wantAuth {
					t.Errorf("Authorization = %q, want %q", got, wantAuth)
				}
				if tc.body != "" {
					body, _ := io.ReadAll(r.Body)
					if string(body) != tc.body {
						t.Errorf("body = %s, want %s", body, tc.body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":1}`))
			}))
			defer srv.Close()
			creds := map[string]string{"site_url": srv.URL, "username": "admin", "application_password": "abcd efgh ijkl"}
			result, err := executeIntegrationTool(&app, tool, creds, tc.input, "")
			if err != nil {
				t.Fatal(err)
			}
			if !seen || !result.Success {
				t.Fatalf("request seen=%v, success=%v", seen, result.Success)
			}
		})
	}
}
