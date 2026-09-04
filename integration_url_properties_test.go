package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func testURLPropertyServer(t *testing.T) (*Server, *AppTemplate, map[string]string) {
	t.Helper()
	s := newTestServer(t)
	s.publicURL = "https://agents.example.test"
	s.catalog = NewAppCatalog()
	app := &AppTemplate{
		Slug: "video-provider", Name: "Video Provider",
		URLProperties: []IntegrationURLProperty{{ID: "delivery", Label: "Delivery", Types: []string{"url_prefix"}, VerificationMethods: []string{"file"}}},
		Tools:         []AppToolDef{{Name: "post", ExternalFetchInputs: []ExternalFetchInput{{Path: "source.urls[]", Property: "delivery", Relay: "required", TTLSeconds: 600, MaxBytes: 100, MIMETypes: []string{"video/mp4"}, When: &ExternalFetchCondition{Path: "source.mode", Equals: "PULL"}}}}},
	}
	s.catalog.Register(app)
	credentials := map[string]string{"client_id": "client-one"}
	fp := integrationOAuthFingerprint(credentials)
	state := integrationURLPropertyState{Type: "url_prefix", Value: s.publicURL + "/api/relay/", VerificationMethod: "file", VerificationFilename: "verify.txt", VerificationContent: "provider-verification", HostingStatus: "ready", RelayStatus: "ready", OperatorConfirmedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.writeURLPropertyState(app.Slug, "delivery", fp, state); err != nil {
		t.Fatal(err)
	}
	return s, app, credentials
}

func TestIntegrationURLPropertyFingerprintIsolation(t *testing.T) {
	if integrationOAuthFingerprint(map[string]string{"client_id": "a"}) == integrationOAuthFingerprint(map[string]string{"client_id": "b"}) {
		t.Fatal("different OAuth clients must not share URL-property state")
	}
	if got := integrationOAuthFingerprint(map[string]string{"client_id": "secret-client"}); strings.Contains(got, "secret-client") || len(got) != 24 {
		t.Fatalf("fingerprint leaks or has wrong shape: %q", got)
	}
}

func TestVerificationFileIsServedExactly(t *testing.T) {
	s, _, _ := testURLPropertyServer(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/relay/verify.txt", nil)
		w := httptest.NewRecorder()
		s.handleIntegrationRelay(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", method, w.Code, w.Body.String())
		}
		if method == http.MethodGet && w.Body.String() != "provider-verification" {
			t.Fatalf("verification body=%q", w.Body.String())
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
}

func TestURLPropertyConfigurationPersistsPerOAuthClient(t *testing.T) {
	s, app, _ := testURLPropertyServer(t)
	user, err := s.store.CreateUser("url-property@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(map[string]string{"client_id": "second-client"})
	encrypted, _ := Encrypt(s.secret, string(plain))
	conn, err := s.store.CreateConnection(user.ID, app.Slug, app.Name, "TikTok account", "oauth2", encrypted, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"verification_method":"file","verification_filename":"tiktok-check.txt","verification_content":"exact-signature"}`)
	req := httptest.NewRequest(http.MethodPut, "/integrations/catalog/"+app.Slug+"/url-properties/delivery?connection_id="+strconv.FormatInt(conn.ID, 10), bytes.NewReader(body))
	req.Header.Set("X-User-ID", strconv.FormatInt(user.ID, 10))
	w := httptest.NewRecorder()
	s.handleIntegrationURLProperties(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", w.Code, w.Body.String())
	}
	fp := integrationOAuthFingerprint(map[string]string{"client_id": "second-client"})
	state := s.readURLPropertyState(app.Slug, "delivery", fp)
	if state.VerificationFilename != "tiktok-check.txt" || state.VerificationContent != "exact-signature" || state.HostingStatus != "configured" {
		t.Fatalf("persisted state=%+v", state)
	}
	if other := s.readURLPropertyState(app.Slug, "delivery", integrationOAuthFingerprint(map[string]string{"client_id": "client-one"})); other.VerificationContent == "exact-signature" {
		t.Fatal("configuration leaked into another OAuth client")
	}
}

func TestCallbackIntegrationURLPropertyReportsReadiness(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "social", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}}}
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	plain, _ := json.Marshal(credentials)
	encrypted, _ := Encrypt(s.secret, string(plain))
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: app.Slug, AppName: app.Name, Name: "Provider account",
		ProjectID: "proj-1", CreatedVia: "integration", EncryptedCreds: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/callback/integrations/"+strconv.FormatInt(conn.ID, 10)+"/url-properties/delivery", nil)
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleAppCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Ready       bool   `json:"ready"`
		Integration string `json:"integration"`
		Property    string `json:"property"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.Ready || response.Integration != app.Slug || response.Property != "delivery" {
		t.Fatalf("callback response=%+v body=%s", response, w.Body.String())
	}
}

func TestPrepareIntegrationExternalFetchRewritesNestedURLs(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	exp := time.Now().Add(time.Hour).Unix()
	input := map[string]any{"source": map[string]any{"mode": "PULL", "urls": []string{
		"https://agents.example.test/api/apps/storage/files/7/content/movie.mp4?sig=abc&exp=" + strconv.FormatInt(exp, 10),
	}}}
	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err != nil {
		t.Fatal(err)
	}
	got := input["source"].(map[string]any)["urls"].([]string)[0]
	if !strings.HasPrefix(got, "https://agents.example.test/api/relay/") || !strings.HasSuffix(got, "/movie.mp4") {
		t.Fatalf("rewritten URL=%q", got)
	}
	token := strings.Split(strings.TrimPrefix(got, "https://agents.example.test/api/relay/"), "/")[0]
	plain, err := Decrypt(s.secret, token)
	if err != nil {
		t.Fatal(err)
	}
	var claims integrationRelayClaims
	if json.Unmarshal([]byte(plain), &claims) != nil || claims.SourceURL == "" || claims.Property != "delivery" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestPrepareIntegrationExternalFetchAcceptsCurrentStoragePublicURL(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	exp := time.Now().Add(time.Hour).Unix()
	source := "https://agents.example.test/api/apps/storage/public/files/7/content/photo.jpg?sig=abc&exp=" + strconv.FormatInt(exp, 10) + "&project_id=project-1"
	input := map[string]any{"source": map[string]any{"mode": "PULL", "urls": []string{source}}}

	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err != nil {
		t.Fatal(err)
	}
	got := input["source"].(map[string]any)["urls"].([]string)[0]
	if !strings.HasPrefix(got, "https://agents.example.test/api/relay/") || !strings.HasSuffix(got, "/photo.jpg") {
		t.Fatalf("rewritten URL=%q", got)
	}
	token := strings.Split(strings.TrimPrefix(got, "https://agents.example.test/api/relay/"), "/")[0]
	plain, err := Decrypt(s.secret, token)
	if err != nil {
		t.Fatal(err)
	}
	var claims integrationRelayClaims
	if json.Unmarshal([]byte(plain), &claims) != nil || claims.SourceURL != source {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestPrepareIntegrationExternalFetchAcceptsStorageProxyURL(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	exp := time.Now().Add(time.Hour).Unix()
	source := "https://agents.example.test/api/apps/storage/files/7/proxy/content/photo.jpg?sig=abc&exp=" + strconv.FormatInt(exp, 10) + "&project_id=project-1"
	input := map[string]any{"source": map[string]any{"mode": "PULL", "urls": []string{source}}}

	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err != nil {
		t.Fatal(err)
	}
	got := input["source"].(map[string]any)["urls"].([]string)[0]
	if !strings.HasPrefix(got, "https://agents.example.test/api/relay/") || !strings.HasSuffix(got, "/photo.jpg") {
		t.Fatalf("rewritten URL=%q", got)
	}
	token := strings.Split(strings.TrimPrefix(got, "https://agents.example.test/api/relay/"), "/")[0]
	plain, err := Decrypt(s.secret, token)
	if err != nil {
		t.Fatal(err)
	}
	var claims integrationRelayClaims
	if json.Unmarshal([]byte(plain), &claims) != nil || claims.SourceURL != source {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestValidateRelaySourceOnlyAcceptsStorageContentRoutes(t *testing.T) {
	s, _, _ := testURLPropertyServer(t)
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{name: "legacy content", path: "/api/apps/storage/files/7/content/photo.jpg", ok: true},
		{name: "current public content", path: "/api/apps/storage/public/files/7/content/photo.jpg", ok: true},
		{name: "content without filename", path: "/api/apps/storage/public/files/7/content", ok: true},
		{name: "redirect-free proxy content", path: "/api/apps/storage/files/7/proxy/content/photo.jpg", ok: true},
		{name: "redirect-free proxy content without filename", path: "/api/apps/storage/files/7/proxy/content", ok: true},
		{name: "metadata", path: "/api/apps/storage/public/files/7", ok: false},
		{name: "download", path: "/api/apps/storage/public/files/7/download/photo.jpg", ok: false},
		{name: "proxy metadata", path: "/api/apps/storage/files/7/proxy", ok: false},
		{name: "proxy download", path: "/api/apps/storage/files/7/proxy/download/photo.jpg", ok: false},
		{name: "proxy content lookalike", path: "/api/apps/storage/files/7/proxy/content-evil/photo.jpg", ok: false},
		{name: "lookalike prefix", path: "/api/apps/storage/public/files-evil/7/content/photo.jpg", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validateRelaySource("https://agents.example.test" + tc.path + "?sig=abc&exp=" + exp)
			if tc.ok && err != nil {
				t.Fatalf("valid source rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestPrepareIntegrationExternalFetchRequiresReadyStateAndSignedStorage(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	fp := integrationOAuthFingerprint(credentials)
	state := s.readURLPropertyState(app.Slug, "delivery", fp)
	state.OperatorConfirmedAt = ""
	_ = s.writeURLPropertyState(app.Slug, "delivery", fp, state)
	input := map[string]any{"source": map[string]any{"mode": "PULL", "urls": []any{"https://agents.example.test/api/apps/storage/files/1/content/a.mp4?sig=x&exp=1999999999"}}}
	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("expected setup error, got %v", err)
	}
	state.OperatorConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	_ = s.writeURLPropertyState(app.Slug, "delivery", fp, state)
	input = map[string]any{"source": map[string]any{"mode": "PULL", "urls": []any{"https://evil.example/a.mp4"}}}
	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err == nil || !strings.Contains(err.Error(), "this Apteva instance") {
		t.Fatalf("expected source isolation error, got %v", err)
	}
}

func TestPrepareIntegrationExternalFetchConditionKeepsFileUploadUnchanged(t *testing.T) {
	s, app, credentials := testURLPropertyServer(t)
	fp := integrationOAuthFingerprint(credentials)
	_ = s.store.SetSetting(integrationURLPropertyKey(app.Slug, "delivery", fp), "")
	input := map[string]any{"source": map[string]any{"mode": "FILE", "urls": []any{"not-a-url"}}}
	if err := s.prepareIntegrationExternalFetch(app, &app.Tools[0], credentials, input); err != nil {
		t.Fatalf("non-pull mode must remain backward compatible: %v", err)
	}
	if got := input["source"].(map[string]any)["urls"].([]any)[0]; got != "not-a-url" {
		t.Fatalf("conditional input was unexpectedly rewritten: %v", got)
	}
}

func TestRelayTokenRejectsTamperingAndExpiry(t *testing.T) {
	s, _, credentials := testURLPropertyServer(t)
	claims := integrationRelayClaims{Version: 1, Integration: "video-provider", Property: "delivery", Fingerprint: integrationOAuthFingerprint(credentials), Filename: "a.mp4", SourceURL: "https://agents.example.test/api/apps/storage/files/1/content/a.mp4?sig=x&exp=1999999999", ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	b, _ := json.Marshal(claims)
	token, _ := Encrypt(s.secret, string(b))
	for _, candidate := range []string{token, token[:len(token)-1] + "0"} {
		w := httptest.NewRecorder()
		s.handleIntegrationRelay(w, httptest.NewRequest(http.MethodGet, "/relay/"+candidate+"/a.mp4", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("expired/tampered token status=%d", w.Code)
		}
	}
}

func TestIntegrationRelayFollowsUpstreamRedirectWithoutExposingIt(t *testing.T) {
	var sawRange string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/apps/storage/files/"):
			http.Redirect(w, r, "/object", http.StatusFound)
		case r.URL.Path == "/object":
			sawRange = r.Header.Get("Range")
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "5")
				return
			}
			if sawRange != "" {
				w.Header().Set("Content-Range", "bytes 0-4/5")
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = io.WriteString(w, "video")
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	s, app, credentials := testURLPropertyServer(t)
	s.publicURL = backend.URL
	s.integrationRelayTransport = backend.Client().Transport
	fp := integrationOAuthFingerprint(credentials)
	state := s.readURLPropertyState(app.Slug, "delivery", fp)
	state.Value = backend.URL + "/api/relay/"
	_ = s.writeURLPropertyState(app.Slug, "delivery", fp, state)
	claims := integrationRelayClaims{
		Version: 1, Integration: app.Slug, Property: "delivery", Fingerprint: fp,
		Filename: "movie.mp4", ExpiresAt: time.Now().Add(time.Hour).Unix(), MaxBytes: 100,
		MIMETypes: []string{"video/mp4"},
		SourceURL: backend.URL + "/api/apps/storage/files/7/content/movie.mp4?sig=x&exp=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
	}
	b, _ := json.Marshal(claims)
	token, _ := Encrypt(s.secret, string(b))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/relay/"+token+"/movie.mp4", nil)
	req.Header.Set("Range", "bytes=0-4")
	s.handleIntegrationRelay(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "video" {
		t.Fatalf("relay status=%d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	if w.Header().Get("Location") != "" {
		t.Fatalf("relay exposed upstream redirect: %q", w.Header().Get("Location"))
	}
	if sawRange != "bytes=0-4" || w.Header().Get("Content-Range") != "bytes 0-4/5" {
		t.Fatalf("range was not preserved: upstream=%q response=%q", sawRange, w.Header().Get("Content-Range"))
	}
	state = s.readURLPropertyState(app.Slug, "delivery", fp)
	if state.LastSuccessfulProviderPullAt == "" {
		t.Fatal("successful provider pull was not recorded")
	}

	w = httptest.NewRecorder()
	s.handleIntegrationRelay(w, httptest.NewRequest(http.MethodHead, "/relay/"+token+"/movie.mp4", nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestValidateExternalRelayURLRejectsPrivateTargets(t *testing.T) {
	for _, target := range []string{"http://example.com/file", "https://127.0.0.1/file", "https://localhost/file"} {
		if err := validateExternalRelayURL(context.Background(), target); err == nil {
			t.Fatalf("expected %s to be rejected", target)
		}
	}
}
