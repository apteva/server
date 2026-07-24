package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func hostRouterForTest(t *testing.T, backendURL, hostname string) *HostRouter {
	t.Helper()
	routes := NewRouteCache()
	routes.Replace([]Route{{
		Hostname:  hostname,
		Target:    backendURL,
		AllowHTTP: true,
	}})
	return NewHostRouter(&Server{
		routeCache: routes,
		edgeCache:  NewEdgeCache(),
	}, http.NotFoundHandler())
}

func TestHostRouterWebSocketBypassesEdgeCache(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade backend connection: %v", err)
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read backend message: %v", err)
			return
		}
		if err := conn.WriteMessage(messageType, payload); err != nil {
			t.Errorf("echo backend message: %v", err)
		}
	}))
	defer backend.Close()

	const hostname = "voice.example.test"
	front := httptest.NewServer(hostRouterForTest(t, backend.URL, hostname))
	defer front.Close()
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}

	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", frontURL.Host)
		},
	}
	conn, resp, err := dialer.Dial("ws://"+hostname+"/media", nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("dial through HostRouter: %v (status=%d body=%q)", err, resp.StatusCode, body)
		}
		t.Fatalf("dial through HostRouter: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if got := resp.Header.Get("X-Cache"); got != "" {
		t.Fatalf("X-Cache=%q on WebSocket response, want empty", got)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write WebSocket message: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read WebSocket echo: %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != "hello" {
		t.Fatalf("echo type=%d payload=%q", messageType, payload)
	}
}

func TestHostRouterEdgeCacheStillMissesThenHits(t *testing.T) {
	var backendRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendRequests.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("asset"))
	}))
	defer backend.Close()

	const hostname = "assets.example.test"
	front := httptest.NewServer(hostRouterForTest(t, backend.URL, hostname))
	defer front.Close()

	client := front.Client()
	for i, wantCache := range []string{"MISS", "HIT"} {
		req, err := http.NewRequest(http.MethodGet, front.URL+"/app.js", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = hostname
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read request %d: %v", i+1, readErr)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "asset" {
			t.Fatalf("request %d: status=%d body=%q", i+1, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Cache"); got != wantCache {
			t.Fatalf("request %d: X-Cache=%q, want %q", i+1, got, wantCache)
		}
	}
	if got := backendRequests.Load(); got != 1 {
		t.Fatalf("backend requests=%d, want 1", got)
	}
}

func TestProtocolUpgradeDetectionAndCacheRejection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://voice.example.test/media", nil)
	req.Header.Set("Upgrade", "WebSocket")
	req.Header.Add("Connection", "keep-alive")
	req.Header.Add("Connection", "uPgRaDe")

	if !requestIsProtocolUpgrade(req) {
		t.Fatal("multi-value mixed-case protocol upgrade was not detected")
	}
	if cacheableRequest(req) {
		t.Fatal("protocol upgrade must not be cacheable")
	}

	ordinary := httptest.NewRequest(http.MethodGet, "http://voice.example.test/asset", nil)
	ordinary.Header.Set("Connection", "keep-alive")
	ordinary.Header.Set("Upgrade", "websocket")
	if requestIsProtocolUpgrade(ordinary) {
		t.Fatal("Upgrade without Connection: upgrade must not be treated as a protocol upgrade")
	}
	if !cacheableRequest(ordinary) {
		t.Fatal("ordinary unauthenticated GET should remain cacheable")
	}

	if strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {
		t.Fatal("test setup must put upgrade outside Header.Get's first value")
	}
}
