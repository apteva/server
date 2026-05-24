package main

// top signs TOP/Open Platform form requests. Callers provide the
// logical API method through signer params, while this signer supplies
// common TOP parameters and the request signature over every parameter
// except sign.

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"
)

func init() { RegisterSigner(&topSigner{}) }

type topSigner struct{}

func (topSigner) Name() string { return "top" }

func (topSigner) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	method := stringParam(params, "method", "")
	if method == "" {
		return nil, fmt.Errorf("top: params.method required")
	}

	appKeyField := stringParam(params, "app_key_field", "app_key")
	appSecretField := stringParam(params, "app_secret_field", "app_secret")
	appKey := creds[appKeyField]
	appSecret := creds[appSecretField]
	if appKey == "" {
		return nil, fmt.Errorf("top: missing credential %q", appKeyField)
	}
	if appSecret == "" {
		return nil, fmt.Errorf("top: missing credential %q", appSecretField)
	}

	values, err := neturl.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("top: parse form body: %w", err)
	}
	if req.URL != nil && req.URL.RawQuery != "" {
		for k, vs := range req.URL.Query() {
			for _, v := range vs {
				values.Add(k, v)
			}
		}
		req.URL.RawQuery = ""
	}

	values.Set("method", method)
	values.Set("app_key", appKey)
	values.Set("timestamp", time.Now().UTC().Format("2006-01-02 15:04:05"))
	values.Set("format", stringParam(params, "format", "json"))
	values.Set("v", stringParam(params, "v", "2.0"))

	signMethod := strings.ToLower(stringParam(params, "sign_method", "hmac"))
	values.Set("sign_method", signMethod)

	if partnerID := stringParam(params, "partner_id", ""); partnerID != "" {
		values.Set("partner_id", partnerID)
	}
	if appSignature := stringParam(params, "app_signature", ""); appSignature != "" {
		values.Set("app_signature", appSignature)
	}
	appSignatureField := stringParam(params, "app_signature_field", "app_signature")
	if values.Get("app_signature") == "" && creds[appSignatureField] != "" {
		values.Set("app_signature", creds[appSignatureField])
	}
	trackingIDField := stringParam(params, "tracking_id_field", "tracking_id")
	if values.Get("tracking_id") == "" && creds[trackingIDField] != "" {
		values.Set("tracking_id", creds[trackingIDField])
	}
	if boolParam(params, "include_access_token", false) {
		tokenField := stringParam(params, "access_token_field", "access_token")
		if token := creds[tokenField]; token != "" {
			values.Set("access_token", token)
		}
	}

	signature, err := signTOP(values, appSecret, signMethod)
	if err != nil {
		return nil, err
	}
	values.Set("sign", signature)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	return []byte(values.Encode()), nil
}

func signTOP(values neturl.Values, secret, method string) (string, error) {
	keys := make([]string, 0, len(values))
	for key, vs := range values {
		if key == "sign" || len(vs) == 0 {
			continue
		}
		if len(vs) == 1 && vs[0] == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for _, key := range keys {
		vs := append([]string(nil), values[key]...)
		sort.Strings(vs)
		for _, value := range vs {
			canonical.WriteString(key)
			canonical.WriteString(value)
		}
	}

	switch strings.ToLower(method) {
	case "hmac", "hmac-md5", "hmac_md5":
		mac := hmac.New(md5.New, []byte(secret))
		mac.Write([]byte(canonical.String()))
		return strings.ToUpper(hex.EncodeToString(mac.Sum(nil))), nil
	case "md5":
		sum := md5.Sum([]byte(secret + canonical.String() + secret))
		return strings.ToUpper(hex.EncodeToString(sum[:])), nil
	default:
		return "", fmt.Errorf("top: unsupported sign_method %q", method)
	}
}

func boolParam(p map[string]any, key string, def bool) bool {
	if v, ok := p[key].(bool); ok {
		return v
	}
	return def
}
