package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

func TestAPNsJWTSigner(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	req, _ := http.NewRequest(http.MethodPost, "https://api.push.apple.com/3/device/device-token", nil)
	req.Header.Set("apns-topic", "ai.apteva.mobile")
	req.Header.Set("x-apteva-apns-environment", "sandbox")

	_, err = (apnsJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{
		"team_id":     "TEAM123456",
		"key_id":      "KEY1234567",
		"private_key": strings.ReplaceAll(privatePEM, "\n", `\n`),
	}, nil)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got := req.URL.Host; got != "api.sandbox.push.apple.com" {
		t.Fatalf("host = %q", got)
	}
	if got := req.Header.Get("apns-topic"); got != "ai.apteva.mobile" {
		t.Fatalf("apns-topic = %q", got)
	}
	if got := req.Header.Get("x-apteva-apns-environment"); got != "" {
		t.Fatalf("internal environment header leaked: %q", got)
	}

	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var header, claims map[string]any
	decodeJWTPart(t, parts[0], &header)
	decodeJWTPart(t, parts[1], &claims)
	if header["alg"] != "ES256" || header["kid"] != "KEY1234567" {
		t.Fatalf("header = %#v", header)
	}
	if claims["iss"] != "TEAM123456" || claims["iat"] == nil {
		t.Fatalf("claims = %#v", claims)
	}
	if _, exists := claims["aud"]; exists {
		t.Fatalf("APNs claims must not contain App Store Connect audience: %#v", claims)
	}
	if _, exists := claims["exp"]; exists {
		t.Fatalf("APNs claims must not contain an expiration claim: %#v", claims)
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

func TestAPNsJWTSignerRejectsUnknownEnvironment(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.push.apple.com/3/device/token", nil)
	req.Header.Set("apns-topic", "ai.apteva.mobile")
	req.Header.Set("x-apteva-apns-environment", "staging")
	_, err := (apnsJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{
		"team_id":     "TEAM123456",
		"key_id":      "KEY1234567",
		"private_key": "unused",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "production or sandbox") {
		t.Fatalf("err = %v", err)
	}
}
