package main

// polymarket_l2 — Polymarket CLOB L2 HMAC authentication. Wraps
// hmac_sha256 with the exact recipe Polymarket's CLOB expects so
// integration JSON only needs `{"name": "polymarket_l2"}` (no params).
//
// Recipe (from py-clob-client and clob-client TypeScript SDK):
//
//   canonical = timestamp + method + request_path + body
//   secret    = base64-decoded `secret` credential (NOT raw bytes)
//   signature = base64-url-encoded HMAC-SHA256(secret, canonical), no padding
//
// Headers attached:
//
//   POLY_ADDRESS     — wallet address (creds.address)
//   POLY_SIGNATURE   — signature
//   POLY_TIMESTAMP   — unix-seconds timestamp string
//   POLY_API_KEY     — L2 api key (creds.api_key)
//   POLY_PASSPHRASE  — L2 passphrase (creds.passphrase)
//
// GET requests pass empty body; the canonical string then ends with
// the request_path. Polymarket's reference implementations include the
// raw body verbatim — no JSON-canonicalization step, so the body bytes
// we have on hand are exactly what gets signed.

import (
	"context"
	"fmt"
	"net/http"
)

func init() { RegisterSigner(&polymarketL2Signer{}) }

type polymarketL2Signer struct{}

func (polymarketL2Signer) Name() string { return "polymarket_l2" }

func (p polymarketL2Signer) Sign(ctx context.Context, req *http.Request, body []byte,
	creds map[string]string, _ map[string]any) ([]byte, error) {
	if creds["api_key"] == "" || creds["secret"] == "" || creds["passphrase"] == "" {
		return nil, fmt.Errorf("polymarket_l2: missing api_key/secret/passphrase (run derive_api_key first)")
	}
	if creds["address"] == "" {
		return nil, fmt.Errorf("polymarket_l2: missing address credential")
	}

	// Delegate the actual HMAC work to the generic signer with
	// polymarket's exact canonical recipe.
	innerParams := map[string]any{
		"key_field":          "api_key",
		"secret_field":       "secret",
		"secret_encoding":    "base64",
		"canonical":          "{ts}{method}{path}{body}",
		"signature_header":   "POLY_SIGNATURE",
		"signature_encoding": "base64_url",
		"key_header":         "POLY_API_KEY",
		"timestamp_header":   "POLY_TIMESTAMP",
		"timestamp_unit":     "s",
	}
	if _, err := (hmacSha256Signer{}).Sign(ctx, req, body, creds, innerParams); err != nil {
		return nil, err
	}

	// Polymarket-specific headers that hmac_sha256 doesn't set.
	req.Header.Set("POLY_ADDRESS", creds["address"])
	req.Header.Set("POLY_PASSPHRASE", creds["passphrase"])
	return nil, nil
}
