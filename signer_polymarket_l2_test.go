package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// TestPolymarketL2_KnownVector signs a fixed (timestamp, method, path,
// body) combination with a known secret and compares the resulting
// POLY_SIGNATURE against a value computed independently from the same
// inputs. Both sides use the same algorithm — but the test catches
// regressions in canonical-string assembly, secret decoding, and
// signature encoding (the three things most likely to silently drift).
func TestPolymarketL2_KnownVector(t *testing.T) {
	// Fixed inputs. The test stubs out time.Now via the synthetic
	// timestamp header check instead — we compute the expected
	// signature for whatever ts the signer chooses, then verify the
	// header matches.
	body := []byte(`{"order":{"salt":"1234","maker":"0xabc","price":"0.5","size":"10"}}`)
	req, _ := http.NewRequest("POST", "https://clob.polymarket.com/order", strings.NewReader(string(body)))

	// Base64-encoded secret as Polymarket's UI emits ("derive_api_key" returns base64).
	secret := base64.StdEncoding.EncodeToString([]byte("test-secret-32-bytes-of-entropy!"))

	creds := map[string]string{
		"api_key":    "key-uuid-from-polymarket",
		"secret":     secret,
		"passphrase": "passphrase-from-polymarket",
		"address":    "0xWalletAddress0000000000000000000000000",
	}

	if _, err := (polymarketL2Signer{}).Sign(context.Background(), req, body, creds, nil); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Required headers present.
	for _, h := range []string{"POLY_API_KEY", "POLY_TIMESTAMP", "POLY_SIGNATURE", "POLY_ADDRESS", "POLY_PASSPHRASE"} {
		if req.Header.Get(h) == "" {
			t.Errorf("header %s not set", h)
		}
	}

	// Independently recompute the signature from the headers we observe.
	// This proves the signer used (ts + method + path + body), base64-
	// decoded secret, HMAC-SHA256, base64-url-no-padding signature —
	// the exact recipe Polymarket's CLOB expects. If any one of those
	// changes, the recomputed signature won't match.
	ts := req.Header.Get("POLY_TIMESTAMP")
	canonical := ts + "POST" + "/order" + string(body)
	keyBytes, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(canonical))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if got := req.Header.Get("POLY_SIGNATURE"); got != expected {
		t.Errorf("signature mismatch\n got:  %s\n want: %s\n canonical: %q", got, expected, canonical)
	}
}

// TestPolymarketL2_GetRequest exercises the empty-body path — a GET
// to /balance-allowance should still produce a valid signature whose
// canonical string is ts + "GET" + "/balance-allowance" + "".
func TestPolymarketL2_GetRequest(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://clob.polymarket.com/balance-allowance?asset_type=USDC", nil)

	creds := map[string]string{
		"api_key":    "k",
		"secret":     base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"passphrase": "p",
		"address":    "0xA",
	}
	if _, err := (polymarketL2Signer{}).Sign(context.Background(), req, nil, creds, nil); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if req.Header.Get("POLY_SIGNATURE") == "" {
		t.Error("no signature on GET")
	}
	// Path in canonical string excludes the query — Polymarket's reference
	// implementations sign request_path only (no query). Confirm via
	// independent recomputation.
	ts := req.Header.Get("POLY_TIMESTAMP")
	canonical := ts + "GET" + "/balance-allowance" + ""
	keyBytes, _ := base64.StdEncoding.DecodeString(creds["secret"])
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(canonical))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if got := req.Header.Get("POLY_SIGNATURE"); got != expected {
		t.Errorf("GET signature mismatch\n got:  %s\n want: %s", got, expected)
	}
}

// TestPolymarketL2_MissingCredsErrors — auth keys are mandatory; the
// signer must refuse rather than emit a useless signature against an
// empty key.
func TestPolymarketL2_MissingCredsErrors(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://clob.polymarket.com/balance-allowance", nil)
	for _, missing := range []string{"api_key", "secret", "passphrase", "address"} {
		creds := map[string]string{
			"api_key":    "k",
			"secret":     base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")),
			"passphrase": "p",
			"address":    "0xA",
		}
		delete(creds, missing)
		if _, err := (polymarketL2Signer{}).Sign(context.Background(), req, nil, creds, nil); err == nil {
			t.Errorf("missing %s should error", missing)
		}
	}
}

// TestHMACSha256_Template — generic signer with a non-Polymarket
// recipe (KuCoin-like: ts + method + path_with_query + body, hex
// signature, raw secret, custom headers). Proves the template + config
// machinery actually drives behavior beyond the polymarket case.
func TestHMACSha256_Template(t *testing.T) {
	body := []byte(`{"price":"100","size":"1"}`)
	req, _ := http.NewRequest("POST", "https://api.example.com/v3/orders?clientOid=abc", strings.NewReader(string(body)))

	creds := map[string]string{
		"api_key": "K",
		"secret":  "raw-secret-bytes",
	}
	params := map[string]any{
		"canonical":          "{ts}{method}{path_with_query}{body}",
		"signature_header":   "KC-SIGN",
		"key_header":         "KC-KEY",
		"timestamp_header":   "KC-TS",
		"signature_encoding": "hex",
		"secret_encoding":    "raw",
		"timestamp_unit":     "ms",
	}
	if _, err := (hmacSha256Signer{}).Sign(context.Background(), req, body, creds, params); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if req.Header.Get("KC-KEY") != "K" {
		t.Errorf("KC-KEY not set: %q", req.Header.Get("KC-KEY"))
	}
	if req.Header.Get("KC-TS") == "" {
		t.Error("KC-TS not set")
	}
	sig := req.Header.Get("KC-SIGN")
	if sig == "" {
		t.Fatal("KC-SIGN empty")
	}
	// Hex encoding → all hex chars, 64 long for SHA256.
	if len(sig) != 64 {
		t.Errorf("hex sig wrong length: got %d, want 64 (%q)", len(sig), sig)
	}
	for _, c := range sig {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Errorf("non-hex char in signature: %q", sig)
			break
		}
	}

	// Recompute to verify path_with_query (not just path) was used.
	ts := req.Header.Get("KC-TS")
	canonical := ts + "POST" + "/v3/orders?clientOid=abc" + string(body)
	mac := hmac.New(sha256.New, []byte("raw-secret-bytes"))
	mac.Write([]byte(canonical))
	want := mac.Sum(nil)
	got, _ := encodeSignature(want, "hex")
	if sig != got {
		t.Errorf("hex template signature mismatch\n got:  %s\n want: %s", sig, got)
	}
}
