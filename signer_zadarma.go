package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
)

// zadarma signs Zadarma API v1 requests.
//
// Zadarma signs the API method path plus sorted RFC1738 query/form params:
//
//	base64(hmac_sha1(method + params + md5(params), secret))
//
// Catalog params:
//
//	key_field     credential key holding the Zadarma API key, default "api_key"
//	secret_field  credential key holding the Zadarma API secret, default "api_secret"
func init() { RegisterSigner(zadarmaSigner{}) }

type zadarmaSigner struct{}

func (zadarmaSigner) Name() string { return "zadarma" }

func (zadarmaSigner) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	keyField := stringParam(params, "key_field", "api_key")
	secretField := stringParam(params, "secret_field", "api_secret")
	key := creds[keyField]
	secret := creds[secretField]
	if key == "" {
		return nil, fmt.Errorf("zadarma: missing credential %q", keyField)
	}
	if secret == "" {
		return nil, fmt.Errorf("zadarma: missing credential %q", secretField)
	}

	paramsString := zadarmaCanonicalParams(req.URL.RawQuery, string(body))
	paramsHash := md5.Sum([]byte(paramsString))
	canonical := fmt.Sprintf("%s%s%x", req.URL.EscapedPath(), paramsString, paramsHash)

	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", key+":"+signature)
	return nil, nil
}

func zadarmaCanonicalParams(rawQuery, rawBody string) string {
	values := url.Values{}
	for _, raw := range []string{rawQuery, rawBody} {
		if raw == "" {
			continue
		}
		parsed, err := url.ParseQuery(raw)
		if err != nil {
			continue
		}
		for k, items := range parsed {
			for _, item := range items {
				values.Add(k, item)
			}
		}
	}
	return values.Encode()
}
