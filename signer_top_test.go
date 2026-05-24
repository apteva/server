package main

import (
	"context"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
)

func TestTOPSignerAddsCommonParamsAndSignature(t *testing.T) {
	req, err := http.NewRequest("POST", "https://example.test/sync", strings.NewReader("keywords=phone&page_no=1"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}

	signer := topSigner{}
	newBody, err := signer.Sign(context.Background(), req, body, map[string]string{
		"app_key":    "123456",
		"app_secret": "secret",
	}, map[string]any{
		"method":      "example.product.query",
		"sign_method": "hmac",
	})
	if err != nil {
		t.Fatal(err)
	}
	form := string(newBody)
	for _, want := range []string{
		"app_key=123456",
		"format=json",
		"keywords=phone",
		"method=example.product.query",
		"page_no=1",
		"sign_method=hmac",
		"v=2.0",
		"sign=",
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("signed form missing %q:\n%s", want, form)
		}
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded;charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestSignTOPDeterministicMD5(t *testing.T) {
	values := mapValues(map[string]string{
		"app_key":     "12129701",
		"format":      "json",
		"method":      "example.category.get",
		"sign_method": "md5",
		"timestamp":   "2026-05-24 00:03:16",
		"v":           "2.0",
	})
	got, err := signTOP(values, "secret", "md5")
	if err != nil {
		t.Fatal(err)
	}
	got2, err := signTOP(values, "secret", "md5")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got != got2 || got != strings.ToUpper(got) {
		t.Fatalf("signature not stable uppercase: %q vs %q", got, got2)
	}
}

func mapValues(in map[string]string) neturl.Values {
	out := neturl.Values{}
	for k, v := range in {
		out.Set(k, v)
	}
	return out
}
