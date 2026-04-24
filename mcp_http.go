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
// Endpoint: POST/GET /mcp/:id
//
// :id is interpreted as an mcp_servers.id FIRST (the row holds both the
// allowed_tools filter and the connection_id pointer). If that lookup
// misses, we fall back to interpreting :id as a connection_id and use
// the most recent mcp_servers row over that connection — this preserves
// backward compatibility with URLs emitted by older builds and gives
// existing single-MCP-per-connection setups zero-effort migration.
//
// POST: JSON-RPC request → JSON-RPC response (or SSE stream for streaming)
// GET: SSE stream for server-initiated messages (notifications)
//
// Per MCP spec 2025-03-26: single endpoint, POST for requests, optional SSE.
func (s *Server) handleMCPEndpoint(w http.ResponseWriter, r *http.Request) {
	// MCP endpoints are localhost-only (core connects from same machine)
	// No auth required — connection ID provides access scoping

	// Parse the id from /mcp/123
	idStr := strings.TrimPrefix(r.URL.Path, "/mcp/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Resolve to a connection + allowed_tools by trying mcp_servers.id
	// first, then falling back to connection_id (legacy URL format).
	var connectionID int64
	var allowedTools []string
	var mcpRowName string

	if srv, err := s.store.GetMCPServerByIDUnscoped(id); err == nil && srv != nil && srv.ConnectionID > 0 {
		connectionID = srv.ConnectionID
		allowedTools = srv.AllowedTools
		mcpRowName = srv.Name
	} else {
		// Legacy: id is a connection_id. Use the most recent mcp_servers
		// row over that connection for the allowed_tools filter.
		connectionID = id
		if srv, err := s.store.FindMCPServerByConnection(connectionID); err == nil && srv != nil {
			allowedTools = srv.AllowedTools
			mcpRowName = srv.Name
		}
	}

	// Load connection (no user auth check — MCP clients don't have session cookies).
	// We also fetch user_id so master-credential lookups stay scoped to
	// the owning user when this connection is a child of a suite master.
	var appSlug, encCreds string
	var connUserID int64
	err = s.store.db.QueryRow(
		"SELECT app_slug, encrypted_credentials, user_id FROM connections WHERE id = ?", connectionID,
	).Scan(&appSlug, &encCreds, &connUserID)
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
		s.handleMCPPost(w, r, app, credentials, connectionID, connUserID, allowedTools, mcpRowName)
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

func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request, app *AppTemplate, credentials map[string]string, connectionID int64, connUserID int64, allowedTools []string, mcpRowName string) {
	// Fast membership lookup for the tool filter. nil map = no filter.
	// Stored names may arrive in any of three forms depending on which
	// UI wrote them:
	//   - bare ("create_template")
	//   - app-slug-prefixed ("omnikit-messaging_create_template") — used by
	//     older pickers that always prefixed with app slug
	//   - mcp-row-name-prefixed ("socialcast-messaging_create_template") —
	//     used by handleMCPServerTools since the MCP row slug can now
	//     diverge from the app slug
	// Expand each stored entry into every recognised form and drop each
	// form into the set. This lets allowedSet[bareName] succeed in the
	// tools/list loop regardless of how the user's allowed_tools were
	// written.
	var allowedSet map[string]bool
	if len(allowedTools) > 0 {
		allowedSet = make(map[string]bool, len(allowedTools)*4)
		stripPrefixes := []string{app.Slug + "_"}
		if mcpRowName != "" && mcpRowName != app.Slug {
			stripPrefixes = append(stripPrefixes, mcpRowName+"_")
		}
		for _, name := range allowedTools {
			bare := name
			for _, p := range stripPrefixes {
				bare = strings.TrimPrefix(bare, p)
			}
			allowedSet[bare] = true
			allowedSet[app.Slug+"_"+bare] = true
			if mcpRowName != "" {
				allowedSet[mcpRowName+"_"+bare] = true
			}
			// Also keep the original stored form for good measure.
			allowedSet[name] = true
		}
	}
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
			// Apply allowed_tools filter. When the MCP server row has a
			// populated allowed_tools list, any tool not in the set is
			// hidden from the client entirely — it can't see it, can't
			// call it. Accepting both bare and slug-prefixed forms in
			// allowedSet above lets callers that stored prefixed names
			// (mcp_registry list output) and bare ones (native schema)
			// both work.
			if allowedSet != nil && !allowedSet[t.Name] {
				continue
			}
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

		// Enforce the allowed_tools filter at call time too — don't rely on
		// the client having hidden the tool in its UI. Reject with a clear
		// JSON-RPC error so the caller sees why.
		if allowedSet != nil && !allowedSet[params.Name] {
			rpcErr = &jsonRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("tool %q is not enabled on this connection (filtered by allowed_tools)", params.Name),
			}
			break
		}

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
			// Resolve master/child indirection + project binding
			// before dispatching. Passes user_id so master lookups are
			// tenant-scoped.
			ctx, err := s.resolveConnectionContext(connUserID, app, credentials, params.Arguments)
			if err != nil {
				rpcErr = &jsonRPCError{Code: -32603, Message: fmt.Sprintf("resolve context: %v", err)}
				break
			}
			persistTargetID := connectionID
			if ctx.MasterConnID != 0 {
				persistTargetID = ctx.MasterConnID
			}
			persist := func(updated map[string]string) error {
				blob, err := json.Marshal(updated)
				if err != nil {
					return err
				}
				enc, err := Encrypt(s.secret, string(blob))
				if err != nil {
					return err
				}
				return s.store.UpdateConnectionCredentials(persistTargetID, enc)
			}
			execResult, err := executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, persist)
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
