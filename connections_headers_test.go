package main

import (
	"net/http"
	"testing"
)

func TestPickForwardableHeaders_IncludesRequestIDs(t *testing.T) {
	h := http.Header{}
	h.Set("Request-Id", "req-1")
	h.Set("X-Request-Id", "req-2")
	h.Set("X-ElevenLabs-Request-Id", "req-3")
	h.Set("Xi-Request-Id", "req-4")

	got := pickForwardableHeaders(h)
	if got["Request-Id"] != "req-1" {
		t.Fatalf("Request-Id = %q, want req-1", got["Request-Id"])
	}
	if got["X-Request-Id"] != "req-2" {
		t.Fatalf("X-Request-Id = %q, want req-2", got["X-Request-Id"])
	}
	if got["X-ElevenLabs-Request-Id"] != "req-3" {
		t.Fatalf("X-ElevenLabs-Request-Id = %q, want req-3", got["X-ElevenLabs-Request-Id"])
	}
	if got["Xi-Request-Id"] != "req-4" {
		t.Fatalf("Xi-Request-Id = %q, want req-4", got["Xi-Request-Id"])
	}
}
