package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestSignAWSSigV4_BasicGET asserts the signer attaches the expected
// AWS-shaped headers to a vanilla GET request. We don't pin the
// signature against a fixture (real timestamps move every second);
// instead we verify the surface — Authorization is present, has the
// right algorithm prefix, and signs the canonical headers we
// declared.
func TestSignAWSSigV4_BasicGET(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://email.eu-west-1.amazonaws.com/v2/email/account", nil)
	if err := signAWSSigV4(req, "AKIATEST", "secret", "", "eu-west-1", "ses", nil); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization missing algorithm prefix: %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIATEST/") {
		t.Errorf("Authorization missing credential: %q", auth)
	}
	if !strings.Contains(auth, "/eu-west-1/ses/aws4_request,") {
		t.Errorf("Authorization missing scope: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date,") {
		t.Errorf("Authorization signed-headers wrong: %q", auth)
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date not set")
	}
	// Empty-body sha256.
	if req.Header.Get("X-Amz-Content-Sha256") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("payload hash wrong for empty body: %q", req.Header.Get("X-Amz-Content-Sha256"))
	}
}

func TestSignAWSSigV4_PostWithBody(t *testing.T) {
	body := []byte(`{"FromEmailAddress":"a@b.com"}`)
	req, _ := http.NewRequest("POST", "https://email.eu-west-1.amazonaws.com/v2/email/outbound-emails", strings.NewReader(string(body)))
	if err := signAWSSigV4(req, "AKIATEST", "secret", "", "eu-west-1", "ses", body); err != nil {
		t.Fatal(err)
	}
	// Body hash must match sha256 of the body bytes, not "" (the
	// nil-body path).
	if req.Header.Get("X-Amz-Content-Sha256") == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Error("payload hash matches empty-body hash; signer didn't hash the body")
	}
}

func TestSignAWSSigV4_SessionToken(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://email.eu-west-1.amazonaws.com/v2/email/account", nil)
	if err := signAWSSigV4(req, "AKIATEST", "secret", "FQoGZ-tok", "eu-west-1", "ses", nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Amz-Security-Token") != "FQoGZ-tok" {
		t.Errorf("session token not propagated: %q", req.Header.Get("X-Amz-Security-Token"))
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("Authorization didn't sign x-amz-security-token")
	}
}

func TestSignAWSSigV4_RequiresFields(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://email.eu-west-1.amazonaws.com/v2/email/account", nil)
	if err := signAWSSigV4(req, "", "secret", "", "eu-west-1", "ses", nil); err == nil {
		t.Error("expected error on missing access_key_id")
	}
	if err := signAWSSigV4(req, "key", "secret", "", "", "ses", nil); err == nil {
		t.Error("expected error on missing region")
	}
	if err := signAWSSigV4(req, "key", "secret", "", "eu-west-1", "", nil); err == nil {
		t.Error("expected error on missing service")
	}
}

func TestCanonicalQueryString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"PageSize=100", "PageSize=100"},
		{"b=2&a=1", "a=1&b=2"},
		{"a=1&a=2", "a=1&a=2"},
		{"key with space=v", "key%20with%20space=v"},
	}
	for _, tc := range cases {
		got := canonicalQueryString(tc.in)
		if got != tc.want {
			t.Errorf("canonicalQueryString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
