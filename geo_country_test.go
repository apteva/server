package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

type staticCountryLookup map[netip.Addr]string

func (s staticCountryLookup) Country(ip netip.Addr) (string, bool) {
	country, ok := s[ip.Unmap()]
	if !ok {
		return "", false
	}
	return normalizeCountryCode(country)
}

func (staticCountryLookup) Close() error { return nil }

func TestApplyGeoCountryHeaderReplacesCallerValue(t *testing.T) {
	visitor := netip.MustParseAddr("81.2.69.142")
	s := &Server{geoCountry: staticCountryLookup{visitor: "gb"}}
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = visitor.String() + ":443"
	req.Header.Set(geoCountryHeader, "ZZ")

	s.applyGeoCountryHeader(req.Header, req)
	if got := req.Header.Get(geoCountryHeader); got != "GB" {
		t.Fatalf("country header=%q, want GB", got)
	}
}

func TestApplyGeoCountryHeaderFailsOpenAndStripsSpoof(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "203.0.113.9:443"
	req.Header.Set(geoCountryHeader, "US")

	(&Server{}).applyGeoCountryHeader(req.Header, req)
	if got := req.Header.Get(geoCountryHeader); got != "" {
		t.Fatalf("disabled GeoIP retained spoofed country %q", got)
	}
}

func TestResolvedClientIPWalksTrustedProxyChain(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "10.0.0.0/8, 2001:db8:1234::/48")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "10.0.0.9:443"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 81.2.69.142, 10.0.0.8")
	if got := clientIP(req); got != "81.2.69.142" {
		t.Fatalf("client IP=%q, want first untrusted hop", got)
	}
}

func TestResolvedClientIPIgnoresUntrustedForwardedHeader(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "10.0.0.0/8")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "81.2.69.142:443"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	if got := clientIP(req); got != "81.2.69.142" {
		t.Fatalf("untrusted peer spoofed client IP: %q", got)
	}
}

func TestResolvedClientIPSkipsLocalTunnelOverlayHops(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "127.0.0.1:443"
	req.Header.Set("X-Forwarded-For", "83.51.215.112, 10.32.2.5, 127.0.0.1")
	if got := clientIP(req); got != "83.51.215.112" {
		t.Fatalf("local tunnel client IP=%q, want public visitor", got)
	}
}

func TestResolvedClientIPSkipsMultipleLocalTunnelOverlayHops(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "[::1]:443"
	req.Header.Set("X-Forwarded-For", "81.2.69.142, fd00::20, 172.20.0.4, ::1")
	if got := clientIP(req); got != "81.2.69.142" {
		t.Fatalf("local tunnel client IP=%q, want public visitor", got)
	}
}

func TestResolvedClientIPSkipsLocalLinkLocalTunnelHop(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "[::1]:443"
	req.Header.Set("X-Forwarded-For", "2001:4860:4860::8888, fe80::1, ::1")
	if got := clientIP(req); got != "2001:4860:4860::8888" {
		t.Fatalf("local tunnel client IP=%q, want public visitor", got)
	}
}

func TestResolvedClientIPDoesNotSkipPrivateHopForRemoteProxy(t *testing.T) {
	t.Setenv(trustedProxyCIDRsEnv, "10.0.0.0/8")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "10.0.0.9:443"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 192.168.20.4")
	if got := clientIP(req); got != "192.168.20.4" {
		t.Fatalf("remote proxy skipped undeclared private hop: %q", got)
	}
}

func TestApplyGeoCountryHeaderThroughLocalTunnel(t *testing.T) {
	visitor := netip.MustParseAddr("83.51.215.112")
	s := &Server{geoCountry: staticCountryLookup{visitor: "ES"}}
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = "127.0.0.1:443"
	req.Header.Set("X-Forwarded-For", "83.51.215.112, 10.32.2.5, 127.0.0.1")

	s.applyGeoCountryHeader(req.Header, req)
	if got := req.Header.Get(geoCountryHeader); got != "ES" {
		t.Fatalf("country header=%q, want ES", got)
	}
}

func TestHostRouterInjectsTrustedCountry(t *testing.T) {
	visitor := netip.MustParseAddr("81.2.69.142")
	var country string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		country = r.Header.Get(geoCountryHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	cache := NewRouteCache()
	cache.Replace([]Route{{Hostname: "shop.example.com", Target: backend.URL, AllowHTTP: true}})
	router := NewHostRouter(&Server{
		routeCache: cache,
		edgeCache:  NewEdgeCache(),
		geoCountry: staticCountryLookup{visitor: "GB"},
	}, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "http://shop.example.com/", nil)
	req.Host = "shop.example.com"
	req.RemoteAddr = visitor.String() + ":443"
	req.Header.Set(geoCountryHeader, "ZZ")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if country != "GB" {
		t.Fatalf("backend country=%q, want GB", country)
	}
}

func TestNewMaxMindCountryLookupRejectsMissingDatabase(t *testing.T) {
	if _, err := newMaxMindCountryLookup(t.TempDir() + "/missing.mmdb"); err == nil {
		t.Fatal("missing MMDB should return an error")
	}
}

func TestNormalizeCountryCode(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{{" es ", "ES", true}, {"USA", "", false}, {"1A", "", false}, {"", "", false}} {
		got, ok := normalizeCountryCode(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("normalizeCountryCode(%q)=(%q,%v), want (%q,%v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func BenchmarkApplyGeoCountryHeader(b *testing.B) {
	visitor := netip.MustParseAddr("81.2.69.142")
	s := &Server{geoCountry: staticCountryLookup{visitor: "GB"}}
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
	req.RemoteAddr = visitor.String() + ":443"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.applyGeoCountryHeader(req.Header, req)
	}
}
