package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

func init() {
	RegisterSigner(&coinbaseJWTSigner{})
	RegisterSigner(&okxSigner{})
	RegisterSigner(&bybitSigner{})
	RegisterSigner(&bitstampSigner{})
}

type coinbaseJWTSigner struct{}

func (coinbaseJWTSigner) Name() string { return "coinbase_jwt" }

func (coinbaseJWTSigner) Sign(_ context.Context, req *http.Request, _ []byte, creds map[string]string, _ map[string]any) ([]byte, error) {
	keyName := firstCred(creds, "key_name", "api_key", "token")
	keySecret := firstCred(creds, "key_secret", "private_key", "api_secret", "apiSecret")
	if keyName == "" {
		return nil, fmt.Errorf("coinbase_jwt: missing key_name")
	}
	if keySecret == "" {
		return nil, fmt.Errorf("coinbase_jwt: missing key_secret/private_key")
	}
	if req.URL == nil {
		return nil, fmt.Errorf("coinbase_jwt: missing request URL")
	}
	requestPath := req.URL.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}
	if req.URL.RawQuery != "" {
		requestPath += "?" + req.URL.RawQuery
	}
	now := time.Now().Unix()
	headerBase := map[string]any{
		"typ":   "JWT",
		"kid":   keyName,
		"nonce": randomHex(16),
	}
	claims := map[string]any{
		"sub": keyName,
		"iss": "cdp",
		"aud": []string{"cdp_service"},
		"nbf": now,
		"exp": now + 120,
		"uri": strings.ToUpper(req.Method) + " " + req.URL.Host + requestPath,
	}

	token, err := signCoinbaseJWT(headerBase, claims, keySecret)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil, nil
}

type okxSigner struct{}

func (okxSigner) Name() string { return "okx" }

