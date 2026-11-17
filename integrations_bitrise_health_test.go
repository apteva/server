package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestBitriseWorkspaceTokenHealthCheck(t *testing.T) {
	source, err := os.ReadFile("../integrations/src/apps/bitrise.json")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := integrationsCatalogFS.ReadFile("integrations-catalog/bitrise.json")
	if err != nil || !bytes.Equal(source, embedded) {
		t.Fatalf("Bitrise embedded catalog differs from source: %v", err)
	}
	for _, tc := range []struct {
		name, token, body string
		status            int
		ok                bool
	}{
		{"workspace", "bitwat_test-workspace-token", `{"data":[{"slug":"app-123"}]}`, 200, true},
		{"personal_without_apps", "test-personal-token", `{"data":[]}`, 200, true},
		{"invalid", "test-invalid-token", `{"message":"Unauthorized"}`, 401, false},
		{"forbidden", "test-forbidden-token", `{"message":"Forbidden"}`, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app AppTemplate
			if err := json.Unmarshal(source, &app); err != nil {
				t.Fatal(err)
			}
			requests := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/me" {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"message":"Not Found"}`)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/apps" || r.URL.Query().Get("limit") != "1" {
					t.Errorf("unexpected health request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != tc.token {
					t.Error("expected raw access token in Authorization")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer provider.Close()
			app.BaseURL = provider.URL
			s := &Server{secret: bytes.Repeat([]byte{1}, 32)}
			credentials, _ := json.Marshal(map[string]string{"token": tc.token})
			encrypted, err := Encrypt(s.secret, string(credentials))
			if err != nil {
				t.Fatal(err)
			}
			result := s.runHealthCheck(&app, encrypted)
			if result.OK != tc.ok || result.StatusCode != tc.status || result.Skipped || requests != 1 {
				t.Fatalf("unexpected health result: %+v, requests=%d", result, requests)
			}
		})
	}
}
