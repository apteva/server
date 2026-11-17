package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func proxyRouter(t *testing.T, backend string) *HostRouter {
	registry := NewInstalledAppsRegistry()
	registry.Add(&InstalledApp{InstallID: 1, AppName: "proxy", SidecarURL: backend, Token: "sidecar-secret"})
	cache := NewRouteCache()
	cache.Replace([]Route{{Hostname: "proxy.example", Target: "app://proxy/transport?endpoint_id=e1&ingress_mode=forward_proxy&ingress_auth=app_token", OwnerInstallID: 1}})
	return NewHostRouter(&Server{routeCache: cache, installedApps: registry}, http.NotFoundHandler())
}
func TestForwardProxySNISelectsEndpointAndKeepsOriginAuthority(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transport" || r.URL.Query().Get("endpoint_id") != "e1" || r.Header.Get("Authorization") != "Bearer sidecar-secret" {
			t.Error("untrusted transport")
		}
		raw, _ := base64.RawStdEncoding.DecodeString(r.Header.Get("X-Apteva-Proxy-Request"))
		var meta struct {
			Method, URL, Host string
			Headers           http.Header
		}
		if json.Unmarshal(raw, &meta) != nil {
			t.Error("invalid metadata")
		}
		if meta.URL != "http://destination.example/path" || meta.Host != "destination.example" {
			t.Errorf("destination lost: %+v", meta)
		}
		if meta.Headers.Get("X-Apteva-Caller-Admin") != "" {
			t.Error("caller assertion forwarded")
		}
		if meta.Headers.Get("Authorization") != "Bearer origin-secret" {
			t.Error("origin auth lost")
		}
		io.Copy(w, r.Body)
	}))
	defer backend.Close()
	router := proxyRouter(t, backend.URL)
	request := httptest.NewRequest("POST", "http://destination.example/path", strings.NewReader("payload"))
	request.TLS = &tls.ConnectionState{ServerName: "proxy.example"}
	request.Header.Set("Authorization", "Bearer origin-secret")
	request.Header.Set("X-Apteva-Caller-Admin", "forged")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "payload" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	noTLS := httptest.NewRequest("GET", "http://proxy.example/transport", nil)
	noTLS.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, noTLS)
	if rec.Code != 400 {
		t.Fatalf("spoofed TLS accepted %d", rec.Code)
	}
}
func TestForwardProxyCONNECTBidirectionalNativeTLS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sidecar-secret" {
			http.Error(w, "unauthorized", 401)
			return
		}
		conn, buf, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: apteva-proxy\r\n\r\n")
		buf.Flush()
		io.Copy(conn, buf)
	}))
	defer backend.Close()
	server := httptest.NewTLSServer(proxyRouter(t, backend.URL))
	defer server.Close()
	conn, e := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{ServerName: "proxy.example", InsecureSkipVerify: true})
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "CONNECT destination.example:443 HTTP/1.1\r\nHost: destination.example:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, e := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("connect=%v err=%v", resp, e)
	}
	payload := bytes.Repeat([]byte("echo"), 32000)
	conn.Write(payload)
	raw := make([]byte, len(payload))
	if _, e = io.ReadFull(reader, raw); e != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("stream echo failed %v", e)
	}
}
func TestForwardProxyRegistrationRequiresOwningInstall(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{AppName: "proxy", InstallID: 7, Token: "token"})
	req := IngressExposeRequest{Hostname: "proxy.example", Target: "app://proxy/transport?endpoint_id=e1", OwnerInstallID: 7, Mode: "forward_proxy"}
	route, e := s.ExposeIngressRoute(req)
	if e != nil || route.Mode != "forward_proxy" {
		t.Fatalf("registration=%+v error=%v", route, e)
	}
	for _, mutate := range []func(*IngressExposeRequest){func(r *IngressExposeRequest) { r.OwnerInstallID = 8 }, func(r *IngressExposeRequest) { r.Target = "http://127.0.0.1:8080" }, func(r *IngressExposeRequest) { r.AllowHTTP = true }, func(r *IngressExposeRequest) { r.TLSMode = "off" }} {
		copy := req
		copy.Hostname = "other.example"
		mutate(&copy)
		if _, e = s.ExposeIngressRoute(copy); e == nil {
			t.Fatal("unsafe registration allowed")
		}
	}
	cfg := (&IngressCertManager{server: s}).TLSConfig()
	s.routeCache = NewRouteCache()
	s.routeCache.Replace([]Route{{Hostname: "proxy.example", Target: route.Target, OwnerInstallID: 7}})
	selected, e := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "proxy.example"})
	if e != nil || selected == nil || selected.NextProtos[0] != "http/1.1" {
		t.Fatal("proxy ALPN not restricted")
	}
}

func TestForwardProxyHostnameStillServesNativeACMEHTTPChallenge(t *testing.T) {
	router := proxyRouter(t, "http://127.0.0.1:1")
	router.server.ingressCerts = &IngressCertManager{challenge: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "proxy.example" || r.URL.Path != "/.well-known/acme-challenge/proof" {
			t.Error("challenge authority lost")
		}
		_, _ = io.WriteString(w, "native-challenge-proof")
	})}
	server := httptest.NewServer(router)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/.well-known/acme-challenge/proof", nil)
	req.Host = "proxy.example"
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "native-challenge-proof" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}
