package main

// nonce_form_hmac — generic signer for APIs that require a nonce inside
// a form-encoded body and an HMAC signature over configurable request
// parts. Kraken is one catalog user of this pattern, but the signer is
// intentionally provider-neutral.
//
// Catalog params:
//
//   key_field          credential key for the public/API key (default "api_key")
//   secret_field       credential key for the HMAC secret (default "secret")
//   secret_encoding    raw | base64 | hex (default "raw")
//   key_header         header that receives the API key (optional)
//   signature_header   header that receives the signature (default "X-Signature")
//   signature_encoding hex | base64 | base64_url (default "base64")
//   nonce_field        body field name for the nonce (default "nonce")
//   nonce_unit         ns | ms | s (default "ns")
//   nonce              fixed nonce, mostly useful in tests
//   content_type       emitted body Content-Type (default application/x-www-form-urlencoded)
//   prehash_hash       sha256 | sha512 | none (default "sha256")
//   prehash_parts      ordered parts to hash before HMAC (default ["nonce", "body"])
//   hmac_hash          sha256 | sha512 (default "sha512")
//   message_parts      ordered parts fed into HMAC (default ["path", "prehash"])
//
// Supported part names:
//   method, path, path_with_query, query, body, nonce, prehash
//   literal:<text> for static separators or prefixes.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func init() { RegisterSigner(&nonceFormHMACSigner{}) }

type nonceFormHMACSigner struct{}

func (nonceFormHMACSigner) Name() string { return "nonce_form_hmac" }

func (nonceFormHMACSigner) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	cfg, err := parseNonceFormHMACParams(params)
	if err != nil {
		return nil, err
	}
	apiKey := creds[cfg.KeyField]
	secret := creds[cfg.SecretField]
	if cfg.KeyHeader != "" && apiKey == "" {
		return nil, fmt.Errorf("nonce_form_hmac: missing credential %q", cfg.KeyField)
	}
	if secret == "" {
		return nil, fmt.Errorf("nonce_form_hmac: missing credential %q", cfg.SecretField)
	}
	secretBytes, err := decodeSecret(secret, cfg.SecretEncoding)
	if err != nil {
		return nil, fmt.Errorf("nonce_form_hmac: decode secret: %w", err)
	}

	fields, err := nonceFormBodyMap(req, body)
	if err != nil {
		return nil, err
	}
	nonce := cfg.FixedNonce
	if nonce == "" {
		nonce = nonceForUnit(time.Now(), cfg.NonceUnit)
	}
	if existing := strings.TrimSpace(fmt.Sprint(fields[cfg.NonceField])); existing != "" && existing != "<nil>" {
		nonce = existing
	}
	fields[cfg.NonceField] = nonce

	formBody := []byte(formEncode(fields))
	prehash, err := digestParts(cfg.PrehashHash, cfg.PrehashParts, req, formBody, nonce, nil)
	if err != nil {
		return nil, err
	}
	message, err := concatParts(cfg.MessageParts, req, formBody, nonce, prehash)
	if err != nil {
		return nil, err
	}

	macHash, err := hmacHash(cfg.HMACHash)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(macHash, secretBytes)
	mac.Write(message)
	signature, err := encodeSignature(mac.Sum(nil), cfg.SignatureEncoding)
	if err != nil {
		return nil, err
	}

	if cfg.KeyHeader != "" {
		req.Header.Set(cfg.KeyHeader, apiKey)
	}
	req.Header.Set(cfg.SignatureHeader, signature)
	if cfg.ContentType != "" {
		req.Header.Set("Content-Type", cfg.ContentType)
	}
	return formBody, nil
}

type nonceFormHMACParams struct {
	KeyField          string
	SecretField       string
	SecretEncoding    string
	KeyHeader         string
	SignatureHeader   string
	SignatureEncoding string
	NonceField        string
	NonceUnit         string
	FixedNonce        string
	ContentType       string
	PrehashHash       string
	PrehashParts      []string
	HMACHash          string
	MessageParts      []string
}

