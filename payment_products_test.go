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
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPaymentTrueLayerOfficialWebhookVector(t *testing.T) {
	signature, e := os.ReadFile("testdata/payment-webhooks/truelayer-signature.txt")
	if e != nil {
		t.Fatal(e)
	}
	jwks, e := os.ReadFile("testdata/payment-webhooks/truelayer-jwks.json")
	if e != nil {
		t.Fatal(e)
	}
	body := []byte(`{"event_type":"example","event_id":"18b2842b-a57b-4887-a0a6-d3c7c36f1020"}`)
	r := httptest.NewRequest("POST", "https://example.com/tl-webhook", nil)
	r.Header.Set("Tl-Signature", strings.TrimSpace(string(signature)))
	r.Header.Set("X-Tl-webhook-Timestamp", "2021-11-29T11:42:55Z")
	r.Header.Set("Content-Type", "application/json")
	fetched := 0
	fetch := func(_ context.Context, m, u string, _ []byte) ([]byte, error) {
		fetched++
		if m != "GET" || u != "https://webhooks.truelayer.com/.well-known/jwks" {
			t.Fatal("untrusted key URL")
		}
		return jwks, nil
	}
	verify := func(b []byte) error {
		return verifyPaymentWebhook(context.Background(), "truelayer-payments", r, b, map[string]string{"environment": "truelayer"}, fetch, time.Now())
	}
	if e := verify(body); e != nil {
		t.Fatal(e)
	}
	if e := verify([]byte(`{"event_type":"payment_settled"}`)); e == nil {
		t.Fatal("accepted altered body")
	}
	r.URL.Path = "/wrong"
	if e := verify(body); e == nil {
		t.Fatal("accepted wrong path")
	}
	r.URL.Path = "/tl-webhook"
	parts := strings.Split(r.Header.Get("Tl-Signature"), ".")
	var h map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(raw, &h)
	h["jku"] = "https://attacker.example/keys"
	raw, _ = json.Marshal(h)
	parts[0] = base64.RawURLEncoding.EncodeToString(raw)
	r.Header.Set("Tl-Signature", strings.Join(parts, "."))
	before := fetched
	if e := verify(body); e == nil || fetched != before {
		t.Fatal("untrusted JKU was fetched")
	}
}

func TestPaymentPlaidWebhookVerification(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	body := []byte(`{"webhook_type":"TRANSFER","webhook_code":"TRANSFER_EVENTS_UPDATE"}`)
	jwk := paymentJWK{Kty: "EC", Crv: "P-256", Alg: "ES256", Kid: "kid", X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}
	fetch := func(_ context.Context, m, u string, b []byte) ([]byte, error) {
		if m != "POST" || u != "https://sandbox.plaid.com/webhook_verification_key/get" {
			t.Fatal("wrong key lookup")
		}
		return json.Marshal(map[string]any{"key": jwk})
	}
	signed := func(iat int64) string {
		sum := sha256.Sum256(body)
		h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "kid"})
		c, _ := json.Marshal(map[string]any{"iat": iat, "request_body_sha256": hex.EncodeToString(sum[:])})
		prefix := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
		digest := sha256.Sum256([]byte(prefix))
		rr, ss, _ := ecdsa.Sign(rand.Reader, key, digest[:])
		sig := make([]byte, 64)
		rr.FillBytes(sig[:32])
		ss.FillBytes(sig[32:])
		return prefix + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	r := httptest.NewRequest("POST", "https://example.com/webhooks/a", nil)
	r.Header.Set("Plaid-Verification", signed(now.Unix()))
	check := func(b []byte) error {
		return verifyPaymentWebhook(context.Background(), "plaid", r, b, map[string]string{"environment": "sandbox", "client_id": "client", "secret": "secret"}, fetch, now)
	}
	if e := check(body); e != nil {
		t.Fatal(e)
	}
	if e := check([]byte(`{"webhook_type":"FAKE"}`)); e == nil {
		t.Fatal("accepted wrong hash")
	}
	r.Header.Set("Plaid-Verification", signed(now.Unix()-301))
	if e := check(body); e == nil {
		t.Fatal("accepted stale JWT")
	}
	r.Header.Set("Plaid-Verification", signed(now.Unix()))
	expired := now.Unix()
	jwk.ExpiredAt = &expired
	if e := check(body); e == nil {
		t.Fatal("accepted expired key")
	}
}

