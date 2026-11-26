package main

import (
	"net/http"
	"testing"
	"time"
)

func TestIntegrationClientAllowsSlowTLSHandshake(t *testing.T) {
	for _, guard := range []bool{false, true} {
		client, err := integrationHTTPClient(&AppTemplate{Slug: "tls-timeout-test"}, nil, time.Minute, guard)
		if err != nil {
			t.Fatalf("guard=%v: %v", guard, err)
		}
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("guard=%v: transport is %T", guard, client.Transport)
		}
		if tr.TLSHandshakeTimeout != integrationTLSHandshakeTimeout {
			t.Fatalf("guard=%v: TLS handshake timeout = %s, want %s", guard, tr.TLSHandshakeTimeout, integrationTLSHandshakeTimeout)
		}
	}
}
