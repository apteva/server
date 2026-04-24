package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// runMCPProxy runs the server as a stdio MCP server for a specific connection.
// It translates MCP tools/list and tools/call into HTTP calls using stored credentials.
func runMCPProxy(dbPath string, connectionID int64, secret []byte) error {
	store, err := NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	// Load connection (we use user_id=0 trick — proxy is trusted, launched by server)
	var appSlug, encCreds string
	var connName string
	err = store.db.QueryRow(
		"SELECT app_slug, name, encrypted_credentials FROM connections WHERE id = ?", connectionID,
	).Scan(&appSlug, &connName, &encCreds)
	if err != nil {
		return fmt.Errorf("connection %d not found: %w", connectionID, err)
	}

	// Decrypt credentials
	plain, err := Decrypt(secret, encCreds)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	var credentials map[string]string
	json.Unmarshal([]byte(plain), &credentials)

	// Load app catalog
	appsDir := os.Getenv("APPS_DIR")
	if appsDir == "" {
		dataDir := os.Getenv("DATA_DIR")
		if dataDir == "" {
			dataDir = "data"
		}
		appsDir = dataDir + "/../../integrations/src/apps"
	}
	catalog := NewAppCatalog()
	catalog.LoadFromDir(appsDir)

	app := catalog.Get(appSlug)
	if app == nil {
		return fmt.Errorf("app %q not found in catalog", appSlug)
	}

	// Build tool list for MCP response
	type mcpInputSchema struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties,omitempty"`
		Required   []string       `json:"required,omitempty"`
	}

	type mcpTool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema mcpInputSchema `json:"inputSchema"`
	}

	var tools []mcpTool
	for _, t := range app.Tools {
		schema := mcpInputSchema{Type: "object"}
		if props, ok := t.InputSchema["properties"].(map[string]any); ok {
			schema.Properties = props
		}
		if req, ok := t.InputSchema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					schema.Required = append(schema.Required, s)
				}
			}
		}
		tools = append(tools, mcpTool{
			Name:        t.Name,
			Description: fmt.Sprintf("[%s] %s", app.Name, t.Description),
			InputSchema: schema,
		})
	}

	// toolsByName for quick lookup
	toolsByOrigName := map[string]*AppToolDef{}
	for i, t := range app.Tools {
		toolsByOrigName[t.Name] = &app.Tools[i]
	}

	// Stdio JSON-RPC server
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		// Notifications (no ID) — just acknowledge
		if req.ID == nil {
			continue
		}

		var result any
		var rpcErr *jsonRPCError

		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": appSlug + "-proxy", "version": "1.0.0"},
			}

		case "tools/list":
			result = map[string]any{"tools": tools}

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &params)

			tool := toolsByOrigName[params.Name]
			if tool == nil {
				rpcErr = &jsonRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)}
			} else {
				// Resolve master/child indirection + project binding.
				// Passes userID=0 so the resolver uses the trusted
				// direct-lookup path.
				ctx, err := resolveConnectionContextRaw(store, secret, 0, app, credentials, params.Arguments)
				if err != nil {
					rpcErr = &jsonRPCError{Code: -32603, Message: fmt.Sprintf("resolve context: %v", err)}
					break
				}
				persistTargetID := connectionID
				if ctx.MasterConnID != 0 {
					persistTargetID = ctx.MasterConnID
				}
				// Persist refreshed OAuth credentials back to the DB so they
				// survive the next subprocess restart. The credentials map
				// is mutated in place by the refresher; we just re-encrypt
				// and write the whole blob.
				persist := func(updated map[string]string) error {
					blob, err := json.Marshal(updated)
					if err != nil {
						return err
					}
					enc, err := Encrypt(secret, string(blob))
					if err != nil {
						return err
					}
					_, err = store.db.Exec(
						"UPDATE connections SET encrypted_credentials = ? WHERE id = ?",
						enc, persistTargetID,
					)
					return err
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

		// Send response
		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		data, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", data)
	}

	return nil
}
