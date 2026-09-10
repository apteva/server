package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func seedDashboardConnectInstall(t *testing.T, s *Server, name string) int64 {
	t.Helper()
	id := seedCORSAppInstall(t, s, name, "")
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, `["platform.dashboard.connect"]`, id); err != nil {
		t.Fatal(err)
	}
	return id
}
func callDashboardConnect(t *testing.T, s *Server, id int64, method, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := s.appInstallToken(id)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/apps/callback/dashboard-connect-origins/"+key, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(rec, req)
	return rec
}
func TestDashboardConnectRegistrationLifecycle(t *testing.T) {
	s := newTestServer(t)
	a := seedDashboardConnectInstall(t, s, "csp-a")
	b := seedDashboardConnectInstall(t, s, "csp-b")
	put := func(id int64, body string) {
		t.Helper()
		r := callDashboardConnect(t, s, id, "PUT", "backend", body)
		if r.Code != 200 {
			t.Fatalf("put %d %s", r.Code, r.Body)
		}
	}
	put(a, `{"origins":["https://BUCKET.example:443/","https://bucket.example"]}`)
	put(a, `{"origins":["https://bucket.example"]}`)
	rows, err := s.listDashboardConnectOrigins(a, "")
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Origins, []string{"https://bucket.example"}) {
		t.Fatalf("rows=%+v %v", rows, err)
	}
	if r := callDashboardConnect(t, s, b, "GET", "backend", ""); r.Code != 404 {
		t.Fatalf("cross-install read %d", r.Code)
	}
	put(b, `{"origins":["https://bucket.example"]}`)
	if r := callDashboardConnect(t, s, a, "DELETE", "backend", ""); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if origins, _ := s.dashboardConnectOrigins(); len(origins) != 1 {
		t.Fatal("other install's grant was removed")
	}
	put(b, `{"origins":["https://new.example"]}`)
	origins, _ := s.dashboardConnectOrigins()
	if !reflect.DeepEqual(origins, []string{"https://new.example"}) {
		t.Fatal(origins)
	}
	// CSP registration never grants inbound CORS on this app or the platform.
	if s.dynamicAppCORSOriginAllowed(httptest.NewRequest("GET", "/apps/csp-b/test", nil), "https://new.example") {
		t.Fatal("CSP granted CORS")
	}
	put(b, `{"origins":[]}`)
	if origins, _ := s.dashboardConnectOrigins(); len(origins) != 0 {
		t.Fatal(origins)
	}
	put(b, `{"origins":["https://new.example"]}`)
	if _, err := s.store.db.Exec(`DELETE FROM app_installs WHERE id=?`, b); err != nil {
		t.Fatal(err)
	}
	var count int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_dashboard_connect_origins`).Scan(&count)
	if count != 0 {
		t.Fatalf("uninstall left %d rows", count)
	}
}
func TestDashboardConnectAuthorizationAndRevocation(t *testing.T) {
	s := newTestServer(t)
	id := seedDashboardConnectInstall(t, s, "csp-auth")
	req := httptest.NewRequest("PUT", "/apps/callback/dashboard-connect-origins/backend", strings.NewReader(`{"origins":["https://bucket.example"]}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa64(id))
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("forged install accepted %d", rec.Code)
	}
	put := func() *httptest.ResponseRecorder {
		return callDashboardConnect(t, s, id, "PUT", "backend", `{"origins":["https://bucket.example"]}`)
	}
	if r := put(); r.Code != 200 {
		t.Fatalf("authorized %d %s", r.Code, r.Body)
	}
	for _, update := range []string{
		`UPDATE app_installs SET permissions_json='[]'`,
		`UPDATE app_installs SET status='disabled'`,
		`UPDATE users SET role='user'`,
	} {
		if _, err := s.store.db.Exec(update); err != nil {
			t.Fatal(err)
		}
		if origins, _ := s.dashboardConnectOrigins(); len(origins) != 0 {
			t.Fatalf("revoked grant remains: %v", origins)
		}
		if r := put(); r.Code < 400 {
			t.Fatalf("revoked registration accepted: %d", r.Code)
		}
		s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.dashboard.connect"]',status='running'`)
		s.store.db.Exec(`UPDATE users SET role='admin'`)
	}
	// Manifest declaration alone does not grant the permission.
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]',manifest_json='{"requires":{"permissions":["platform.dashboard.connect"]}}'`)
	if r := put(); r.Code != 403 {
		t.Fatalf("manifest-only permission accepted %d", r.Code)
	}
}
func TestDashboardConnectExactOrigins(t *testing.T) {
	for _, origin := range []string{"http://bucket.example", "https://*.example", "https://a.example/path", "https://a.example?", "https://a.example#", "https://u:p@a.example", "https://a.example;script-src", "https://a.example\"", "https://a.example:0", "https://a.example:65536", "https://a.example:", "https://[::1%25en0]", "https://bad_host", "https://[example.com]", "https://", "data:text/plain,hello"} {
		t.Run(origin, func(t *testing.T) {
			if _, err := normalizeDashboardConnectOrigins([]string{origin}); err == nil {
				t.Fatal("accepted invalid origin")
			}
		})
	}
	out, err := normalizeDashboardConnectOrigins([]string{"https://BUCKET.example:443/", "https://bucket.example", "https://[::1]:8443"})
	if err != nil || !reflect.DeepEqual(out, []string{"https://[::1]:8443", "https://bucket.example"}) {
		t.Fatalf("%v %v", out, err)
	}
	s := newTestServer(t)
	id := seedDashboardConnectInstall(t, s, "csp-validate")
	if r := callDashboardConnect(t, s, id, "PUT", "backend", `{"origins":["https://bucket.example"],"script-src":"*"}`); r.Code != 400 {
		t.Fatalf("arbitrary directive accepted %d", r.Code)
	}
}

