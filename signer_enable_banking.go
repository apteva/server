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

func init() { RegisterSigner(enableBankingJWTSigner{}) }

type enableBankingJWTSigner struct{}

func (enableBankingJWTSigner) Name() string { return "enable_banking_jwt" }

// Enable Banking identifies the application by kid, with fixed issuer/audience.
// Generate short-lived JWTs per request so scheduled sync never stores an expired token.
func (enableBankingJWTSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, _ map[string]any) ([]byte, error) {
	applicationID := strings.TrimSpace(creds["application_id"])
	if applicationID == "" || strings.TrimSpace(creds["private_key"]) == "" {
		return nil, fmt.Errorf("Enable Banking requires application_id and private_key")
	}
	block, _ := pem.Decode([]byte(normalizeVonagePrivateKey(creds["private_key"])))
	if block == nil {
		return nil, fmt.Errorf("Enable Banking private_key must be an RSA private-key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, fmt.Errorf("parse Enable Banking private key: %w", parseErr)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("Enable Banking private_key must be an RSA private key")
		}
	}
	now := time.Now().Unix()
	token, err := signJWT(
		map[string]any{"alg": "RS256", "typ": "JWT", "kid": applicationID},
		map[string]any{"iss": "enablebanking.com", "aud": "api.enablebanking.com", "iat": now, "exp": now + 900},
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