func (okxSigner) Sign(_ context.Context, req *http.Request, body []byte, creds map[string]string, _ map[string]any) ([]byte, error) {
	apiKey := creds["api_key"]
	secret := firstCred(creds, "secret_key", "api_secret", "secret")
	passphrase := creds["passphrase"]
	if apiKey == "" || secret == "" || passphrase == "" {
		return nil, fmt.Errorf("okx: missing api_key/secret_key/passphrase")
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := ts + strings.ToUpper(req.Method) + requestPathWithQuery(req) + string(body)
	signature := hmacSHA256Base64([]byte(secret), []byte(prehash))
	req.Header.Set("OK-ACCESS-KEY", apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", passphrase)
	if truthy(creds["simulated"]) || truthy(creds["demo"]) {
		req.Header.Set("x-simulated-trading", "1")
	}
	return nil, nil
}

type bybitSigner struct{}

func (bybitSigner) Name() string { return "bybit" }

func (bybitSigner) Sign(_ context.Context, req *http.Request, body []byte, creds map[string]string, _ map[string]any) ([]byte, error) {
	apiKey := creds["api_key"]
	secret := firstCred(creds, "api_secret", "secret")
	if apiKey == "" || secret == "" {
		return nil, fmt.Errorf("bybit: missing api_key/api_secret")
	}
	recvWindow := creds["recv_window"]
	if recvWindow == "" {
		recvWindow = "5000"
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	payload := string(body)
	if len(body) == 0 && req.URL != nil {
		payload = req.URL.RawQuery
	}
	canonical := ts + apiKey + recvWindow + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	req.Header.Set("X-BAPI-API-KEY", apiKey)
	req.Header.Set("X-BAPI-TIMESTAMP", ts)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
	req.Header.Set("X-BAPI-SIGN", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-BAPI-SIGN-TYPE", "2")
	return nil, nil
}

type bitstampSigner struct{}

func (bitstampSigner) Name() string { return "bitstamp" }

func (bitstampSigner) Sign(_ context.Context, req *http.Request, body []byte, creds map[string]string, _ map[string]any) ([]byte, error) {
	apiKey := creds["api_key"]
	secret := firstCred(creds, "api_secret", "secret")
	if apiKey == "" || secret == "" {
		return nil, fmt.Errorf("bitstamp: missing api_key/api_secret")
	}
	if req.URL == nil {
		return nil, fmt.Errorf("bitstamp: missing request URL")
	}
	nonce := randomHex(18)
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	version := "v2"
	auth := "BITSTAMP " + apiKey
	contentType := req.Header.Get("Content-Type")
	if len(body) == 0 {
		contentType = ""
		req.Header.Del("Content-Type")
	}
	path := req.URL.EscapedPath()
	query := req.URL.RawQuery
	if query != "" {
		query = "?" + query
	}
	stringToSign := auth + strings.ToUpper(req.Method) + req.URL.Host + path + query + contentType + nonce + ts + version + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	req.Header.Set("X-Auth", auth)
	req.Header.Set("X-Auth-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Auth-Nonce", nonce)
	req.Header.Set("X-Auth-Timestamp", ts)
	req.Header.Set("X-Auth-Version", version)
	if subaccount := creds["subaccount_id"]; subaccount != "" {
		req.Header.Set("X-Auth-Subaccount-Id", subaccount)
	}
	return nil, nil
}

func signCoinbaseJWT(header map[string]any, claims map[string]any, keySecret string) (string, error) {
	if edKey, ok, err := parseCoinbaseEd25519Key(keySecret); err != nil {
		return "", fmt.Errorf("coinbase_jwt: parse Ed25519 key: %w", err)
	} else if ok {
		header["alg"] = "EdDSA"
		return signJWT(header, claims, func(message []byte) ([]byte, error) {
			return ed25519.Sign(edKey, message), nil
		})
	}
	ecKey, err := parseCoinbaseECDSAKey(keySecret)
	if err != nil {
		return "", fmt.Errorf("coinbase_jwt: parse ECDSA key: %w", err)
	}
	header["alg"] = "ES256"
	return signJWT(header, claims, func(message []byte) ([]byte, error) {
		digest := sha256.Sum256(message)
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, digest[:])
		if err != nil {
			return nil, err
		}
		return joseECDSASignature(r, s, 32), nil
	})
}

func signJWT(header map[string]any, claims map[string]any, sign func([]byte) ([]byte, error)) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	message := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sig, err := sign([]byte(message))
	if err != nil {
		return "", err
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseCoinbaseEd25519Key(secret string) (ed25519.PrivateKey, bool, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(secret, `\n`, "\n"))
	if block, _ := pem.Decode([]byte(normalized)); block != nil {
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			if k, ok := key.(ed25519.PrivateKey); ok {
				return k, true, nil
			}
		}
		return nil, false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		if b, rawErr := base64.RawStdEncoding.DecodeString(normalized); rawErr == nil {
			decoded = b
		} else if b, urlErr := base64.RawURLEncoding.DecodeString(normalized); urlErr == nil {
			decoded = b
		} else {
			return nil, false, nil
		}
	}
	switch len(decoded) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), true, nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), true, nil
	default:
		return nil, false, nil
	}
}

func parseCoinbaseECDSAKey(secret string) (*ecdsa.PrivateKey, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(secret, `\n`, "\n"))
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("expected PEM private key or Ed25519 base64 secret")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("expected P-256 ECDSA key")
		}
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA private key")
	}
	if ecKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("expected P-256 ECDSA key")
	}
	return ecKey, nil
}

func joseECDSASignature(r, s *big.Int, size int) []byte {
	out := make([]byte, size*2)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(out[size-len(rb):size], rb)
	copy(out[size*2-len(sb):], sb)
	return out
}

func firstCred(creds map[string]string, keys ...string) string {
	for _, key := range keys {
		if v := creds[key]; v != "" {
			return v
		}
	}
	return ""
}

func requestPathWithQuery(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "/"
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	return path
}

func hmacSHA256Base64(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
