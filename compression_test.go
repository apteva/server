package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompressHTTPGzipsJSON(t *testing.T) {
	handler := compressHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary=%q, want Accept-Encoding", got)
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected body %q", string(body))
	}
}

func TestCompressHTTPSkipsEventStream(t *testing.T) {
	handler := compressHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/telemetry/live", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty for event stream", got)
	}
	if got := rec.Body.String(); got != "data: hello\n\n" {
		t.Fatalf("body=%q", got)
	}
}

func TestSPAHandlerCacheHeaders(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "index.html", "<html></html>")
	writeTestFile(t, dir, "main-abc123.js", "console.log('ok')")
	writeTestFile(t, dir, "style.css", "body{}")

	handler := newSPAHandler(dir, "")

	tests := []struct {
		path string
		want string
	}{
		{path: "/", want: "no-cache"},
		{path: "/route/inside/spa", want: "no-cache"},
		{path: "/main-abc123.js", want: "public, max-age=31536000, immutable"},
		{path: "/style.css", want: "no-cache"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Cache-Control"); got != tt.want {
			t.Fatalf("%s Cache-Control=%q, want %q", tt.path, got, tt.want)
		}
	}
}

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
