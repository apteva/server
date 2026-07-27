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

func init() { RegisterSigner(apnsJWTSigner{}) }

type apnsJWTSigner struct{}

func (apnsJWTSigner) Name() string { return "apns_jwt" }

func (apnsJWTSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, _ map[string]any) ([]byte, error) {
	teamID := strings.TrimSpace(creds["team_id"])
	keyID := strings.TrimSpace(creds["key_id"])
	privateKey := creds["private_key"]
	bundleID := strings.TrimSpace(creds["bundle_id"])
	if teamID == "" || keyID == "" || privateKey == "" || bundleID == "" {
		return nil, fmt.Errorf("missing team_id/key_id/private_key/bundle_id")
	}

	switch environment := strings.ToLower(strings.TrimSpace(creds["environment"])); environment {
	case "", "production":
		req.URL.Scheme = "https"
		req.URL.Host = "api.push.apple.com"
	case "sandbox":
		req.URL.Scheme = "https"
		req.URL.Host = "api.sandbox.push.apple.com"
	default:
		return nil, fmt.Errorf("environment must be production or sandbox")
	}

	key, err := parseAPNsPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	token, err := signJWT(
		map[string]any{"alg": "ES256", "kid": keyID, "typ": "JWT"},
		map[string]any{"iss": teamID, "iat": now},
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
	req.Header.Set("apns-topic", bundleID)
	return nil, nil
}

func parseAPNsPrivateKey(raw string) (*ecdsa.PrivateKey, error) {
	normalized := normalizeAPNsPrivateKey(raw)
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("private_key must be the APNs .p8 PEM downloaded from Apple Developer")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("APNs private_key must be an ES256 P-256 key")
	}
	return key, nil
}

func normalizeAPNsPrivateKey(raw string) string {
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
	body := strings.Join(strings.Fields(normalized[beginAt+len(begin):endAt]), "")
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
