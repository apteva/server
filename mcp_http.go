package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// handleMCPEndpoint serves Streamable HTTP MCP transport for integration connections.
// Endpoint: POST/GET /mcp/:connection-id
//
// POST: JSON-RPC request → JSON-RPC response (or SSE stream for streaming)
// GET: SSE stream for server-initiated messages (notifications)
//
// Per MCP spec 2025-03-26: single endpoint, POST for requests, optional SSE.
func (s *Server) handleMCPEndpoint(w http.ResponseWriter, r *http.Request) {
	// Parse connection ID from path: /mcp/123
	idStr := strings.TrimPrefix(r.URL.Path, "/mcp/")
	connectionID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}

	// Load connection (no user auth check — MCP clients don't have session cookies)
	var appSlug, encCreds string
	err = s.store.db.QueryRow(
		"SELECT app_slug, encrypted_credentials FROM connections WHERE id = ?", connectionID,
	).Scan(&appSlug, &encCreds)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	// Decrypt credentials
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	var credentials map[string]string
	json.Unmarshal([]byte(plain), &credentials)

	// Load app from catalog
	app := s.catalog.Get(appSlug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handleMCPPost(w, r, app, credentials, connectionID)
	case http.MethodGet:
		// GET = SSE stream for server notifications (not needed for simple request-response)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Keep connection open until client disconnects
		<-r.Context().Done()
	default:
		http.Error(w, "POST or GET only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request, app *AppTemplate, credentials map[string]string, connectionID int64) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	// Notifications (no ID) — acknowledge with 202
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result any
	var rpcErr *jsonRPCError

	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":   map[string]any{"tools": map[string]any{}},
			"serverInfo":     map[string]string{"name": app.Slug + "-mcp", "version": "1.0.0"},
		}
		// Set session ID header per spec
		sessionID := generateID()
		w.Header().Set("Mcp-Session-Id", sessionID)

	case "tools/list":
		var tools []map[string]any
		for _, t := range app.Tools {
			schema := map[string]any{"type": "object"}
			if props, ok := t.InputSchema["properties"]; ok {
				schema["properties"] = props
			}
			if req, ok := t.InputSchema["required"]; ok {
				schema["required"] = req
			}
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": fmt.Sprintf("[%s] %s", app.Name, t.Description),
				"inputSchema": schema,
			})
		}
		result = map[string]any{"tools": tools}

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &params)

		// Find tool by name
		var tool *AppToolDef
		for i, t := range app.Tools {
			if app.Slug+"_"+t.Name == params.Name || t.Name == params.Name {
				tool = &app.Tools[i]
				break
			}
		}
		if tool == nil {
			rpcErr = &jsonRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)}
		} else {
			execResult, err := executeIntegrationTool(app, tool, credentials, params.Arguments)
			if err != nil {
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("error: %v", err)}},
					"isError": true,
				}
			} else {
				text, _ := json.Marshal(execResult.Data)
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(text)}},
					"isError": !execResult.Success,
				}
			}
		}

	default:
		rpcErr = &jsonRPCError{Code: -32601, Message: "method not found"}
	}

	// Send JSON-RPC response
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	json.NewEncoder(w).Encode(resp)
}
