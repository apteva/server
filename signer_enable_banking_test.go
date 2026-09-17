package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnableBankingJWTSigner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshalPKCS8(t, key)})
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for _, raw := range []string{string(pkcs8), strings.ReplaceAll(string(pkcs8), "\n", `\n`), strings.ReplaceAll(string(pkcs1), "\n", "")} {
		req, _ := http.NewRequest("GET", "https://api.enablebanking.com/application", nil)
		_, err := (enableBankingJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{"application_id": "app-1", "private_key": raw}, nil)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatal("invalid JWT")
		}
		var header, claims map[string]any
		decodeJWTPart(t, parts[0], &header)
		decodeJWTPart(t, parts[1], &claims)
		if header["kid"] != "app-1" || header["alg"] != "RS256" || header["typ"] != "JWT" || claims["iss"] != "enablebanking.com" || claims["aud"] != "api.enablebanking.com" {
			t.Fatalf("incorrect header/claims: %#v %#v", header, claims)
		}
		if claims["exp"].(float64)-claims["iat"].(float64) != 900 || int64(claims["iat"].(float64)) > time.Now().Unix() {
			t.Fatal("invalid JWT lifetime")
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnableBankingJWTRejectsInvalidCredentials(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ec)
	for _, creds := range []map[string]string{{}, {"application_id": "a", "private_key": "not a key"}, {"application_id": "a", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}} {
		req, _ := http.NewRequest("GET", "https://api.enablebanking.com/application", nil)
		if _, err := (enableBankingJWTSigner{}).Sign(context.Background(), req, nil, creds, nil); err == nil {
			t.Fatal("accepted invalid credentials")
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("set authorization for invalid key")
		}
	}
}

func openBankingCatalog(t *testing.T, slug string) *AppTemplate {
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

func TestOpenBankingEmbeddedExecution(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, slug := range []string{"enable-banking", "yapily"} {
		t.Run(slug, func(t *testing.T) {
			app := openBankingCatalog(t, slug)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if slug == "yapily" {
					u, p, ok := r.BasicAuth()
					if !ok || u != "app-id" || p != "secret" || r.Header.Get("consent") != "bank-consent" {
						t.Error("incorrect Yapily authentication")
					}
					if r.URL.Query().Get("offset") != "100" {
						t.Error("missing pagination offset")
					}
				} else {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
						t.Error("missing Enable Banking JWT")
					}
					if r.URL.Query().Get("continuation_key") != "page +/=" {
						t.Error("missing pagination key")
					}
				}
				if strings.Contains(r.URL.RawQuery, "consent") || strings.Contains(r.URL.RawQuery, "private_key") || strings.Contains(r.URL.RawQuery, "192.0.2.1") {
					t.Error("credentials or headers leaked into URL")
				}
				if r.Header.Get("Psu-Ip-Address") != "192.0.2.1" {
					t.Error("missing PSU header")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"transactions":[],"continuation_key":"next-page","meta":{"pagination":{"totalCount":250}}}`)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == "get_account_transactions" {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatal("missing transactions tool")
			}
			input := map[string]any{"account_id": "a1", "continuation_key": "page +/=", "psu_ip_address": "192.0.2.1"}
			creds := map[string]string{"application_id": "app-id", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshalPKCS8(t, key)}))}
			if slug == "yapily" {
				input = map[string]any{"accountId": "a1", "consent": "bank-consent", "offset": 100, "psu_ip_address": "192.0.2.1"}
				creds = map[string]string{"username": "app-id", "password": "secret"}
			}
			result, err := executeIntegrationTool(app, tool, creds, input, "")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result.Data)
			if !result.Success || !strings.Contains(string(raw), "next-page") || !strings.Contains(string(raw), "totalCount") {
				t.Fatalf("lost pagination metadata: %s", raw)
			}
		})
	}
}
