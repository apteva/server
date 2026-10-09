package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/gorilla/websocket"
)

const testClientIPToken = "local-test-app-token"

type clientIPObservation struct{ IP, Error, Authorization, Path string }

func observeClientIP(r *http.Request) clientIPObservation {
	ip, err := sdk.ClientIPFromRequest(r)
	result := clientIPObservation{IP: ip, Authorization: r.Header.Get("Authorization"), Path: r.URL.RequestURI()}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func clientIPProxy(t *testing.T, backend, token string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httputil.NewSingleHostReverseProxy(target)
		original := p.Director
		p.Director = func(req *http.Request) {
			original(req)
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/apps/telephony")
		}
		configureAppClientIP(p, r, token)
		p.ServeHTTP(w, r)
	}))
}

func TestAppClientIPHTTPRewriteTrustAndScrubbing(t *testing.T) {
	t.Setenv("APTEVA_APP_TOKEN", testClientIPToken)
	t.Setenv("APTEVA_TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128,192.0.2.0/24")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(observeClientIP(r)) }))
	defer backend.Close()
	for _, tc := range []struct{ name, forwarded, token, ip string }{
		{"trusted_chain", "203.0.113.9, 198.51.100.7, 192.0.2.4", testClientIPToken, "198.51.100.7"},
		{"ipv6", "2001:db8::7", testClientIPToken, "2001:db8::7"},
		{"no_metadata_for_backend", "198.51.100.7", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := clientIPProxy(t, backend.URL, tc.token)
			defer proxy.Close()
			r, _ := http.NewRequest("GET", proxy.URL+"/apps/telephony/softphone/media/call/session?transport=websocket", nil)
			r.Header.Set("X-Forwarded-For", tc.forwarded)
			r.Header.Set("Authorization", "Bearer visitor-unchanged")
			r.Header.Set(sdk.HeaderClientIPMetadata, "forged")
			r.Header.Set(sdk.HeaderClientIPSignature, "forged")
			r.Header.Set("X-Apteva-Client-IP", "forged")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var got clientIPObservation
			if err = json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.IP != tc.ip || got.Error != "" || got.Authorization != "Bearer visitor-unchanged" || got.Path != "/softphone/media/call/session?transport=websocket" {
				t.Fatalf("%+v", got)
			}
		})
	}
	// An untrusted socket cannot inject a forwarded public address.
	r := httptest.NewRequest("GET", "http://local/path", nil)
	r.RemoteAddr = "203.0.113.8:1000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	target, _ := url.Parse(backend.URL)
	p := httputil.NewSingleHostReverseProxy(target)
	configureAppClientIP(p, r, testClientIPToken)
	p.Director(r)
	if got := observeClientIP(r); got.IP != "203.0.113.8" || got.Error != "" {
		t.Fatalf("untrusted peer: %+v", got)
	}
}

func TestAppClientIPWebSocketReconnectLeavesMediaUnchanged(t *testing.T) {
	t.Setenv("APTEVA_APP_TOKEN", testClientIPToken)
	t.Setenv("APTEVA_TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	observations := make(chan clientIPObservation, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation := observeClientIP(r)
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		observations <- observation
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		kind, data, err := conn.ReadMessage()
		if err == nil {
			conn.WriteMessage(kind, data)
		}
	}))
	defer backend.Close()
	proxy := clientIPProxy(t, backend.URL, testClientIPToken)
	defer proxy.Close()
	for _, ip := range []string{"198.51.100.7", "2001:db8::8"} {
		h := http.Header{"X-Forwarded-For": []string{ip}, "Authorization": []string{"Bearer unchanged"}, sdk.HeaderClientIPMetadata: []string{"forged"}, sdk.HeaderClientIPSignature: []string{"forged"}}
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/apps/telephony/softphone/media/call/session?transport=websocket", h)
		if err != nil {
			t.Fatal(err)
		}
		pcm := bytes.Repeat([]byte{0x34, 0x12}, 480)
		if err = conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		kind, got, err := conn.ReadMessage()
		conn.Close()
		if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, pcm) {
			t.Fatal("media changed", err)
		}
		select {
		case got := <-observations:
			if got.IP != ip || got.Error != "" || got.Authorization != "Bearer unchanged" {
				t.Fatalf("%+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("no handshake")
		}
	}
}

func TestAppClientIPAssertionsAreNotForwardedToGenericTargets(t *testing.T) {
	t.Setenv("APTEVA_APP_TOKEN", testClientIPToken)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sdk.HeaderClientIPMetadata) != "" || r.Header.Get(sdk.HeaderClientIPSignature) != "" || r.Header.Get("X-Apteva-Client-IP") != "" {
			http.Error(w, "assertions retained", 500)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	proxy := clientIPProxy(t, backend.URL, "")
	defer proxy.Close()
	req, _ := http.NewRequest("GET", proxy.URL+"/apps/telephony/path", nil)
	sdk.SetClientIPHeaders(req, "198.51.100.7", testClientIPToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
}

func TestAppClientIPProductionProxyAndHostRoute(t *testing.T) {
	t.Setenv("APTEVA_APP_TOKEN", testClientIPToken)
	t.Setenv("APTEVA_TRUSTED_PROXY_CIDRS", "192.0.2.0/24")
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(observeClientIP(r)) }))
	defer backend.Close()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: 708, AppName: "telephony", ProjectID: "project", SidecarURL: backend.URL, Token: testClientIPToken, Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{Prefix: "/softphone/media/", NoAuth: true}}}}})
	mux := http.NewServeMux()
	s.registerAppRuntimeRoutes(mux)
	req := httptest.NewRequest("GET", "/apps/telephony/_install/708/softphone/media/call/session?transport=websocket", nil)
	req.RemoteAddr = "192.0.2.4:1000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("Authorization", "Bearer visitor")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var got clientIPObservation
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if got.IP != "198.51.100.7" || got.Error != "" || got.Path != "/softphone/media/call/session?transport=websocket" || got.Authorization != "Bearer visitor" {
		t.Fatalf("app proxy: %+v", got)
	}
	// Signing is independent of optional app-token bearer replacement.
	hr := NewHostRouter(s, http.NotFoundHandler())
	target, _ := url.Parse(backend.URL)
	for _, owner := range []int64{708, 999} {
		r := httptest.NewRequest("GET", "http://app.example/softphone/media/call/session", nil)
		r.RemoteAddr = "192.0.2.4:1000"
		r.Header.Set("X-Forwarded-For", "2001:db8::7")
		r.Header.Set("Authorization", "Bearer visitor")
		r.Header.Set(sdk.HeaderClientIPMetadata, "forged")
		r.Header.Set(sdk.HeaderClientIPSignature, "forged")
		out := httptest.NewRecorder()
		hr.serveRoute(out, r, RouteHit{Hostname: "app.example", OriginApp: "telephony", OriginProject: "project", OwnerInstallID: owner, Target: target, AllowHTTP: true})
		var observation clientIPObservation
		if err := json.Unmarshal(out.Body.Bytes(), &observation); err != nil {
			t.Fatal(out.Code, out.Body.String())
		}
		want := ""
		if owner == 708 {
			want = "2001:db8::7"
		}
		if observation.IP != want || observation.Error != "" || observation.Authorization != "Bearer visitor" {
			t.Fatalf("host owner %d: %+v", owner, observation)
		}
	}
}