// Extract the policy and all remaining raw bytes to verify script hashes and
// every other directive survive production index transformation unchanged.
func splitDashboardPolicy(t *testing.T, document []byte) (map[string]string, []byte) {
	t.Helper()
	z := html.NewTokenizer(bytes.NewReader(document))
	var rest bytes.Buffer
	policy := map[string]string{}
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				t.Fatal(z.Err())
			}
			break
		}
		raw := append([]byte{}, z.Raw()...)
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			token := z.Token()
			isCSP := false
			content := ""
			for _, a := range token.Attr {
				if a.Key == "http-equiv" && a.Val == "Content-Security-Policy" {
					isCSP = true
				}
				if a.Key == "content" {
					content = a.Val
				}
			}
			if token.Data == "meta" && isCSP {
				for _, d := range strings.Split(content, ";") {
					f := strings.Fields(d)
					if len(f) > 0 {
						policy[f[0]] = strings.Join(f[1:], " ")
					}
				}
				continue
			}
		}
		rest.Write(raw)
	}
	return policy, rest.Bytes()
}
func TestDashboardConnectProductionCSPAndCache(t *testing.T) {
	document, err := fs.ReadFile(dashboardFS, "dashboard/index.html")
	if err != nil {
		t.Fatal(err)
	}
	original, raw := splitDashboardPolicy(t, document)
	if original["connect-src"] == "" {
		t.Fatal("production CSP missing")
	}
	for _, source := range []string{"embedded", "disk"} {
		t.Run(source, func(t *testing.T) {
			s := newTestServer(t)
			id := seedDashboardConnectInstall(t, s, "csp-page")
			var handler http.Handler
			if source == "embedded" {
				handler = s.dashboardHandler()
			} else {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "index.html"), document, 0600)
				os.WriteFile(filepath.Join(dir, "asset-12345678.js"), []byte("hello"), 0600)
				handler = s.dashboardSPAHandler(os.DirFS(dir))
			}
			render := func(path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("If-Modified-Since", "Wed, 31 Dec 2099 23:59:59 GMT")
				r.Header.Set("If-None-Match", "*")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("stale HTML possible %d %v", w.Code, w.Header())
				}
				return w
			}
			before := render("/")
			if r := callDashboardConnect(t, s, id, "PUT", "backend", `{"origins":["https://bucket.example"]}`); r.Code != 200 {
				t.Fatalf("registration %d %s", r.Code, r.Body)
			}
			for _, path := range []string{"/", "/index.html", "/agents/1103"} {
				w := render(path)
				policy, rest := splitDashboardPolicy(t, w.Body.Bytes())
				if !strings.Contains(policy["connect-src"], "https://bucket.example") {
					t.Fatal("destination not allowed")
				}
				delete(policy, "connect-src")
				want := map[string]string{}
				for k, v := range original {
					if k != "connect-src" {
						want[k] = v
					}
				}
				if !reflect.DeepEqual(policy, want) || !bytes.Equal(rest, raw) {
					t.Fatal("unrelated policy or HTML changed")
				}
			}
			callDashboardConnect(t, s, id, "DELETE", "backend", "")
			if after := render("/"); !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
				t.Fatal("registration removal failed")
			}
			if source == "disk" {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", "/asset-12345678.js", nil))
				if w.Body.String() != "hello" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
					t.Fatal("asset serving changed")
				}
			}
		})
	}
}
func TestDashboardConnectRegistryPersists(t *testing.T) {
	s := newTestServer(t)
	id := seedDashboardConnectInstall(t, s, "csp-durable")
	r := callDashboardConnect(t, s, id, "PUT", "backend", `{"origins":["https://bucket.example"]}`)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	// Re-running the additive migration must preserve registrations.
	var dbPath string
	s.store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath)
	reopened, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	copy := &Server{store: reopened}
	rows, err := copy.listDashboardConnectOrigins(id, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	encoded, _ := json.Marshal(rows[0])
	if !strings.Contains(string(encoded), "https://bucket.example") {
		t.Fatal(string(encoded))
	}
}

