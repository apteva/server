package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConnectionReauthDispatchPreservesBrowserOAuthRoutes(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = NewAppCatalog()
	app := &AppTemplate{
		Slug: "browser-oauth-test",
		Name: "Browser OAuth Test",
		Auth: AppAuthConfig{
			Types: []string{"oauth2"},
			OAuth2: &OAuthConfig{
				AuthorizeURL: "https://accounts.example.test/authorize",
			},
		},
	}
	s.catalog.Register(app)
	encrypted, err := Encrypt(s.secret, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: app.Slug, AppName: app.Name, Name: app.Name,
		AuthType: "oauth2", EncryptedCreds: encrypted, Status: "active", Source: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"/reauth", "/oauth/reauth"} {
		t.Run(strings.TrimPrefix(suffix, "/"), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/connections/"+strconv.FormatInt(conn.ID, 10)+suffix, nil)
			req.Header.Set("X-User-ID", "1")
			rec := httptest.NewRecorder()
			s.handleReauthConnection(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var out struct {
				Connection  Connection `json:"connection"`
				RedirectURL string     `json:"redirect_url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(out.RedirectURL)
			if err != nil {
				t.Fatal(err)
			}
			if out.Connection.ID != conn.ID || parsed.Host != "accounts.example.test" || parsed.Query().Get("state") == "" {
				t.Fatalf("unexpected response: %+v", out)
			}
			got, _, err := s.store.GetConnection(1, conn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "active" {
				t.Fatalf("browser OAuth reauth changed status to %q", got.Status)
			}
		})
	}
}

func TestDeviceCodeReauthPreservesExistingConnectionOnExpiry(t *testing.T) {
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usercode" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"user_code": "ABCD-EFGH", "device_auth_id": "device-1", "expires_in": 900, "interval": 5,
		})
	}))
	defer deviceServer.Close()
	oldUserCodeURL := integrationOpenAICodexDeviceUserCodeURL
	integrationOpenAICodexDeviceUserCodeURL = deviceServer.URL + "/usercode"
	t.Cleanup(func() { integrationOpenAICodexDeviceUserCodeURL = oldUserCodeURL })
	globalConnectionDeviceAuthSessions = &connectionDeviceAuthSessionStore{sessions: map[string]*connectionDeviceAuthSession{}}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = NewAppCatalog()
	app := &AppTemplate{
		Slug: integrationOpenAICodexSlug,
		Name: "OpenAI Codex",
		Auth: AppAuthConfig{Types: []string{connectionAuthTypeDeviceCode}},
	}
	s.catalog.Register(app)
	originalCredentials := `{"access_token":"old-token","refresh_token":"old-refresh"}`
	encrypted, err := Encrypt(s.secret, originalCredentials)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: app.Slug, AppName: app.Name, Name: app.Name,
		AuthType: connectionAuthTypeDeviceCode, EncryptedCreds: encrypted, Status: "active", Source: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/connections/"+strconv.FormatInt(conn.ID, 10)+"/reauth", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleReauthConnection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Connection Connection `json:"connection"`
		DeviceAuth struct {
			SessionID string `json:"session_id"`
			UserCode  string `json:"user_code"`
		} `json:"device_auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Connection.ID != conn.ID || out.DeviceAuth.SessionID == "" || out.DeviceAuth.UserCode != "ABCD-EFGH" {
		t.Fatalf("unexpected response: %+v", out)
	}
	session, ok := globalConnectionDeviceAuthSessions.get(out.DeviceAuth.SessionID)
	if !ok || !session.Reauth || session.ConnectionID != conn.ID {
		t.Fatalf("unexpected session: %+v ok=%v", session, ok)
	}
	session.ExpiresAt = time.Now().Add(-time.Second)

	pollReq := httptest.NewRequest(http.MethodGet, "/connections/auth/"+out.DeviceAuth.SessionID, nil)
	pollReq.Header.Set("X-User-ID", "1")
	pollRec := httptest.NewRecorder()
	s.handlePollConnectionDeviceAuth(pollRec, pollReq)
	if pollRec.Code != http.StatusOK || !strings.Contains(pollRec.Body.String(), `"status":"expired"`) {
		t.Fatalf("poll status=%d body=%s", pollRec.Code, pollRec.Body.String())
	}
	got, gotEncrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || gotEncrypted != encrypted {
		t.Fatalf("reauth expiry mutated connection status=%q credentials_changed=%v", got.Status, gotEncrypted != encrypted)
	}
}

func TestDeviceCodeReauthAtomicallyUpdatesExistingConnection(t *testing.T) {
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usercode":
			writeJSON(w, map[string]any{
				"user_code": "WXYZ-1234", "device_auth_id": "device-2", "expires_in": 900, "interval": 1,
			})
		case "/poll":
			writeJSON(w, map[string]any{"authorization_code": "auth-code", "code_verifier": "verifier"})
		case "/token":
			writeJSON(w, map[string]any{
				"access_token": "new-token", "refresh_token": "new-refresh", "account_id": "acct-new",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer deviceServer.Close()
	oldUserCodeURL := integrationOpenAICodexDeviceUserCodeURL
	oldDeviceTokenURL := integrationOpenAICodexDeviceTokenURL
	oldTokenURL := integrationOpenAICodexTokenURL
	integrationOpenAICodexDeviceUserCodeURL = deviceServer.URL + "/usercode"
	integrationOpenAICodexDeviceTokenURL = deviceServer.URL + "/poll"
	integrationOpenAICodexTokenURL = deviceServer.URL + "/token"
	t.Cleanup(func() {
		integrationOpenAICodexDeviceUserCodeURL = oldUserCodeURL
		integrationOpenAICodexDeviceTokenURL = oldDeviceTokenURL
		integrationOpenAICodexTokenURL = oldTokenURL
	})
	globalConnectionDeviceAuthSessions = &connectionDeviceAuthSessionStore{sessions: map[string]*connectionDeviceAuthSession{}}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = NewAppCatalog()
	app := &AppTemplate{
		Slug: integrationOpenAICodexSlug,
		Name: "OpenAI Codex",
		Auth: AppAuthConfig{Types: []string{connectionAuthTypeDeviceCode}},
	}
	s.catalog.Register(app)
	encrypted, err := Encrypt(s.secret, `{"access_token":"old-token","refresh_token":"old-refresh"}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: app.Slug, AppName: app.Name, Name: app.Name,
		AuthType: connectionAuthTypeDeviceCode, EncryptedCreds: encrypted, Status: "active", Source: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	startReq := httptest.NewRequest(http.MethodPost, "/connections/"+strconv.FormatInt(conn.ID, 10)+"/reauth", nil)
	startReq.Header.Set("X-User-ID", "1")
	startRec := httptest.NewRecorder()
	s.handleReauthConnection(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startRec.Code, startRec.Body.String())
	}
	var started struct {
		DeviceAuth struct {
			SessionID string `json:"session_id"`
		} `json:"device_auth"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	pollReq := httptest.NewRequest(http.MethodGet, "/connections/auth/"+started.DeviceAuth.SessionID, nil)
	pollReq.Header.Set("X-User-ID", "1")
	pollRec := httptest.NewRecorder()
	s.handlePollConnectionDeviceAuth(pollRec, pollReq)
	if pollRec.Code != http.StatusOK || !strings.Contains(pollRec.Body.String(), `"status":"connected"`) {
		t.Fatalf("poll status=%d body=%s", pollRec.Code, pollRec.Body.String())
	}
	got, gotEncrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, gotEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]string
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		t.Fatal(err)
	}
	if got.ID != conn.ID || got.Status != "active" || credentials["access_token"] != "new-token" || credentials["refresh_token"] != "new-refresh" || credentials["account_id"] != "acct-new" {
		t.Fatalf("connection=%+v credential_keys=%v", got, filterKeys(credentials))
	}
}
