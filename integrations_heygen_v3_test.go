package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func heygenV3Catalog(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/heygen.json")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../integrations/src/apps/heygen.json")
	if err != nil || !bytes.Equal(raw, source) {
		t.Fatalf("HeyGen embedded catalog differs from source: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	for _, tool := range app.Tools {
		if !strings.HasPrefix(tool.Path, "/v3/") {
			t.Fatalf("legacy tool %s: %s", tool.Name, tool.Path)
		}
	}
	return &app
}

func TestHeyGenV3Requests(t *testing.T) {
	scene := map[string]any{"type": "avatar_video", "input": map[string]any{"type": "avatar", "avatar_id": "avatar", "script": "Hello"}}
	srt := map[string]any{"type": "url", "url": "https://example.com/corrected.srt"}
	for _, tc := range []struct {
		name, method, path string
		input, body        map[string]any
		idempotency        string
	}{
		{"generate_studio_video", "POST", "/v3/videos", map[string]any{"title": "Report", "scenes": []any{scene}, "idempotency_key": "studio:1"}, map[string]any{"type": "studio", "title": "Report", "scenes": []any{scene}}, "studio:1"},
		{"create_webm_video", "POST", "/v3/videos", map[string]any{"avatar_id": "avatar", "script": "Hello", "background": "ignored"}, map[string]any{"type": "avatar", "output_format": "webm", "avatar_id": "avatar", "script": "Hello"}, ""},
		{"upload_proofread_srt", "PUT", "/v3/video-translations/proofreads/proof/srt", map[string]any{"proofread_id": "proof", "srt": srt}, map[string]any{"srt": srt}, ""},
		{"create_digital_twin", "POST", "/v3/avatars", map[string]any{"name": "Presenter", "file": map[string]any{"type": "url", "url": "https://example.com/footage.mp4"}}, map[string]any{"type": "digital_twin", "name": "Presenter", "file": map[string]any{"type": "url", "url": "https://example.com/footage.mp4"}}, ""},
		{"create_avatar_consent", "POST", "/v3/avatars/group/consent", map[string]any{"group_id": "group", "consent_video": srt}, map[string]any{"consent_video": srt}, ""},
		{"update_webhook_endpoint", "PATCH", "/v3/webhooks/endpoints/endpoint", map[string]any{"endpoint_id": "endpoint", "events": []any{"avatar_video.success"}}, map[string]any{"events": []any{"avatar_video.success"}}, ""},
		{"delete_webhook_endpoint", "DELETE", "/v3/webhooks/endpoints/endpoint", map[string]any{"endpoint_id": "endpoint"}, nil, ""},
		{"get_current_user", "GET", "/v3/users/me", map[string]any{}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := heygenV3Catalog(t)
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatal("missing tool")
			}
			response := `{"data":{"status":"pending","video_page_url":"https://app.heygen.com/video"},"has_more":true,"next_token":"opaque +/="}`
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != "" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("X-Api-Key") != "test-heygen-key" || r.Header.Get("Idempotency-Key") != tc.idempotency {
					t.Error("incorrect auth/idempotency headers")
				}
				raw, _ := io.ReadAll(r.Body)
				if tc.body == nil {
					if len(raw) != 0 {
						t.Errorf("expected empty body: %s", raw)
					}
				} else {
					var got, want any
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Error(err)
					}
					expected, _ := json.Marshal(tc.body)
					_ = json.Unmarshal(expected, &want)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("wrong JSON: %s != %s", raw, expected)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, response)
			}))
			defer provider.Close()
			app.BaseURL = provider.URL
			result, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "test-heygen-key"}, tc.input, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %v %#v", err, result)
			}
			var want any
			_ = json.Unmarshal([]byte(response), &want)
			gotJSON, _ := json.Marshal(result.Data)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("native response lost: %s", gotJSON)
			}
		})
	}
}

