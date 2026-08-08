package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestOAuth1SignatureMatchesRFC5849Example(t *testing.T) {
	req, err := http.NewRequest("POST", "http://example.com/request?b5=%3D%253D&a3=a&c%40=&a2=r%20b", strings.NewReader("c2&a3=2+q"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	err = signOAuth1Request(req, []byte("c2&a3=2+q"), "9djdj82h48djs9d2", "j49sk3j29djd", "kkk9d7dh3k39sjv7", "dh893hdasih9", nil, "7d8f3e4a", "137131201")
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); !strings.Contains(got, `oauth_signature="r6%2FTJjbCOr97%2F%2BUU0NsvSne7s5g%3D"`) {
		t.Fatalf("unexpected OAuth signature: %s", got)
	}
}

func TestOAuth1SignerRequiresUserToken(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://ads-api.x.com/12/accounts", nil)
	_, err := (oauth1Signer{}).Sign(t.Context(), req, nil, map[string]string{
		"client_id": "key", "client_secret": "secret",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("expected missing token error, got %v", err)
	}
}
