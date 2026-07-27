package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func init() { RegisterSigner(appStoreConnectJWTSigner{}) }

type appStoreConnectJWTSigner struct{}

func (appStoreConnectJWTSigner) Name() string { return "app_store_connect_jwt" }

func (appStoreConnectJWTSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, _ map[string]any) ([]byte, error) {
	issuerID := creds["issuer_id"]
	keyID := creds["key_id"]
	privateKey := creds["private_key"]
	if issuerID == "" || keyID == "" || privateKey == "" {
		return nil, fmt.Errorf("missing issuer_id/key_id/private_key")
	}
	key, err := parseAppStoreConnectPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	token, err := signJWT(
		map[string]any{"alg": "ES256", "kid": keyID, "typ": "JWT"},
		map[string]any{"iss": issuerID, "iat": now, "exp": now + 1190, "aud": "appstoreconnect-v1"},
		func(message []byte) ([]byte, error) {
			digest := sha256.Sum256(message)
			r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
			if err != nil {
				return nil, err
			}
			return joseECDSASignature(r, s, 32), nil
		},
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil, nil
}

func parseAppStoreConnectPrivateKey(raw string) (*ecdsa.PrivateKey, error) {
	normalized := normalizeAppStoreConnectPrivateKey(raw)
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("private_key must be the .p8 PEM downloaded from App Store Connect")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse App Store Connect private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("App Store Connect private_key must be an ES256 P-256 key")
	}
	return key, nil
}

func normalizeAppStoreConnectPrivateKey(raw string) string {
	normalized := strings.TrimSpace(raw)
	normalized = strings.ReplaceAll(normalized, `\r\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\r`, "\n")
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	const begin = "-----BEGIN PRIVATE KEY-----"
	const end = "-----END PRIVATE KEY-----"
	beginAt := strings.Index(normalized, begin)
	endAt := strings.Index(normalized, end)
	if beginAt < 0 || endAt <= beginAt {
		return normalized
	}

	body := normalized[beginAt+len(begin) : endAt]
	body = strings.Join(strings.Fields(body), "")
	if body == "" {
		return normalized
	}

	var rebuilt strings.Builder
	rebuilt.WriteString(begin)
	rebuilt.WriteByte('\n')
	for len(body) > 64 {
		rebuilt.WriteString(body[:64])
		rebuilt.WriteByte('\n')
		body = body[64:]
	}
	rebuilt.WriteString(body)
	rebuilt.WriteByte('\n')
	rebuilt.WriteString(end)
	rebuilt.WriteByte('\n')
	return rebuilt.String()
}
