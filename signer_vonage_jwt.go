package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func init() { RegisterSigner(vonageJWTSigner{}) }

type vonageJWTSigner struct{}

func (vonageJWTSigner) Name() string { return "vonage_jwt" }

func (vonageJWTSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, _ map[string]any) ([]byte, error) {
	applicationID := strings.TrimSpace(creds["application_id"])
	privateKey := creds["private_key"]
	if applicationID == "" || strings.TrimSpace(privateKey) == "" {
		return nil, fmt.Errorf("missing application_id/private_key required by Vonage Voice API")
	}
	key, err := parseVonagePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	token, err := signJWT(
		map[string]any{"alg": "RS256", "typ": "JWT"},
		map[string]any{
			"application_id": applicationID,
			"iat":            now,
			"exp":            now + 900,
			"jti":            fmt.Sprintf("apteva-%d", time.Now().UnixNano()),
		},
		func(message []byte) ([]byte, error) {
			digest := sha256.Sum256(message)
			return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		},
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil, nil
}

func parseVonagePrivateKey(raw string) (*rsa.PrivateKey, error) {
	normalized := normalizeVonagePrivateKey(raw)
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("private_key must be the PEM downloaded for the Vonage application")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Vonage private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("Vonage private_key must be an RSA private key")
	}
	return key, nil
}

func normalizeVonagePrivateKey(raw string) string {
	normalized := strings.TrimSpace(raw)
	normalized = strings.ReplaceAll(normalized, `\r\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\r`, "\n")
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	for _, marker := range []struct{ begin, end string }{
		{"-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----"},
		{"-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"},
	} {
		beginAt := strings.Index(normalized, marker.begin)
		endAt := strings.Index(normalized, marker.end)
		if beginAt < 0 || endAt <= beginAt {
			continue
		}
		body := strings.Join(strings.Fields(normalized[beginAt+len(marker.begin):endAt]), "")
		if body == "" {
			return normalized
		}
		var rebuilt strings.Builder
		rebuilt.WriteString(marker.begin)
		rebuilt.WriteByte('\n')
		for len(body) > 64 {
			rebuilt.WriteString(body[:64])
			rebuilt.WriteByte('\n')
			body = body[64:]
		}
		rebuilt.WriteString(body)
		rebuilt.WriteByte('\n')
		rebuilt.WriteString(marker.end)
		rebuilt.WriteByte('\n')
		return rebuilt.String()
	}
	return normalized
}
