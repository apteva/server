package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationRequestContextCancelsProvider(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer remote.Close()
	defer remote.CloseClientConnections()
	app := &AppTemplate{Slug: "delayed-mock"}
	tool := &AppToolDef{Name: "evaluate", Method: "POST", BaseURL: remote.URL, TimeoutMS: 300000}
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executeIntegrationToolWithRefreshContext(parent, app, tool, map[string]string{}, map[string]any{}, "", nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("provider connection did not cancel")
	}
}
func TestIntegrationRequestDeadlineBoundsBody(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer remote.Close()
	defer remote.CloseClientConnections()
	app := &AppTemplate{Slug: "delayed-mock"}
	tool := &AppToolDef{Name: "evaluate", Method: "POST", BaseURL: remote.URL, TimeoutMS: 300000}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err := executeIntegrationToolWithRefreshContext(ctx, app, tool, map[string]string{}, map[string]any{}, "", nil)
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline")) {
		t.Fatalf("body deadline error=%v", err)
	}
}
