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
	"fmt"
	"net/http"
	"strings"
	"time"
)

// doba signs Doba Retailer API requests.
//
// Doba requires public request headers appKey, signType, timestamp, and an
// RSA2 signature over the public header fields.
//
// Catalog params:
//
//	app_key_field       credential key for the app key, default "app_key"
//	private_key_field   credential key for the private key, default "private_key"
//	sign_type           header signType value, default "rsa2"
//	timestamp_unit      "ms" (default) or "s"
//	*_header            optional header-name overrides
func init() { RegisterSigner(dobaSigner{}) }

type dobaSigner struct{}

func (dobaSigner) Name() string { return "doba" }

func (dobaSigner) Sign(_ context.Context, req *http.Request, _ []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	appKeyField := stringParam(params, "app_key_field", "app_key")
	privateKeyField := stringParam(params, "private_key_field", "private_key")
	appKey := creds[appKeyField]
	if appKey == "" {
		appKey = creds["appKey"]
	}
	privateKey := creds[privateKeyField]
	if privateKey == "" {
		privateKey = creds["privateKey"]
	}
	if appKey == "" {
		return nil, fmt.Errorf("missing credential %q", appKeyField)
	}
	if privateKey == "" {
		return nil, fmt.Errorf("missing credential %q", privateKeyField)
	}

	signType := stringParam(params, "sign_type", "")
	if signType == "" {
		signType = creds["sign_type"]
	}
	if signType == "" {
		signType = "rsa2"
	}

	ts := formatDobaTimestamp(time.Now(), stringParam(params, "timestamp_unit", "ms"))
	canonical := fmt.Sprintf("appKey=%s&signType=%s&timestamp=%s", appKey, signType, ts)
	key, err := parseDobaRSAPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, err
	}
	sign := base64.StdEncoding.EncodeToString(sig)

	req.Header.Set(stringParam(params, "app_key_header", "appKey"), appKey)
	req.Header.Set(stringParam(params, "sign_type_header", "signType"), signType)
	req.Header.Set(stringParam(params, "timestamp_header", "timestamp"), ts)
	req.Header.Set(stringParam(params, "signature_header", "sign"), sign)
	return nil, nil
}

func formatDobaTimestamp(t time.Time, unit string) string {
	if strings.EqualFold(unit, "s") {
		return fmt.Sprintf("%d", t.Unix())
	}
	return fmt.Sprintf("%d", t.UnixMilli())
}

func parseDobaRSAPrivateKey(raw string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(raw)))
	if block == nil {
		compact := strings.Join(strings.Fields(raw), "")
		wrapped := "-----BEGIN PRIVATE KEY-----\n" + wrapDobaPEM(compact) + "\n-----END PRIVATE KEY-----"
		block, _ = pem.Decode([]byte(wrapped))
	}
	if block == nil {
		return nil, fmt.Errorf("invalid Doba RSA private key PEM")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Doba RSA private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("Doba private key is not an RSA key")
	}
	return rsaKey, nil
}

func wrapDobaPEM(s string) string {
	if len(s) <= 64 {
		return s
	}
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64])
		b.WriteByte('\n')
		s = s[64:]
	}
	b.WriteString(s)
	return b.String()
}
