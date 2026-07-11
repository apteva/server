package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

func TestAppStoreConnectJWTSigner(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	req, _ := http.NewRequest(http.MethodGet, "https://api.appstoreconnect.apple.com/v1/apps", nil)
	_, err = (appStoreConnectJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{
		"issuer_id":   "issuer-123",
		"key_id":      "KEY123",
		"private_key": strings.ReplaceAll(privatePEM, "\n", `\n`),
	}, nil)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var header, claims map[string]any
	decodeJWTPart(t, parts[0], &header)
	decodeJWTPart(t, parts[1], &claims)
	if header["alg"] != "ES256" || header["kid"] != "KEY123" {
		t.Fatalf("header = %#v", header)
	}
	if claims["iss"] != "issuer-123" || claims["aud"] != "appstoreconnect-v1" {
		t.Fatalf("claims = %#v", claims)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature len=%d err=%v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("JWT signature did not verify")
	}
}

func decodeJWTPart(t *testing.T, part string, target any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