func TestDashboardConnectInvalidReplacementIsAtomicAndCORSIsSeparate(t *testing.T) {
	s := newTestServer(t)
	id := seedDashboardConnectInstall(t, s, "csp-atomic")
	if r := callCORSRegistration(t, s, id, "PUT", "inbound", `{"origins":["https://inbound.example"]}`); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if origins, _ := s.dashboardConnectOrigins(); len(origins) != 0 {
		t.Fatal("CORS registration extended CSP")
	}
	if r := callDashboardConnect(t, s, id, "PUT", "backend", `{"origins":["https://bucket.example"]}`); r.Code != 200 {
		t.Fatal(r.Body)
	}
	for _, body := range []string{`{"origins":["https://good.example","https://*.example"]}`, `{"origins":[]} {"origins":["https://other.example"]}`} {
		if r := callDashboardConnect(t, s, id, "PUT", "backend", body); r.Code != 400 {
			t.Fatalf("invalid payload %d", r.Code)
		}
		if origins, _ := s.dashboardConnectOrigins(); !reflect.DeepEqual(origins, []string{"https://bucket.example"}) {
			t.Fatalf("failed replace changed registry %v", origins)
		}
	}
	if r := serveDynamicPreflight(s, "/apps/csp-atomic/test", "https://inbound.example"); r.Header().Get("Access-Control-Allow-Origin") != "https://inbound.example" {
		t.Fatal("CSP changed existing CORS")
	}
}

func TestDashboardConnectEveryMetaAndHead(t *testing.T) {
	doc := []byte(`<meta http-equiv="Content-Security-Policy" content="default-src 'self'; connect-src 'self'"><meta http-equiv="Content-Security-Policy" content="connect-src 'none'; object-src 'none'">`)
	out, err := dashboardHTMLWithOrigins(doc, []string{"https://bucket.example"})
	if err != nil || strings.Count(string(out), "https://bucket.example") != 2 {
		t.Fatalf("policies=%s %v", out, err)
	}
	if _, err := dashboardHTMLWithOrigins([]byte("<html></html>"), []string{"https://bucket.example"}); err == nil {
		t.Fatal("missing policy silently accepted")
	}
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.dashboardHandler().ServeHTTP(w, httptest.NewRequest("HEAD", "/agents", nil))
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("HEAD %d %v", w.Code, w.Header())
	}
}