func TestHeyGenV3MultipartAsset(t *testing.T) {
	app := heygenV3Catalog(t)
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "upload_asset" {
			tool = &app.Tools[i]
		}
	}
	if tool == nil {
		t.Fatal("missing upload tool")
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v3/assets" || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
			t.Errorf("wrong multipart request: %s %s %s", r.Method, r.URL, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Idempotency-Key") != "asset:1" {
			t.Error("lost idempotency header")
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.Value) != 0 || len(r.MultipartForm.File) != 1 {
			t.Errorf("unexpected multipart fields: %#v", r.MultipartForm)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer file.Close()
		raw, _ := io.ReadAll(file)
		if !bytes.Equal(raw, []byte{0, 255, 128, 13, 10}) || header.Header.Get("Content-Type") != "image/png" {
			t.Errorf("wrong file content/type: %v %#v", raw, header.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"asset_id":"asset"}}`)
	}))
	defer provider.Close()
	app.BaseURL = provider.URL
	result, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "test-heygen-key"}, map[string]any{"file": "data:image/png;base64,AP+ADQo=", "idempotency_key": "asset:1"}, "")
	if err != nil || !result.Success {
		t.Fatalf("upload failed: %v %#v", err, result)
	}
}

func TestSubscriptionAutoRegisterProviderSecret(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		valid          bool
	}{
		{"valid", `{"data":{"endpoint_id":"ep_test","secret":"whsec_provider-secret"}}`, true},
		{"missing-secret", `{"data":{"endpoint_id":"ep_test"}}`, false},
		{"non-string-secret", `{"data":{"endpoint_id":"ep_test","secret":123}}`, false},
		{"missing-id", `{"data":{"secret":"whsec_provider-secret"}}`, false},
		{"invalid-json", `invalid`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registeredURL string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v3/webhooks/endpoints" || r.Header.Get("X-Api-Key") != "test-key" {
					t.Errorf("wrong registration: %s %s", r.Method, r.URL)
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				registeredURL, _ = body["url"].(string)
				if len(body) != 2 || !reflect.DeepEqual(body["events"], []any{"avatar_video.success"}) {
					t.Errorf("wrong registration payload: %#v", body)
				}
				fmt.Fprint(w, tc.response)
			}))
			defer provider.Close()
			s := newTestServer(t)
			s.secret = testSecret()
			s.publicURL = "https://agents.test.com"
			s.catalog = NewAppCatalog()
			app := heygenV3Catalog(t)
			app.BaseURL = provider.URL
			s.catalog.Register(app)
			postJSON(t, s.handleRegister, map[string]string{"email": "heygen@test.com", "password": "password123"})
			login := postJSON(t, s.handleLogin, map[string]string{"email": "heygen@test.com", "password": "password123"})
			cookie := getSessionCookie(login)
			encrypted, err := Encrypt(s.secret, `{"api_key":"test-key"}`)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := s.store.CreateConnection(1, "heygen", "HeyGen", "My HeyGen", "api_key", encrypted, "")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"name": "Videos", "slug": "heygen", "connection_id": conn.ID, "events": []string{"avatar_video.success"}, "hmac_secret": "local-secret"})
			req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
			req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.authMiddleware(s.handleCreateSubscription)(rec, req)
			if rec.Code != 200 {
				t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
			}
			var created struct {
				Subscription   Subscription `json:"subscription"`
				WebhookURL     string       `json:"webhook_url"`
				AutoRegistered bool         `json:"auto_registered"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.AutoRegistered != tc.valid {
				t.Fatalf("auto_registered=%v want %v", created.AutoRegistered, tc.valid)
			}
			if registeredURL != created.WebhookURL {
				t.Fatalf("wrong registered URL: %s", registeredURL)
			}
			_, stored, err := s.store.GetSubscriptionByPath(created.Subscription.WebhookPath)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := Decrypt(s.secret, stored)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.valid {
				if plain != "local-secret" || s.store.GetSubscriptionExternalID(created.Subscription.ID) != "" {
					t.Fatal("invalid provider response changed stored credentials")
				}
				return
			}
			if plain != "whsec_provider-secret" || stored == plain || s.store.GetSubscriptionExternalID(created.Subscription.ID) != "ep_test" {
				t.Fatal("provider credentials were not stored encrypted")
			}
			payload := []byte("{ \"event_type\": \"avatar_video.success\" }\n")
			mac := hmac.New(sha256.New, []byte(plain))
			mac.Write(payload)
			signature := hex.EncodeToString(mac.Sum(nil))
			for _, check := range []struct {
				name, header, sig string
				payload           []byte
				status            int
			}{
				{"valid", "signature", signature, payload, 400},
				{"case-insensitive", "Signature", signature, payload, 400},
				{"missing", "signature", "", payload, 401},
				{"wrong", "signature", "bad", payload, 401},
				{"fallback-rejected", "x-webhook-signature", signature, payload, 401},
				{"tampered", "signature", signature, append(append([]byte{}, payload...), byte(' ')), 401},
			} {
				t.Run(check.name, func(t *testing.T) {
					request := httptest.NewRequest("POST", "/webhooks/"+created.Subscription.WebhookPath, bytes.NewReader(check.payload))
					request.Header.Set(check.header, check.sig)
					result := httptest.NewRecorder()
					s.handleWebhook(result, request)
					if result.Code != check.status {
						t.Fatalf("ingress status=%d want %d: %s", result.Code, check.status, result.Body.String())
					}
					if check.status == 400 && !strings.Contains(result.Body.String(), "no instance configured") {
						t.Fatalf("signature did not pass verification: %s", result.Body.String())
					}
				})
			}
		})
	}
}
