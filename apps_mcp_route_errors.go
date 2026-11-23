package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// appMCPRouteErrors returns the errors the app route writes itself on
// POST /api/apps/<name>/mcp as JSON-RPC, so MCP clients can parse them. Only
// plain-text error responses (what http.Error writes) are converted, keeping
// their HTTP status; JSON responses pass through unchanged. The route wraps
// its writer only until it hands the request to the app, whose own responses
// are never rewritten.
type appMCPRouteErrors struct {
	http.ResponseWriter
	r       *http.Request
	status  int
	message bytes.Buffer
	held    bool
}

const appMCPRouteErrorCode = -32000

func newAppMCPRouteErrors(w http.ResponseWriter, r *http.Request) *appMCPRouteErrors {
	return &appMCPRouteErrors{ResponseWriter: w, r: r}
}

func (e *appMCPRouteErrors) WriteHeader(status int) {
	if status >= 400 && strings.HasPrefix(e.Header().Get("Content-Type"), "text/plain") {
		e.status, e.held = status, true
		return
	}
	e.ResponseWriter.WriteHeader(status)
}

func (e *appMCPRouteErrors) Write(p []byte) (int, error) {
	if !e.held {
		return e.ResponseWriter.Write(p)
	}
	if room := 4096 - e.message.Len(); room > 0 {
		e.message.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (e *appMCPRouteErrors) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// finish writes a held error as a JSON-RPC response. It does nothing when no
// error was held, so it can be deferred unconditionally.
func (e *appMCPRouteErrors) finish() {
	if !e.held {
		return
	}
	e.held = false
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if e.r != nil && e.r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(e.r.Body, 1<<20)).Decode(&request)
	}
	var id any
	if len(request.ID) > 0 {
		id = request.ID
	}
	message := strings.TrimSpace(e.message.String())
	if message == "" {
		message = http.StatusText(e.status)
	}
	header := e.Header()
	header.Set("Content-Type", "application/json")
	header.Del("X-Content-Type-Options")
	e.ResponseWriter.WriteHeader(e.status)
	_ = json.NewEncoder(e.ResponseWriter).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    appMCPRouteErrorCode,
			"message": message,
			"data":    map[string]any{"http_status": e.status},
		},
	})
}
