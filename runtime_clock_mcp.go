package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Runtime agents receive this read-only clock tool only in manual mode.
func (s *Server) handleRuntimeClockMCP(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/runtime-clock-mcp/"), "/")
	if len(parts) != 2 || s.environments == nil {
		http.NotFound(w, r)
		return
	}
	runtime, ok := s.environments.Get(parts[0])
	if !ok || runtime.clock.State().Mode != "manual" || subtle.ConstantTimeCompare([]byte(parts[1]), []byte(runtime.clockToken)) != 1 {
		http.NotFound(w, r)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	respond := func(result any, message string) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if message != "" {
			out["error"] = map[string]any{"code": -32601, "message": message}
		} else {
			out["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
	switch req.Method {
	case "initialize":
		respond(map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "environment-clock", "version": "1"}}, "")
	case "tools/list":
		respond(map[string]any{"tools": []map[string]any{{"name": "environment_clock_get", "description": "Read current simulated time in this test run.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}, "")
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Name != "environment_clock_get" {
			respond(nil, "unknown tool")
			return
		}
		b, _ := json.Marshal(runtime.clock.State())
		respond(map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}, "")
	default:
		respond(nil, "method not found")
	}
}
