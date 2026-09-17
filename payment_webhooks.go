package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const saltEdgeCallbackKey = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA8qxSS5BmftHK/eyW+o98
NR89TyDmz1V8e6yyFdoMPddEYN4Bcidkk2whoJEc/T/AKghHQ9Nq+DuebnRYYcSJ
YT99VbR1PpIw2R9i8z+DZ79hoizy6z+rwxGANnJOr5BDF5HUKJ8uKS9yGRieojFv
Y9j+rxH6Fj6P90bO4d2igYYspKVoI3Zb3hWS0LrWN+JXAaW9qcOmQPTgO0WG0MUK
gB3NNMfN7gMIkl3chbaULiEgVciP2qZTIGb1b7IDr5+fA9oVVGaXiybdieGHIa4J
S7JNTf0JjWrIKd2DaczKULnghqNQsnoCu+S8BurEOJR5EN1BBfQBPlbSh+ru1zgZ
AQIDAQAB
-----END PUBLIC KEY-----`

func isPaymentWebhookProvider(slug string) bool {
	return slug == "plaid" || slug == "truelayer-payments" || slug == "saltedge-payments"
}

// Verify against the subscription's owned connection, never caller-selected credentials.
// Provider signatures replace the unrelated HMAC auto-generated for generic subscriptions.
func (s *Server) verifyPaymentSubscription(r *http.Request, sub *Subscription, body []byte) (bool, error) {
	if sub.ConnectionID <= 0 {
		if isPaymentWebhookProvider(sub.Slug) {
			return true, errors.New("payment webhook requires a bound connection")
		}
		return false, nil
	}
	conn, enc, err := s.store.GetConnection(sub.UserID, sub.ConnectionID)
	if err != nil || conn == nil {
		return true, errors.New("webhook connection unavailable")
	}
	if !isPaymentWebhookProvider(conn.AppSlug) {
		return false, nil
	}
	raw, err := Decrypt(s.secret, enc)
	if err != nil {
		return true, err
	}
	var creds map[string]string
	if err = json.Unmarshal([]byte(raw), &creds); err != nil {
		return true, err
	}
	return true, verifyPaymentWebhook(r.Context(), conn.AppSlug, r, body, creds, paymentWebhookJSON, time.Now())
}

type paymentWebhookFetch func(context.Context, string, string, []byte) ([]byte, error)

func paymentWebhookJSON(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("webhook key redirects are not allowed") }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("webhook key lookup failed: %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if len(data) > 1<<20 {
		return nil, errors.New("webhook key response too large")
	}
	return data, err
}

type paymentJWSHeader struct {
	Alg     string `json:"alg"`
	Kid     string `json:"kid"`
	Jku     string `json:"jku"`
	Version string `json:"tl_version"`
	Headers string `json:"tl_headers"`
}
type paymentJWK struct {
	Kty       string `json:"kty"`
	Crv       string `json:"crv"`
	X         string `json:"x"`
	Y         string `json:"y"`
	Kid       string `json:"kid"`
	Alg       string `json:"alg"`
	ExpiredAt *int64 `json:"expired_at"`
}

func paymentECPublicKey(k paymentJWK, curve elliptic.Curve) (*ecdsa.PublicKey, error) {
	x, e := base64.RawURLEncoding.DecodeString(k.X)
	if e != nil {
		return nil, e
	}
	y, e := base64.RawURLEncoding.DecodeString(k.Y)
	if e != nil {
		return nil, e
	}
	expected := "P-256"
	if curve == elliptic.P521() {
		expected = "P-521"
	}
	if k.Kty != "EC" || k.Crv != expected {
		return nil, errors.New("unexpected webhook key type")
	}
	pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("invalid webhook public key")
	}
	return pub, nil
}
func verifyPaymentECDSA(pub *ecdsa.PublicKey, digest, sig []byte) error {
	size := (pub.Curve.Params().BitSize + 7) / 8
	if len(sig) != size*2 || !ecdsa.Verify(pub, digest, new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])) {
		return errors.New("invalid webhook signature")
	}
	return nil
}
func verifyPaymentWebhook(ctx context.Context, provider string, r *http.Request, body []byte, creds map[string]string, fetch paymentWebhookFetch, now time.Time) error {
	if r.Method != "POST" || !json.Valid(body) {
		return errors.New("payment webhook requires POST JSON")
	}
	if provider == "saltedge-payments" {
		if r.Header.Get("Signature-key-version") != "6.0" {
			return errors.New("unsupported Salt Edge callback key version")
		}
		callback := creds["callback_url"]
		u, e := url.Parse(callback)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.EscapedPath() != r.URL.EscapedPath() || u.RawQuery != r.URL.RawQuery {
			return errors.New("Salt Edge callback URL is not configured for this endpoint")
		}
		block, _ := pem.Decode([]byte(saltEdgeCallbackKey))
		parsed, e := x509.ParsePKIXPublicKey(block.Bytes)
		if e != nil {
			return e
		}
		return verifySaltEdgeCallback(parsed.(*rsa.PublicKey), callback, body, r.Header.Get("Signature"))
	}
	signature := r.Header.Get("Plaid-Verification")
	if provider == "truelayer-payments" {
		signature = r.Header.Get("Tl-Signature")
	}
	parts := strings.Split(signature, ".")
	if len(parts) != 3 {
		return errors.New("invalid webhook JWS")
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return e
	}
	var header paymentJWSHeader
	if e = json.Unmarshal(raw, &header); e != nil {
		return e
	}
	if header.Kid == "" {
		return errors.New("missing webhook key id")
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil {
		return e
	}
	if provider == "plaid" {
		if header.Alg != "ES256" {
			return errors.New("Plaid webhook requires ES256")
		}
		env := creds["environment"]
		if env != "sandbox" && env != "production" {
			return errors.New("invalid Plaid webhook environment")
		}
		if creds["client_id"] == "" || creds["secret"] == "" {
			return errors.New("missing Plaid credentials")
		}
		request, _ := json.Marshal(map[string]string{"client_id": creds["client_id"], "secret": creds["secret"], "key_id": header.Kid})
		data, e := fetch(ctx, "POST", "https://"+env+".plaid.com/webhook_verification_key/get", request)
		if e != nil {
			return e
		}
		var keys struct {
			Key paymentJWK `json:"key"`
		}
		if e = json.Unmarshal(data, &keys); e != nil {
			return e
		}
		if keys.Key.Kid != header.Kid || keys.Key.Alg != "ES256" || keys.Key.ExpiredAt != nil {
			return errors.New("invalid or expired Plaid key")
		}
		key, e := paymentECPublicKey(keys.Key, elliptic.P256())
		if e != nil {
			return e
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if e = verifyPaymentECDSA(key, digest[:], sig); e != nil {
			return e
		}
		payload, e := base64.RawURLEncoding.DecodeString(parts[1])
		if e != nil {
			return e
		}
		var claims struct {
			Iat  int64  `json:"iat"`
			Hash string `json:"request_body_sha256"`
		}
		if e = json.Unmarshal(payload, &claims); e != nil {
			return e
		}
		age := now.Unix() - claims.Iat
		if claims.Iat == 0 || age > 300 || age < -30 {
			return errors.New("stale Plaid webhook")
		}
		hash := sha256.Sum256(body)
		expected := hex.EncodeToString(hash[:])
		if subtle.ConstantTimeCompare([]byte(expected), []byte(claims.Hash)) != 1 {
			return errors.New("Plaid webhook body hash mismatch")
		}
		return nil
	}
	if provider != "truelayer-payments" {
		return errors.New("unsupported payment webhook provider")
	}
	allowed := "https://webhooks.truelayer-sandbox.com/.well-known/jwks"
	switch creds["environment"] {
	case "truelayer":
		allowed = "https://webhooks.truelayer.com/.well-known/jwks"
	case "", "truelayer-sandbox":
	default:
		return errors.New("invalid TrueLayer environment")
	}
	if header.Alg != "ES512" || header.Version != "2" || header.Jku != allowed || parts[1] != "" {
		return errors.New("invalid TrueLayer JWS metadata")
	}
	data, e := fetch(ctx, "GET", allowed, nil)
	if e != nil {
		return e
	}
	var keys struct {
		Keys []paymentJWK `json:"keys"`
	}
	if e = json.Unmarshal(data, &keys); e != nil {
		return e
	}
	payload := r.Method + " " + strings.TrimSuffix(r.URL.EscapedPath(), "/") + "\n"
	if header.Headers != "" {
		seen := map[string]bool{}
		for _, name := range strings.Split(header.Headers, ",") {
			if name == "" || strings.TrimSpace(name) != name || seen[strings.ToLower(name)] {
				return errors.New("invalid signed header list")
			}
			seen[strings.ToLower(name)] = true
			value := r.Header.Get(name)
			if value == "" {
				return errors.New("missing signed webhook header")
			}
			payload += name + ": " + value + "\n"
		}
	}
	payload += string(body)
	digest := sha512.Sum512([]byte(parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))))
	for _, k := range keys.Keys {
		if k.Kid == header.Kid {
			key, e := paymentECPublicKey(k, elliptic.P521())
			if e != nil {
				return e
			}
			return verifyPaymentECDSA(key, digest[:], sig)
		}
	}
	return errors.New("TrueLayer webhook key not found")
}

func verifySaltEdgeCallback(key *rsa.PublicKey, callback string, body []byte, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(append([]byte(callback+"|"), body...))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig)
}
