package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() { RegisterSigner(&oauth1Signer{}) }

type oauth1Signer struct{}

func (oauth1Signer) Name() string { return "oauth1" }

func (oauth1Signer) Sign(_ context.Context, req *http.Request, body []byte,
	creds map[string]string, params map[string]any) ([]byte, error) {
	consumerKey := firstCredential(creds, stringParam(params, "consumer_key_field", "client_id"), "consumer_key", "api_key")
	consumerSecret := firstCredential(creds, stringParam(params, "consumer_secret_field", "client_secret"), "consumer_secret", "api_secret")
	token := firstCredential(creds, stringParam(params, "token_field", "oauth_token"), "token")
	tokenSecret := firstCredential(creds, stringParam(params, "token_secret_field", "oauth_token_secret"), "token_secret")
	if consumerKey == "" || consumerSecret == "" {
		return nil, fmt.Errorf("oauth1: missing consumer key or secret")
	}
	if token == "" || tokenSecret == "" {
		return nil, fmt.Errorf("oauth1: missing access token or token secret")
	}
	return nil, signOAuth1Request(req, body, consumerKey, consumerSecret, token, tokenSecret, nil, "", "")
}

func firstCredential(creds map[string]string, keys ...string) string {
	for _, key := range keys {
		if key != "" && creds[key] != "" {
			return creds[key]
		}
	}
	return ""
}

func signOAuth1Request(req *http.Request, body []byte, consumerKey, consumerSecret, token, tokenSecret string,
	extra map[string]string, nonce, timestamp string) error {
	if nonce == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("oauth1 nonce: %w", err)
		}
		nonce = hex.EncodeToString(b[:])
	}
	if timestamp == "" {
		timestamp = strconv.FormatInt(time.Now().Unix(), 10)
	}
	oauth := map[string]string{
		"oauth_consumer_key":     consumerKey,
		"oauth_nonce":            nonce,
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        timestamp,
	}
	if token != "" {
		oauth["oauth_token"] = token
	}
	for k, v := range extra {
		if strings.HasPrefix(k, "oauth_") && k != "oauth_signature" && v != "" {
			oauth[k] = v
		}
	}

	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(oauth)+len(req.URL.Query()))
	for k, values := range req.URL.Query() {
		for _, v := range values {
			pairs = append(pairs, pair{oauth1Escape(k), oauth1Escape(v)})
		}
	}
	contentType := strings.ToLower(req.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") && len(body) > 0 {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return fmt.Errorf("oauth1 form body: %w", err)
		}
		for k, vals := range values {
			for _, v := range vals {
				pairs = append(pairs, pair{oauth1Escape(k), oauth1Escape(v)})
			}
		}
	}
	for k, v := range oauth {
		pairs = append(pairs, pair{oauth1Escape(k), oauth1Escape(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	normalized := make([]string, len(pairs))
	for i, p := range pairs {
		normalized[i] = p.key + "=" + p.value
	}
	baseURL := *req.URL
	baseURL.RawQuery = ""
	baseURL.Fragment = ""
	base := strings.ToUpper(req.Method) + "&" + oauth1Escape(baseURL.String()) + "&" + oauth1Escape(strings.Join(normalized, "&"))
	key := oauth1Escape(consumerSecret) + "&" + oauth1Escape(tokenSecret)
	mac := hmac.New(sha1.New, []byte(key))
	_, _ = mac.Write([]byte(base))
	oauth["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	keys := make([]string, 0, len(oauth))
	for k := range oauth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, oauth1Escape(k), oauth1Escape(oauth[k])))
	}
	req.Header.Set("Authorization", "OAuth "+strings.Join(parts, ", "))
	return nil
}

func oauth1Escape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
