package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func init() { RegisterSigner(ghostAdminSigner{}) }

type ghostAdminSigner struct{}

func (ghostAdminSigner) Name() string { return "ghost_admin" }

func (ghostAdminSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	keyField := stringParam(params, "key_field", "admin_api_key")
	key := creds[keyField]
	if key == "" {
		return nil, fmt.Errorf("ghost_admin: missing credential %q", keyField)
	}
	token, err := buildGhostAdminJWT(key, time.Now())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Ghost "+token)
	return nil, nil
}

func buildGhostAdminJWT(adminKey string, now time.Time) (string, error) {
	id, secret, ok := strings.Cut(strings.TrimSpace(adminKey), ":")
	if !ok || id == "" || secret == "" {
		return "", fmt.Errorf("ghost_admin: admin API key must be id:secret")
	}
	secretBytes, err := hex.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("ghost_admin: decode hex secret: %w", err)
	}
	iat := now.Unix()
	header := map[string]any{"alg": "HS256", "typ": "JWT", "kid": id}
	payload := map[string]any{"iat": iat, "exp": iat + 300, "aud": "/admin/"}
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)
	unsigned := ghostBase64URL(headerJSON) + "." + ghostBase64URL(payloadJSON)

	mac := hmac.New(sha256.New, secretBytes)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + ghostBase64URL(mac.Sum(nil)), nil
}

func ghostBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
