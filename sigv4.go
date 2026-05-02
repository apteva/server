package main

// AWS Signature Version 4 signing for outbound integration calls
// (SES, S3, anything else that lives behind aws_sigv4 auth).
//
// We don't import aws-sdk-go-v2 — every catalog template that uses
// SigV4 today is a thin REST wrapper, and the signing algorithm is
// stable. Inlining keeps the dep graph narrow and matches how every
// other auth type is implemented in this file.
//
// The signer is request-shaped (mutates a *http.Request + body bytes
// it's given) so it slots cleanly into executeIntegrationTool right
// before client.Do.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signAWSSigV4 mutates req with the headers AWS expects:
//   - Host (already on the request, asserted)
//   - X-Amz-Date
//   - X-Amz-Content-Sha256 (the hex of sha256(body))
//   - X-Amz-Security-Token (only when sessionToken != "")
//   - Authorization (the signed credential)
//
// body must be the bytes already on req (the caller supplies them
// because http.Request.Body is single-read; we need the bytes both
// to hash and to re-attach as the actual request body).
//
// Returns nil on success; an error if any required field is missing.
func signAWSSigV4(req *http.Request, accessKey, secretKey, sessionToken, region, service string, body []byte) error {
	if accessKey == "" || secretKey == "" {
		return fmt.Errorf("aws_sigv4: access_key_id and secret_access_key required")
	}
	if region == "" {
		return fmt.Errorf("aws_sigv4: region required")
	}
	if service == "" {
		return fmt.Errorf("aws_sigv4: service required (set via auth.aws_sigv4.service in the integration template)")
	}
	if req.URL == nil || req.URL.Host == "" {
		return fmt.Errorf("aws_sigv4: request URL has no host")
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	host := req.URL.Host
	payloadHash := sha256Hex(body)

	// Pin the headers that go into the signature. Adding Host explicitly
	// — req.Header.Get("Host") returns "" on Go's transport because Host
	// is on req.Host, but we still want it in CanonicalHeaders.
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}

	// Build canonical headers + signed-headers list. SES + most v4
	// services accept the minimum set; signing more is optional but
	// pulls in "host;x-amz-content-sha256;x-amz-date" which AWS treats
	// as canonical anyway. We include x-amz-security-token when
	// present.
	headerNames := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if sessionToken != "" {
		headerNames = append(headerNames, "x-amz-security-token")
	}
	sort.Strings(headerNames)
	signedHeaders := strings.Join(headerNames, ";")

	var canonHeaders strings.Builder
	for _, name := range headerNames {
		canonHeaders.WriteString(name)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(req.Header.Get(http.CanonicalHeaderKey(name))))
		canonHeaders.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURIPath(req.URL.Path),
		canonicalQueryString(req.URL.RawQuery),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature,
	))
	return nil
}

// canonicalURIPath URI-encodes each segment of the path per the
// SigV4 spec. AWS wants UNRESERVED-style encoding; net/url.PathEscape
// encodes a few extra chars (e.g. `:`) which is fine for SES but
// breaks S3 in some edge cases. SES + most v2-shaped REST services
// don't trip those edge cases, so we keep it simple.
func canonicalURIPath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// canonicalQueryString sorts key=value pairs by key (and by value
// when keys collide), URI-encodes both halves, and re-joins with &.
// Empty input yields empty output (the canonical-request line then
// becomes blank, which is what SigV4 wants).
func canonicalQueryString(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	parsed := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			continue
		}
		k, v := p, ""
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			k, v = p[:eq], p[eq+1:]
		}
		// Decode any pre-existing percent-escapes, then re-encode under
		// SigV4 rules. Tolerant of malformed input — a bad %XY just
		// stays as-is.
		dk, err := url.QueryUnescape(k)
		if err == nil {
			k = dk
		}
		dv, err := url.QueryUnescape(v)
		if err == nil {
			v = dv
		}
		parsed = append(parsed, [2]string{uriEncode(k, true), uriEncode(v, true)})
	}
	sort.SliceStable(parsed, func(i, j int) bool {
		if parsed[i][0] != parsed[j][0] {
			return parsed[i][0] < parsed[j][0]
		}
		return parsed[i][1] < parsed[j][1]
	})
	parts := make([]string, len(parsed))
	for i, kv := range parsed {
		parts[i] = kv[0] + "=" + kv[1]
	}
	return strings.Join(parts, "&")
}

// uriEncode is the SigV4-spec encoder: percent-encode every byte
// EXCEPT A-Z / a-z / 0-9 / '-' / '_' / '.' / '~', and (for path
// segments only) '/'.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// hasAuthType reports whether the integration's auth.types list
// includes the named scheme. Used to gate signing — most templates
// declare a single type, but the field is plural to leave room for
// providers that accept multiple (e.g. an OAuth fallback).
func hasAuthType(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
