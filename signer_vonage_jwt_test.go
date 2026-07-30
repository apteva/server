package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
)

func TestVonageJWTSignerSignsPastedPrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: mustMarshalPKCS8(t, key),
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://api.nexmo.com/v1/calls", nil)
	_, err = (vonageJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{
		"application_id": "app-123",
		"private_key":    strings.ReplaceAll(privatePEM, "\n", ""),
	}, nil)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var claims map[string]any
	decodeJWTPart(t, parts[1], &claims)
	if claims["application_id"] != "app-123" {
		t.Fatalf("claims = %#v", claims)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify JWT: %v", err)
	}
}

func mustMarshalPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
