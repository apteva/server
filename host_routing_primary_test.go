package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostRouterPrimaryHostRedirectsOnlyWithNativeIngress(t *testing.T) {
	nextCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	hr := NewHostRouter(&Server{primaryHost: "agents.example.com"}, next)

	t.Setenv("APTEVA_INGRESS_ENABLED", "1")
	req := httptest.NewRequest(http.MethodGet, "http://agents.example.com/api/health?full=1", nil)
	rec := httptest.NewRecorder()
	hr.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://agents.example.com/api/health?full=1" {
		t.Fatalf("redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if nextCalls != 0 {
		t.Fatalf("next handler called during redirect")
	}

	req = httptest.NewRequest(http.MethodGet, "https://agents.example.com/api/health", nil)
	req.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	hr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("HTTPS request did not reach primary handler: code=%d calls=%d", rec.Code, nextCalls)
	}

	t.Setenv("APTEVA_INGRESS_ENABLED", "0")
	req = httptest.NewRequest(http.MethodGet, "http://agents.example.com/api/health", nil)
	rec = httptest.NewRecorder()
	hr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || nextCalls != 2 {
		t.Fatalf("disabled native ingress changed primary behavior: code=%d calls=%d", rec.Code, nextCalls)
	}
}
