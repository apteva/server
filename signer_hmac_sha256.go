package main

// hmac_sha256 — generic HMAC-SHA256 request signer driven by a catalog
// template. Used directly by integrations that follow common HMAC-auth
// conventions (Coinbase Advanced, Kraken, KuCoin, OKX, …) and wrapped
// by integration-specific signers that need a fixed recipe
// (polymarket_l2 — see signer_polymarket_l2.go).
//
// Catalog params:
//
//   key_field         (string)  — credential key holding the API key (default: "api_key")
//   secret_field      (string)  — credential key holding the secret (default: "secret")
//   secret_encoding   (string)  — "raw" (default) or "base64" — how to decode the secret before HMAC
//   canonical         (string)  — canonical-string template; see vars below
//   signature_header  (string)  — header name for the signature (default: "X-Signature")
//   signature_encoding (string) — "hex" (default), "base64", or "base64_url" — how to encode the HMAC digest
//   key_header        (string)  — optional header name for the API key
//   timestamp_header  (string)  — optional header name for the timestamp
//   timestamp_unit    (string)  — "s" (default) or "ms" — granularity of the {ts} variable
//
// Template variables resolved in `canonical`:
//
//   {ts}              — current Unix timestamp in declared unit
//   {method}          — uppercase HTTP method ("GET", "POST", …)
//   {path}            — URL path component (no host, no query)
//   {path_with_query} — path + "?" + raw query if non-empty; else just path
//   {body}            — request body bytes, as a string
//
// The signer reads the listed credentials, evaluates the template,
// HMACs the result, attaches the signature header (and key/timestamp
// headers if declared), and returns. It never mutates the body.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func init() { RegisterSigner(&hmacSha256Signer{}) }

type hmacSha256Signer struct{}

func (hmacSha256Signer) Name() string { return "hmac_sha256" }

func (h hmacSha256Signer) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	cfg, err := parseHMACParams(params)
	if err != nil {
		return nil, err
	}
	apiKey := creds[cfg.KeyField]
	secret := creds[cfg.SecretField]
	if apiKey == "" {
		return nil, fmt.Errorf("hmac_sha256: missing credential %q", cfg.KeyField)
	}
	if secret == "" {
		return nil, fmt.Errorf("hmac_sha256: missing credential %q", cfg.SecretField)
	}

	keyBytes, err := decodeSecret(secret, cfg.SecretEncoding)
	if err != nil {
		return nil, fmt.Errorf("hmac_sha256: decode secret: %w", err)
	}

	ts := formatTimestamp(time.Now(), cfg.TimestampUnit)
	canonical := expandCanonical(cfg.Canonical, req, body, ts)

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(canonical))
	sig := mac.Sum(nil)

	signature, err := encodeSignature(sig, cfg.SignatureEncoding)
	if err != nil {
		return nil, err
	}

	req.Header.Set(cfg.SignatureHeader, signature)
	if cfg.KeyHeader != "" {
		req.Header.Set(cfg.KeyHeader, apiKey)
	}
	if cfg.TimestampHeader != "" {
		req.Header.Set(cfg.TimestampHeader, ts)
	}
	return nil, nil
}

// ─── Params + helpers ──────────────────────────────────────────────

type hmacParams struct {
	KeyField          string
	SecretField       string
	SecretEncoding    string
	Canonical         string
	SignatureHeader   string
	SignatureEncoding string
	KeyHeader         string
	TimestampHeader   string
	TimestampUnit     string
}

func parseHMACParams(p map[string]any) (*hmacParams, error) {
	out := &hmacParams{
		KeyField:          stringParam(p, "key_field", "api_key"),
		SecretField:       stringParam(p, "secret_field", "secret"),
		SecretEncoding:    strings.ToLower(stringParam(p, "secret_encoding", "raw")),
		Canonical:         stringParam(p, "canonical", ""),
		SignatureHeader:   stringParam(p, "signature_header", "X-Signature"),
		SignatureEncoding: strings.ToLower(stringParam(p, "signature_encoding", "hex")),
		KeyHeader:         stringParam(p, "key_header", ""),
		TimestampHeader:   stringParam(p, "timestamp_header", ""),
		TimestampUnit:     strings.ToLower(stringParam(p, "timestamp_unit", "s")),
	}
	if out.Canonical == "" {
		return nil, fmt.Errorf("hmac_sha256: canonical template required (params.canonical)")
	}
	return out, nil
}

func stringParam(p map[string]any, key, def string) string {
	if v, ok := p[key].(string); ok && v != "" {
		return v
	}
	return def
}

func decodeSecret(s, encoding string) ([]byte, error) {
	switch encoding {
	case "", "raw":
		return []byte(s), nil
	case "base64":
		// Some providers (Polymarket) use URL-safe base64 even though they
		// don't say so; StdEncoding rejects `-` and `_` characters. Try
		// std first, fall through to URL-safe.
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return b, nil
		}
		return base64.URLEncoding.DecodeString(s)
	case "hex":
		return hex.DecodeString(s)
	}
	return nil, fmt.Errorf("unknown secret_encoding %q", encoding)
}

func encodeSignature(sig []byte, encoding string) (string, error) {
	switch encoding {
	case "", "hex":
		return hex.EncodeToString(sig), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(sig), nil
	case "base64_url":
		// URL-safe alphabet, no padding (RFC 4648 §5). Polymarket
		// requires this exact variant — `+` → `-`, `/` → `_`, no `=`.
		return base64.RawURLEncoding.EncodeToString(sig), nil
	}
	return "", fmt.Errorf("unknown signature_encoding %q", encoding)
}

func formatTimestamp(t time.Time, unit string) string {
	switch unit {
	case "ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	default:
		return strconv.FormatInt(t.Unix(), 10)
	}
}

// expandCanonical resolves template variables in the canonical string.
// We use simple `{var}` syntax (not Go's text/template) because the
// inputs come from operator-controlled catalog JSON and the failure
// mode for an unrecognised `{foo}` should be "leave it literal" rather
// than "panic" — easier to debug a wire-trace with an unsubstituted
// variable than a crashed signer.
func expandCanonical(tmpl string, req *http.Request, body []byte, ts string) string {
	method := strings.ToUpper(req.Method)
	path := "/"
	pathWithQuery := "/"
	if req.URL != nil {
		path = req.URL.Path
		if path == "" {
			path = "/"
		}
		pathWithQuery = path
		if req.URL.RawQuery != "" {
			pathWithQuery = path + "?" + req.URL.RawQuery
		}
	}
	replacer := strings.NewReplacer(
		"{ts}", ts,
		"{method}", method,
		"{path_with_query}", pathWithQuery,
		"{path}", path,
		"{body}", string(body),
	)
	return replacer.Replace(tmpl)
}
