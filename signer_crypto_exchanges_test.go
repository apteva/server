package main

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCoinbaseJWTSigner_Ed25519(t *testing.T) {
	seed := []byte("0123456789abcdef0123456789abcdef")
	privateKey := ed25519.NewKeyFromSeed(seed)
	secret := base64.StdEncoding.EncodeToString(privateKey)
	req, _ := http.NewRequest("GET", "https://api.coinbase.com/api/v3/brokerage/accounts?limit=10", nil)

	if _, err := (coinbaseJWTSigner{}).Sign(context.Background(), req, nil, map[string]string{
		"key_name":   "organizations/org/apiKeys/key",
		"key_secret": secret,
	}, nil); err != nil {
		t.Fatalf("sign: %v", err)
	}

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("missing bearer token: %q", auth)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d, want 3", len(parts))
	}
	signed := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), signed, sig) {
		t.Fatal("JWT signature did not verify")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if got, want := payload["uri"], "GET api.coinbase.com/api/v3/brokerage/accounts?limit=10"; got != want {
		t.Fatalf("uri claim = %v, want %s", got, want)
	}
}

func TestOKXSignerCanonical(t *testing.T) {
	body := []byte(`{"instId":"BTC-USDT","tdMode":"cash","side":"buy","ordType":"market","sz":"10"}`)
	req, _ := http.NewRequest("POST", "https://www.okx.com/api/v5/trade/order", strings.NewReader(string(body)))
	creds := map[string]string{"api_key": "K", "secret_key": "S", "passphrase": "P", "simulated": "true"}

	if _, err := (okxSigner{}).Sign(context.Background(), req, body, creds, nil); err != nil {
		t.Fatalf("sign: %v", err)
	}
	ts := req.Header.Get("OK-ACCESS-TIMESTAMP")
	canonical := ts + "POST" + "/api/v5/trade/order" + string(body)
	if got, want := req.Header.Get("OK-ACCESS-SIGN"), hmacSHA256Base64([]byte("S"), []byte(canonical)); got != want {
		t.Fatalf("signature mismatch got %s want %s", got, want)
	}
	if req.Header.Get("OK-ACCESS-KEY") != "K" || req.Header.Get("OK-ACCESS-PASSPHRASE") != "P" {
		t.Fatal("missing OKX credential headers")
	}
	if req.Header.Get("x-simulated-trading") != "1" {
		t.Fatal("missing OKX simulated trading header")
	}
}

func TestBybitSignerCanonicalGETAndPOST(t *testing.T) {
	creds := map[string]string{"api_key": "K", "api_secret": "S", "recv_window": "7000"}

	getReq, _ := http.NewRequest("GET", "https://api.bybit.com/v5/order/realtime?category=linear&symbol=BTCUSDT", nil)
	if _, err := (bybitSigner{}).Sign(context.Background(), getReq, nil, creds, nil); err != nil {
		t.Fatalf("sign GET: %v", err)
	}
	ts := getReq.Header.Get("X-BAPI-TIMESTAMP")
	want := hmacSHA256Hex("S", ts+"K"+"7000"+"category=linear&symbol=BTCUSDT")
	if got := getReq.Header.Get("X-BAPI-SIGN"); got != want {
		t.Fatalf("GET signature mismatch got %s want %s", got, want)
	}

	body := []byte(`{"category":"linear","symbol":"BTCUSDT","side":"Buy","orderType":"Market","qty":"0.01"}`)
	postReq, _ := http.NewRequest("POST", "https://api.bybit.com/v5/order/create", strings.NewReader(string(body)))
	if _, err := (bybitSigner{}).Sign(context.Background(), postReq, body, creds, nil); err != nil {
		t.Fatalf("sign POST: %v", err)
	}
	ts = postReq.Header.Get("X-BAPI-TIMESTAMP")
	want = hmacSHA256Hex("S", ts+"K"+"7000"+string(body))
	if got := postReq.Header.Get("X-BAPI-SIGN"); got != want {
		t.Fatalf("POST signature mismatch got %s want %s", got, want)
	}
}

func TestBitstampSignerCanonical(t *testing.T) {
	body := []byte(`amount=0.01&price=100`)
	req, _ := http.NewRequest("POST", "https://www.bitstamp.net/api/v2/buy/btcusd/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	creds := map[string]string{"api_key": "K", "api_secret": "S", "subaccount_id": "sub"}

	if _, err := (bitstampSigner{}).Sign(context.Background(), req, body, creds, nil); err != nil {
		t.Fatalf("sign: %v", err)
	}
	nonce := req.Header.Get("X-Auth-Nonce")
	ts := req.Header.Get("X-Auth-Timestamp")
	stringToSign := "BITSTAMP K" + "POST" + "www.bitstamp.net" + "/api/v2/buy/btcusd/" + "" + "application/x-www-form-urlencoded" + nonce + ts + "v2" + string(body)
	if got, want := req.Header.Get("X-Auth-Signature"), hmacSHA256Hex("S", stringToSign); got != want {
		t.Fatalf("signature mismatch got %s want %s", got, want)
	}
	if req.Header.Get("X-Auth-Subaccount-Id") != "sub" {
		t.Fatal("missing subaccount header")
	}
}

func hmacSHA256Hex(secret, data string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
