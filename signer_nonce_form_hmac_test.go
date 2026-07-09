package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNonceFormHMACSigner_KrakenRecipe(t *testing.T) {
	secretBytes := []byte("test-secret-material")
	creds := map[string]string{
		"api_key":    "public-key",
		"api_secret": base64.StdEncoding.EncodeToString(secretBytes),
	}
	bodyIn := []byte(`{"pair":"XBTUSD","type":"buy","volume":"0.01"}`)
	req, err := http.NewRequest("POST", "https://api.kraken.com/0/private/AddOrder", strings.NewReader(string(bodyIn)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	body, err := runSigners(context.Background(), req, bodyIn, creds, []SignerSpec{{
		Name: "nonce_form_hmac",
		Params: map[string]any{
			"key_field":          "api_key",
			"secret_field":       "api_secret",
			"secret_encoding":    "base64",
			"key_header":         "API-Key",
			"signature_header":   "API-Sign",
			"signature_encoding": "base64",
			"nonce":              "1700000000000",
			"prehash_hash":       "sha256",
			"prehash_parts":      []any{"nonce", "body"},
			"hmac_hash":          "sha512",
			"message_parts":      []any{"path", "prehash"},
		},
	}})
	if err != nil {
		t.Fatalf("runSigners: %v", err)
	}
	if got := req.Header.Get("API-Key"); got != "public-key" {
		t.Fatalf("API-Key = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", got)
	}

	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("body is not form encoded: %v", err)
	}
	for k, want := range map[string]string{
		"nonce":  "1700000000000",
		"pair":   "XBTUSD",
		"type":   "buy",
		"volume": "0.01",
	} {
		if got := values.Get(k); got != want {
			t.Fatalf("form[%s] = %q, want %q in %q", k, got, want, string(body))
		}
	}

	digest := sha256.Sum256([]byte("1700000000000" + string(body)))
	mac := hmac.New(sha512.New, secretBytes)
	mac.Write([]byte("/0/private/AddOrder"))
	mac.Write(digest[:])
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got := req.Header.Get("API-Sign"); got != expected {
		t.Fatalf("API-Sign mismatch\n got: %s\nwant: %s", got, expected)
	}
}

func TestNonceFormHMACSignerPreservesExistingNonce(t *testing.T) {
	secretBytes := []byte("test-secret-material")
	creds := map[string]string{
		"api_key":    "public-key",
		"api_secret": base64.StdEncoding.EncodeToString(secretBytes),
	}
	req, _ := http.NewRequest("POST", "https://api.example.com/private/Balance", strings.NewReader("nonce=42"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := (nonceFormHMACSigner{}).Sign(context.Background(), req, []byte("nonce=42"), creds, map[string]any{
		"secret_field": "api_secret",
		"key_header":   "API-Key",
		"nonce":        "99",
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if string(body) != "nonce=42" {
		t.Fatalf("body = %q, want existing nonce preserved", string(body))
	}
}