func parseNonceFormHMACParams(p map[string]any) (*nonceFormHMACParams, error) {
	out := &nonceFormHMACParams{
		KeyField:          stringParam(p, "key_field", "api_key"),
		SecretField:       stringParam(p, "secret_field", "secret"),
		SecretEncoding:    strings.ToLower(stringParam(p, "secret_encoding", "raw")),
		KeyHeader:         stringParam(p, "key_header", ""),
		SignatureHeader:   stringParam(p, "signature_header", "X-Signature"),
		SignatureEncoding: strings.ToLower(stringParam(p, "signature_encoding", "base64")),
		NonceField:        stringParam(p, "nonce_field", "nonce"),
		NonceUnit:         strings.ToLower(stringParam(p, "nonce_unit", "ns")),
		FixedNonce:        stringParam(p, "nonce", ""),
		ContentType:       stringParam(p, "content_type", "application/x-www-form-urlencoded"),
		PrehashHash:       strings.ToLower(stringParam(p, "prehash_hash", "sha256")),
		PrehashParts:      stringSliceParam(p, "prehash_parts", []string{"nonce", "body"}),
		HMACHash:          strings.ToLower(stringParam(p, "hmac_hash", "sha512")),
		MessageParts:      stringSliceParam(p, "message_parts", []string{"path", "prehash"}),
	}
	if out.SignatureHeader == "" {
		return nil, fmt.Errorf("nonce_form_hmac: signature_header required")
	}
	return out, nil
}

func stringSliceParam(p map[string]any, key string, def []string) []string {
	v, ok := p[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case []string:
		if len(x) > 0 {
			return x
		}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return def
}

func nonceForUnit(t time.Time, unit string) string {
	switch unit {
	case "s":
		return strconv.FormatInt(t.Unix(), 10)
	case "ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	default:
		return strconv.FormatInt(t.UnixNano(), 10)
	}
}

func nonceFormBodyMap(req *http.Request, body []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(body) == 0 {
		return out, nil
	}
	contentType := ""
	if req != nil {
		contentType = strings.ToLower(req.Header.Get("Content-Type"))
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		return parseFormBody(body)
	}
	if err := json.Unmarshal(body, &out); err == nil {
		return out, nil
	}
	return parseFormBody(body)
}

func parseFormBody(body []byte) (map[string]any, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("nonce_form_hmac: body must be JSON object or form body")
	}
	out := map[string]any{}
	for k, vals := range values {
		if len(vals) > 0 {
			out[k] = vals[len(vals)-1]
		}
	}
	return out, nil
}

func digestParts(alg string, parts []string, req *http.Request, body []byte, nonce string, prehash []byte) ([]byte, error) {
	msg, err := concatParts(parts, req, body, nonce, prehash)
	if err != nil {
		return nil, err
	}
	switch alg {
	case "", "none":
		return msg, nil
	case "sha256":
		sum := sha256.Sum256(msg)
		return sum[:], nil
	case "sha512":
		sum := sha512.Sum512(msg)
		return sum[:], nil
	default:
		return nil, fmt.Errorf("nonce_form_hmac: unsupported prehash_hash %q", alg)
	}
}

func concatParts(parts []string, req *http.Request, body []byte, nonce string, prehash []byte) ([]byte, error) {
	var out []byte
	for _, part := range parts {
		b, err := signerPartBytes(part, req, body, nonce, prehash)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func signerPartBytes(part string, req *http.Request, body []byte, nonce string, prehash []byte) ([]byte, error) {
	switch part {
	case "method":
		if req == nil {
			return nil, nil
		}
		return []byte(strings.ToUpper(req.Method)), nil
	case "path":
		path := "/"
		if req != nil && req.URL != nil && req.URL.Path != "" {
			path = req.URL.Path
		}
		return []byte(path), nil
	case "path_with_query":
		path := "/"
		if req != nil && req.URL != nil {
			path = req.URL.Path
			if path == "" {
				path = "/"
			}
			if req.URL.RawQuery != "" {
				path += "?" + req.URL.RawQuery
			}
		}
		return []byte(path), nil
	case "query":
		if req == nil || req.URL == nil {
			return nil, nil
		}
		return []byte(req.URL.RawQuery), nil
	case "body":
		return body, nil
	case "nonce":
		return []byte(nonce), nil
	case "prehash":
		return prehash, nil
	default:
		if strings.HasPrefix(part, "literal:") {
			return []byte(strings.TrimPrefix(part, "literal:")), nil
		}
		return nil, fmt.Errorf("nonce_form_hmac: unsupported part %q", part)
	}
}

func hmacHash(alg string) (func() hash.Hash, error) {
	switch alg {
	case "sha256":
		return sha256.New, nil
	case "", "sha512":
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("nonce_form_hmac: unsupported hmac_hash %q", alg)
	}
}
