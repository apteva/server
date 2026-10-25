package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppProxyRemoteDashboardActionsAndLiveEvents(t *testing.T) {
	s := newTestServer(t)
	key := testPrivateAPIKey(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	var writes atomic.Int32
	closed := make(chan struct{})
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deployment-token" || r.Header.Get("X-Apteva-Project-ID") != "proxy-project" {
			http.Error(w, "wrong identity", 403)
			return
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"changed\":true}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(closed)
			return
		}
		if r.Method == "POST" {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 921, AppName: "remote-dashboard", ProjectID: "proxy-project", SidecarURL: sidecar.URL, Token: "deployment-token"})
	mux := http.NewServeMux()
	s.registerAppRuntimeRoutes(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	for _, method := range []string{"GET", "POST"} {
		req, _ := http.NewRequest(method, gateway.URL+"/apps/remote-dashboard/_install/921/items?project_id=proxy-project", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", method, resp.StatusCode, body)
		}
	}
	if writes.Load() != 1 {
		t.Fatalf("writes=%d", writes.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", gateway.URL+"/apps/remote-dashboard/_install/921/events?project_id=proxy-project", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: {\"changed\":true}\n" {
		t.Fatalf("stream was buffered or corrupted: %q %v", line, err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream stream survived cancellation")
	}
}
