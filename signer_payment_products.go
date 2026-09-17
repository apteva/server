package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type paymentProductSigner string

func (p paymentProductSigner) Name() string { return string(p) }
func init() {
	RegisterSigner(paymentProductSigner("truelayer_payments"))
	RegisterSigner(paymentProductSigner("saltedge_pis"))
}
func paymentPrivateKey(raw string) (any, error) {
	raw = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(raw), `\r\n`, "\n"), `\n`, "\n")
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, fmt.Errorf("payment signing requires a PEM private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("invalid payment private key")
}
func (p paymentProductSigner) Sign(_ context.Context, req *http.Request, body []byte, creds map[string]string, _ map[string]any) ([]byte, error) {
	parsed, err := paymentPrivateKey(creds["private_key"])
	if err != nil {
		return nil, err
	}
	if p == "saltedge_pis" {
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("Salt Edge PIS requires an RSA key")
		}
		expires := strconv.FormatInt(time.Now().Unix()+60, 10)
		sum := sha256.Sum256([]byte(expires + "|" + req.Method + "|" + req.URL.String() + "|" + string(body)))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			return nil, err
		}
		req.Header.Set("Expires-at", expires)
		req.Header.Set("Signature", base64.StdEncoding.EncodeToString(sig))
		return nil, nil
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P521() {
		return nil, fmt.Errorf("TrueLayer Payments requires an EC P-521 key")
	}
	kid := creds["signing_key_id"]
	idempotency := req.Header.Get("Idempotency-Key")
	if kid == "" || idempotency == "" {
		return nil, fmt.Errorf("TrueLayer Payments requires signing_key_id and idempotency_key")
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES512", "kid": kid, "tl_version": "2", "tl_headers": "Idempotency-Key"})
	protected := base64.RawURLEncoding.EncodeToString(header)
	payload := req.Method + " " + strings.TrimSuffix(req.URL.EscapedPath(), "/") + "\nIdempotency-Key: " + idempotency + "\n" + string(body)
	sum := sha512.Sum512([]byte(protected + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, 132)
	r.FillBytes(sig[:66])
	s.FillBytes(sig[66:])
	req.Header.Set("Tl-Signature", protected+".."+base64.RawURLEncoding.EncodeToString(sig))
	return nil, nil
}