func TestPaymentSaltEdgeCallbackSignature(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	body := []byte(`{"data":{"payment_id":"p"}}`)
	callback := "https://example.com/webhooks/a"
	digest := sha256.Sum256(append([]byte(callback+"|"), body...))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	signature := base64.StdEncoding.EncodeToString(sig)
	if e := verifySaltEdgeCallback(&key.PublicKey, callback, body, signature); e != nil {
		t.Fatal(e)
	}
	if e := verifySaltEdgeCallback(&key.PublicKey, callback+"?bad", body, signature); e == nil {
		t.Fatal("accepted callback URL change")
	}
	if e := verifySaltEdgeCallback(&key.PublicKey, callback, []byte(`{}`), signature); e == nil {
		t.Fatal("accepted tampered body")
	}
	block, _ := pem.Decode([]byte(saltEdgeCallbackKey))
	if block == nil {
		t.Fatal("missing pinned key")
	}
	if _, e := x509.ParsePKIXPublicKey(block.Bytes); e != nil {
		t.Fatal(e)
	}
}

func TestPaymentProductSigners(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, name := range []string{"truelayer_payments", "saltedge_pis"} {
		var key any = ec
		if name == "saltedge_pis" {
			key = rsaKey
		}
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		private := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		body := []byte(`{"amount":100}`)
		r := httptest.NewRequest("POST", "https://example.com/v3/payments", nil)
		r.Header.Set("Idempotency-Key", "intent")
		if _, e := (paymentProductSigner(name)).Sign(context.Background(), r, body, map[string]string{"private_key": private, "signing_key_id": "kid"}, nil); e != nil {
			t.Fatal(e)
		}
		if name == "saltedge_pis" {
			sum := sha256.Sum256([]byte(r.Header.Get("Expires-at") + "|POST|" + r.URL.String() + "|" + string(body)))
			sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("Signature"))
			if e := rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, sum[:], sig); e != nil {
				t.Fatal(e)
			}
		} else {
			parts := strings.Split(r.Header.Get("Tl-Signature"), ".")
			payload := "POST /v3/payments\nIdempotency-Key: intent\n" + string(body)
			sum := sha512.Sum512([]byte(parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))))
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			if e := verifyPaymentECDSA(&ec.PublicKey, sum[:], sig); e != nil {
				t.Fatal(e)
			}
		}
	}
}

func TestPaymentTellerOptionsAndUnsignedIngress(t *testing.T) {
	app := openBankingCatalog(t, "teller")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "OPTIONS" || len(body) != 0 {
			t.Error("OPTIONS request has wrong method/body")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"schemes":["zelle"]}`))
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	found := false
	for i := range app.Tools {
		if app.Tools[i].Name == "get_payment_capabilities" {
			found = true
			result, e := executeIntegrationTool(app, &app.Tools[i], map[string]string{"username": "token", "password": "x"}, map[string]any{"account_id": "a"}, "")
			if e != nil || !result.Success {
				t.Fatalf("OPTIONS: %v %#v", e, result)
			}
		}
	}
	if !found {
		t.Fatal("missing capabilities tool")
	}
	s := newTestServer(t)
	for _, slug := range []string{"plaid", "truelayer-payments", "saltedge-payments"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/webhooks/a", strings.NewReader(`{}`))
		s.handleSubscriptionWebhook(w, r, &Subscription{Slug: slug, Enabled: true}, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unsigned %s accepted: %d", slug, w.Code)
		}
	}
}
