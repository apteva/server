package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoogleServiceAccountSignerExchangesAndCachesToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount, _ := json.Marshal(map[string]string{
		"project_id":     "apteva-mobile",
		"private_key_id": "firebase-key-id",
		"private_key": string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: privateKey,
		})),
		"client_email": "push@apteva-mobile.iam.gserviceaccount.com",
	})

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		data, _ := io.ReadAll(r.Body)
		values, err := url.ParseQuery(string(data))
		if err != nil {
			t.Fatal(err)
		}
		if values.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" ||
			strings.Count(values.Get("assertion"), ".") != 2 {
			t.Fatalf("unexpected token request: %s", data)
		}
		writeJSON(w, map[string]any{
			"access_token": "firebase-access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	signer := newGoogleServiceAccountSigner()
	paths := []string{
		"https://fcm.googleapis.com/v1/projects/-/messages:send",
		"https://fcm.googleapis.com/v1/projects/{project_id}/messages:send",
	}
	for _, path := range paths {
		request := httptest.NewRequest(
			http.MethodPost,
			path,
			nil,
		)
		_, err := signer.Sign(
			context.Background(),
			request,
			nil,
			map[string]string{"service_account_json": string(serviceAccount)},
			map[string]any{"token_url": tokenServer.URL},
		)
		if err != nil {
			t.Fatal(err)
		}
		if request.Header.Get("Authorization") != "Bearer firebase-access-token" {
			t.Fatalf("authorization=%q", request.Header.Get("Authorization"))
		}
		if request.URL.Path != "/v1/projects/apteva-mobile/messages:send" {
			t.Fatalf("path=%q", request.URL.Path)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token exchange calls=%d, want 1", tokenCalls.Load())
	}
}

func TestGoogleServiceAccountSignerPreservesOAuthBearerToken(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"https://firebase.googleapis.com/v1beta1/projects",
		nil,
	)
	request.Header.Set("Authorization", "Bearer oauth-access-token")

	if _, err := newGoogleServiceAccountSigner().Sign(
		context.Background(),
		request,
		nil,
		map[string]string{"access_token": "oauth-access-token"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer oauth-access-token" {
		t.Fatalf("authorization=%q", got)
	}
}

func TestGoogleServiceAccountSignerRequiresOAuthOrLegacyCredentials(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"https://firebase.googleapis.com/v1beta1/projects",
		nil,
	)
	_, err := newGoogleServiceAccountSigner().Sign(
		context.Background(),
		request,
		nil,
		map[string]string{},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "OAuth access token") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseGoogleServiceAccountRejectsIncompleteJSON(t *testing.T) {
	_, err := parseGoogleServiceAccount(`{"project_id":"apteva-mobile"}`)
	if err == nil {
		t.Fatal("expected incomplete service account to fail")
	}
}
