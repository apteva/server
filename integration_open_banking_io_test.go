package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type obTestTransport func(*http.Request) (*http.Response, error)

func (f obTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenBankingIO(t *testing.T) {
	var bundle struct {
		APIKey        string `json:"apiKey"`
		EncryptionKey struct {
			PrivateKey string `json:"privateKey"`
		} `json:"encryptionKey"`
	}
	raw, _ := os.ReadFile("testdata/open-banking-io/credentials.json")
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	accounts, _ := os.ReadFile("testdata/open-banking-io/accounts.json")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	syncCalled := false
	http.DefaultTransport = obTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Api-Key") != bundle.APIKey {
			t.Fatal("missing API key")
		}
		body := string(accounts)
		status := 200
		if strings.HasSuffix(r.URL.Path, "/sync") {
			syncCalled = true
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["uid"] == "" || len(payload) != 1 {
				t.Fatalf("invalid sync payload keys")
			}
			body = `{"reason":"reconnect_needed","bankErrorCode":"expired"}`
			status = 409
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	creds := map[string]string{"api_base_url": "https://bank.test", "api_key": bundle.APIKey, "private_key": bundle.EncryptionKey.PrivateKey}
	app := &AppTemplate{Slug: "open-banking-io"}
	result, err := executeIntegrationTool(app, &AppToolDef{Name: "list_accounts"}, creds, map[string]any{}, "")
	if err != nil || !result.Success {
		t.Fatalf("accounts failed: %v %+v", err, result)
	}
	first := result.Data.([]any)[0].(map[string]any)
	if first["iban"] == "" || first["iban"] == nil {
		t.Fatal("missing decrypted IBAN")
	}
	if _, ok := first["uidEnc"]; ok {
		t.Fatal("session leaked")
	}
	result, err = executeIntegrationTool(app, &AppToolDef{Name: "sync_account"}, creds, map[string]any{"account_id": first["id"]}, "")
	if err != nil || result.Success || result.Status != 409 || !syncCalled {
		t.Fatalf("sync refusal lost: %v %+v", err, result)
	}
	// Corruption must fail closed.
	http.DefaultTransport = obTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"enc":"corrupt"}]`)), Header: http.Header{}}, nil
	})
	result, err = executeOpenBankingIO(context.Background(), &AppToolDef{Name: "list_accounts"}, creds, nil)
	if err != nil || result.Success {
		t.Fatal("tampered envelope accepted")
	}
}
