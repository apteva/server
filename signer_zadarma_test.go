package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
)

func TestZadarmaSignerSetsAuthorization(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://api.zadarma.com/v1/info/balance/?sip=100&format=json", nil)
	if err != nil {
		t.Fatal(err)
	}

	creds := map[string]string{"api_key": "key123", "api_secret": "secret456"}
	if _, err := (zadarmaSigner{}).Sign(context.Background(), req, nil, creds, nil); err != nil {
		t.Fatalf("Sign returned error: %v", err)
	}

	paramsString := "format=json&sip=100"
	paramsHash := md5.Sum([]byte(paramsString))
	canonical := fmt.Sprintf("/v1/info/balance/%s%x", paramsString, paramsHash)
	mac := hmac.New(sha1.New, []byte("secret456"))
	_, _ = mac.Write([]byte(canonical))
	want := "key123:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestZadarmaSignerUsesFormBodyWhenNoQuery(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.zadarma.com/v1/sms/send/", nil)
	if err != nil {
		t.Fatal(err)
	}

	creds := map[string]string{"api_key": "key123", "api_secret": "secret456"}
	body := []byte("message=hello&number=15551234567")
	if _, err := (zadarmaSigner{}).Sign(context.Background(), req, body, creds, nil); err != nil {
		t.Fatalf("Sign returned error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got == "" {
		t.Fatal("Authorization header was not set")
	}
}

func TestZadarmaCanonicalParamsCombinesQueryAndBody(t *testing.T) {
	got := zadarmaCanonicalParams("format=json", "message=hello+world&number=15551234567")
	want := "format=json&message=hello+world&number=15551234567"
	if got != want {
		t.Fatalf("canonical params mismatch\n got: %s\nwant: %s", got, want)
	}
}
