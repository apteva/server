package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/gorilla/websocket"
)

// newWrappedWriterFront serves an app route that answers its own CORS behind
// the same wrappers as the public API: compression, CORS, then the security
// audit recorder. With an Origin header the response is wrapped by
// corsCredentialGuardWriter and then by statusRecorder.
func newWrappedWriterFront(t *testing.T, sidecar http.Handler) *httptest.Server {
	t.Helper()
	s := newTestServer(t)
	s.securityAudit = openTestAudit(t, securityAuditOptions{})
	s.installedApps = NewInstalledAppsRegistry()
	install := seedCORSAppInstall(t, s, "streams", "")
	backend := httptest.NewServer(sidecar)
	t.Cleanup(backend.Close)
	s.installedApps.Add(&InstalledApp{
		InstallID: install, AppName: "streams", SidecarURL: backend.URL, Token: "install-token",
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
			{Prefix: "/live/", NoAuth: true, CORS: sdk.RouteCORSApp},
		}}},
	})
	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	front := httptest.NewServer(compressHTTP((*corsConfig)(nil).middlewareWithDynamicPolicy(s.securityAuditMiddleware(apiMux), s.dynamicAppCORSPolicy)))
	t.Cleanup(front.Close)
	return front
}

// A browser always sends Origin on a WebSocket handshake. The upgrade used
// to fail with 502 because the audit recorder could not reach the hijacker
// beneath the CORS guard.
func TestWebSocketUpgradesThroughWrappedWriters(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	front := newWrappedWriterFront(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if kind, payload, err := conn.ReadMessage(); err == nil {
			_ = conn.WriteMessage(kind, append([]byte("echo:"), payload...))
		}
	}))
	target := "ws" + strings.TrimPrefix(front.URL, "http") + "/apps/streams/live/session"
	for name, header := range map[string]http.Header{
		"with Origin":    {"Origin": {"https://viewer.example"}},
		"without Origin": {},
	} {
		client, response, err := websocket.DefaultDialer.Dial(target, header)
		if err != nil {
			status := 0
			if response != nil {
				status = response.StatusCode
			}
			t.Fatalf("%s: dial failed (status %d): %v", name, status, err)
		}
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := client.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		if _, payload, err := client.ReadMessage(); err != nil || string(payload) != "echo:hi" {
			t.Fatalf("%s: reply %q err=%v, want echo:hi", name, payload, err)
		}
		_ = client.Close()
	}
}

// A streamed response must arrive as it is written. The recorder used to
// drop every flush on these routes, so the first event only arrived when the
// app finished the response.
func TestStreamedResponseFlushesThroughWrappedWriters(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	front := newWrappedWriterFront(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Without a flush the headers never arrive; fail instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/apps/streams/live/events", nil)
	req.Header.Set("Origin", "https://viewer.example")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no response while the app kept the stream open: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status=%d allow-origin=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credentials header %q got through the stream", got)
	}
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- text
	}()
	select {
	case got := <-line:
		if strings.TrimSpace(got) != "data: first" {
			t.Fatalf("first event = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive while the app kept the response open")
	}
}
