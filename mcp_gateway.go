package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// runMCPGateway runs the server as a stdio MCP server exposing management tools.
func runMCPGateway(dbPath string, userID int64, secret []byte) error {
	store, err := NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	// Load app catalog
	appsDir := os.Getenv("APPS_DIR")
	if appsDir == "" {
		dataDir := os.Getenv("DATA_DIR")
		if dataDir == "" {
			dataDir = "data"
		}
		appsDir = filepath.Join(dataDir, "..", "..", "integrations", "src", "apps")
	}
	catalog := NewAppCatalog()
	catalog.LoadFromDir(appsDir)

	// Project scoping — instance's project_id is passed via env
	projectID := os.Getenv("PROJECT_ID")

	// The server binary path (for stdio server configs)
	selfPath, _ := os.Executable()
	_ = selfPath // used for stdio MCP servers

	// Tool definitions
	type toolParam struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	type toolSchema struct {
		Type       string                `json:"type"`
		Properties map[string]toolParam  `json:"properties,omitempty"`
		Required   []string              `json:"required,omitempty"`
	}
	type toolDef struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		InputSchema toolSchema `json:"inputSchema"`
	}

	tools := []toolDef{
		// Integrations
		{Name: "list_integrations", Description: "Browse available integrations. Returns name, slug, description, tool count.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"query": {Type: "string", Description: "Search query"}}}},
		{Name: "get_integration", Description: "Get full details of an integration including credential fields and tools.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"slug": {Type: "string", Description: "Integration slug"}}, Required: []string{"slug"}}},
		{Name: "list_connections", Description: "List active integration connections.", InputSchema: toolSchema{Type: "object"}},
		{Name: "create_connection", Description: "Create a new integration connection. Credentials are stored securely — after creating, use the returned connect_now instruction to access tools. NEVER pass API keys to threads or include them in messages/directives.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"slug": {Type: "string", Description: "Integration slug"}, "name": {Type: "string", Description: "Connection name"}, "credentials": {Type: "string", Description: "JSON string with credential fields matching the integration's auth config. Example: {\"api_key\": \"sk_...\"}"}}, Required: []string{"slug", "credentials"}}},
		{Name: "delete_connection", Description: "Delete an integration connection.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Connection ID"}}, Required: []string{"id"}}},
		// MCP Servers
		{Name: "list_mcp_servers", Description: "List registered MCP servers with status, tool count, and mcp_url. Use the mcp_url with [[connect]] to access the server's tools.", InputSchema: toolSchema{Type: "object"}},
		{Name: "create_mcp_server", Description: "Register a new custom MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"name": {Type: "string"}, "command": {Type: "string"}, "args": {Type: "string", Description: "Comma-separated arguments"}, "description": {Type: "string"}}, Required: []string{"name", "command"}}},
		{Name: "start_mcp_server", Description: "Start a registered MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "stop_mcp_server", Description: "Stop a running MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "delete_mcp_server", Description: "Delete an MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "list_server_tools", Description: "List tools from a running MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		// Subscriptions
		{Name: "list_subscribable", Description: "List connected integrations that support automatic webhook subscriptions.", InputSchema: toolSchema{Type: "object"}},
		{Name: "create_subscription", Description: "Subscribe to events from a connected integration. Auto-registers the webhook with the external service. Use list_subscribable to see available events. Set thread_id to deliver webhook events directly to a specific thread instead of main.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"connection_id": {Type: "string", Description: "Connection ID"}, "name": {Type: "string", Description: "Subscription name"}, "events": {Type: "string", Description: "Comma-separated event names from list_subscribable. Use EXACT event names (e.g. 'messaging.inbound_message_processed'). Do NOT invent event names."}, "thread_id": {Type: "string", Description: "Target thread ID for webhook events. Must be an already-running thread (spawn it first). If omitted, events go to main thread."}}, Required: []string{"connection_id"}}},
		{Name: "list_subscriptions", Description: "List active webhook subscriptions for this instance.", InputSchema: toolSchema{Type: "object"}},
		{Name: "delete_subscription", Description: "Remove a webhook subscription.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Subscription ID"}}, Required: []string{"id"}}},
		// Providers
		{Name: "list_providers", Description: "List active providers.", InputSchema: toolSchema{Type: "object"}},
		{Name: "activate_provider", Description: "Activate a provider.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"type": {Type: "string"}, "name": {Type: "string"}, "credentials": {Type: "string", Description: "JSON object of credentials (optional)"}}, Required: []string{"type", "name"}}},
		{Name: "deactivate_provider", Description: "Deactivate a provider.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Provider ID"}}, Required: []string{"id"}}},
	}

	// Handler dispatch
	handle := func(name string, args map[string]any) (any, error) {
		switch name {
		// --- Integrations ---
		case "list_integrations":
			q, _ := args["query"].(string)
			if q != "" {
				return catalog.Search(q), nil
			}
			return catalog.List(), nil

		case "get_integration":
			slug, _ := args["slug"].(string)
			app := catalog.Get(slug)
			if app == nil {
				return nil, fmt.Errorf("integration %q not found", slug)
			}
			return app, nil

		case "list_connections":
			conns, err := store.ListConnections(userID, projectID)
			if err != nil {
				return nil, err
			}
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			// Enrich with server config so core can connect directly
			type connWithServer struct {
				Connection
				ToolCount int            `json:"tool_count"`
				Server    map[string]any `json:"server"`
			}
			var result []connWithServer
			for _, c := range conns {
				tc := 0
				if app := catalog.Get(c.AppSlug); app != nil {
					tc = len(app.Tools)
				}
				result = append(result, connWithServer{
					Connection: c,
					ToolCount:  tc,
					Server: map[string]any{
						"name":      c.AppSlug,
						"transport": "http",
						"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, c.ID),
					},
				})
			}
			return result, nil

		case "create_connection":
			slug, _ := args["slug"].(string)
			connName, _ := args["name"].(string)

			app := catalog.Get(slug)
			if app == nil {
				return nil, fmt.Errorf("integration %q not found", slug)
			}
			if connName == "" {
				connName = app.Name
			}

			// Handle credentials as either JSON string or native object
			var creds map[string]string
			switch v := args["credentials"].(type) {
			case string:
				json.Unmarshal([]byte(v), &creds)
			case map[string]any:
				creds = make(map[string]string)
				for k, val := range v {
					creds[k] = fmt.Sprintf("%v", val)
				}
			}
			if creds == nil {
				// Build hint showing expected fields
				fields := []string{}
				for _, f := range app.Auth.CredentialFields {
					fields = append(fields, fmt.Sprintf("%q", f.Name))
				}
				return nil, fmt.Errorf("credentials must be a JSON object, e.g. {%s: \"value\"}", strings.Join(fields, ", "))
			}

			authType := "api_key"
			if len(app.Auth.Types) > 0 {
				authType = app.Auth.Types[0]
			}

			credsJSON, _ := json.Marshal(creds)
			encrypted, err := Encrypt(secret, string(credsJSON))
			if err != nil {
				return nil, fmt.Errorf("encryption failed: %w", err)
			}

			conn, err := store.CreateConnection(userID, slug, app.Name, connName, authType, encrypted, projectID)
			if err != nil {
				return nil, fmt.Errorf("create connection: %w", err)
			}

			store.CreateMCPServerFromConnection(userID, conn, len(app.Tools))

			// Return connection + server config for core to connect
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			mcpURL := fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, conn.ID)
			return map[string]any{
				"connection_id": conn.ID,
				"status":        "connected",
				"tools_count":   len(app.Tools),
				"connect_now":   fmt.Sprintf("Use [[connect name=\"%s\" url=\"%s\" transport=\"http\"]] to access the tools. Credentials are securely stored — NEVER pass API keys to threads or include them in directives.", slug, mcpURL),
			}, nil

		case "delete_connection":
			id, _ := parseIntArg(args["id"])
			store.DeleteMCPServerByConnection(id)
			store.DeleteConnection(userID, id)
			return map[string]string{"status": "deleted"}, nil

		// --- MCP Servers ---
		case "list_mcp_servers":
			servers, err := store.ListMCPServers(userID, projectID)
			if err != nil {
				return nil, err
			}
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			type serverWithURL struct {
				MCPServerRecord
				MCPURL string `json:"mcp_url,omitempty"`
			}
			var result []serverWithURL
			for _, s := range servers {
				sw := serverWithURL{MCPServerRecord: s}
				if s.Source == "local" && s.ConnectionID > 0 {
					sw.MCPURL = fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, s.ConnectionID)
				}
				result = append(result, sw)
			}
			return result, nil

		case "create_mcp_server":
			name, _ := args["name"].(string)
			command, _ := args["command"].(string)
			argsStr, _ := args["args"].(string)
			desc, _ := args["description"].(string)

			var mcpArgs []string
			if argsStr != "" {
				for _, a := range splitArgs(argsStr) {
					mcpArgs = append(mcpArgs, a)
				}
			}
			argsJSON, _ := json.Marshal(mcpArgs)

			return store.CreateMCPServer(userID, name, command, string(argsJSON), "", desc)

		case "start_mcp_server":
			id, _ := parseIntArg(args["id"])
			record, encEnv, err := store.GetMCPServer(userID, id)
			if err != nil {
				return nil, fmt.Errorf("server not found")
			}
			env := map[string]string{}
			if encEnv != "" {
				if plain, err := Decrypt(secret, encEnv); err == nil {
					json.Unmarshal([]byte(plain), &env)
				}
			}
			mcpMgr := NewMCPManager()
			proc, err := mcpMgr.Start(record, env)
			if err != nil {
				return nil, err
			}
			store.UpdateMCPServerStatus(id, "running", len(proc.Tools), proc.cmd.Process.Pid)
			return map[string]any{"status": "running", "tool_count": len(proc.Tools)}, nil

		case "stop_mcp_server":
			id, _ := parseIntArg(args["id"])
			store.UpdateMCPServerStatus(id, "stopped", 0, 0)
			return map[string]string{"status": "stopped"}, nil

		case "delete_mcp_server":
			id, _ := parseIntArg(args["id"])
			store.DeleteMCPServer(userID, id)
			return map[string]string{"status": "deleted"}, nil

		case "list_server_tools":
			id, _ := parseIntArg(args["id"])
			// For local integrations, get tools from catalog
			var connID int64
			store.db.QueryRow("SELECT connection_id FROM mcp_servers WHERE id = ? AND user_id = ?", id, userID).Scan(&connID)
			if connID > 0 {
				conn, _, err := store.GetConnection(userID, connID)
				if err == nil {
					if app := catalog.Get(conn.AppSlug); app != nil {
						var toolList []map[string]string
						for _, t := range app.Tools {
							toolList = append(toolList, map[string]string{
								"name":        conn.AppSlug + "_" + t.Name,
								"description": t.Description,
								"method":      t.Method,
							})
						}
						return toolList, nil
					}
				}
			}
			return []any{}, nil

		// --- Subscriptions ---
		case "list_subscribable":
			conns, err := store.ListConnections(userID, projectID)
			if err != nil {
				return nil, err
			}
			type subscribableConn struct {
				ConnectionID int64             `json:"connection_id"`
				AppSlug      string            `json:"app_slug"`
				AppName      string            `json:"app_name"`
				Events       []AppWebhookEvent `json:"events,omitempty"`
			}
			var result []subscribableConn
			for _, c := range conns {
				if app := catalog.Get(c.AppSlug); app != nil {
					if app.Webhooks != nil && len(app.Webhooks.Events) > 0 {
						result = append(result, subscribableConn{ConnectionID: c.ID, AppSlug: c.AppSlug, AppName: c.AppName, Events: app.Webhooks.Events})
					}
				}
			}
			return result, nil

		case "create_subscription":
			connIDRaw, _ := parseIntArg(args["connection_id"])
			subName, _ := args["name"].(string)
			eventsStr, _ := args["events"].(string)
			threadID, _ := args["thread_id"].(string)
			var eventsList []string
			if eventsStr != "" {
				for _, e := range strings.Split(eventsStr, ",") {
					if t := strings.TrimSpace(e); t != "" {
						eventsList = append(eventsList, t)
					}
				}
			}

			conn, encCreds, err := store.GetConnection(userID, connIDRaw)
			if err != nil {
				return nil, fmt.Errorf("connection not found")
			}

			app := catalog.Get(conn.AppSlug)
			if app == nil {
				return nil, fmt.Errorf("app %q not found", conn.AppSlug)
			}

			// Validate event names against the app's webhook events
			if len(eventsList) > 0 && app.Webhooks != nil {
				validEvents := map[string]bool{}
				for _, e := range app.Webhooks.Events {
					validEvents[e.Name] = true
				}
				var invalid []string
				for _, e := range eventsList {
					if !validEvents[e] {
						invalid = append(invalid, e)
					}
				}
				if len(invalid) > 0 {
					var validNames []string
					for _, e := range app.Webhooks.Events {
						validNames = append(validNames, e.Name)
					}
					return nil, fmt.Errorf("invalid event names: %v. Valid events: %v", invalid, validNames)
				}
			}

			if subName == "" {
				subName = conn.AppName + " webhooks"
			}

			// Get instance ID from env (this gateway runs for a specific instance)
			instanceID := int64(0)
			if id := os.Getenv("INSTANCE_ID"); id != "" {
				fmt.Sscanf(id, "%d", &instanceID)
			}

			webhookPath := generateToken(16)
			publicURL := os.Getenv("PUBLIC_URL")
			var webhookURL string
			if publicURL != "" {
				webhookURL = strings.TrimSuffix(publicURL, "/") + "/webhooks/" + webhookPath
			} else {
				serverPort := os.Getenv("PORT")
				if serverPort == "" {
					serverPort = "8080"
				}
				webhookURL = fmt.Sprintf("http://127.0.0.1:%s/webhooks/%s", serverPort, webhookPath)
			}

			sub, err := store.CreateSubscription(userID, instanceID, conn.ID, subName, conn.AppSlug, "", webhookPath, "", threadID)
			if err != nil {
				return nil, fmt.Errorf("create subscription: %w", err)
			}

			// Auto-register webhook with external service using webhooks.registration config
			autoRegistered := false
			if app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.ManualSetup == "" {
				plain, err := Decrypt(secret, encCreds)
				if err != nil {
					log.Printf("[WEBHOOK-REG] decrypt error: %v", err)
				} else {
					reg := app.Webhooks.Registration
					headers := map[string]string{"Content-Type": "application/json"}
					for k, v := range app.Auth.Headers {
						headers[k] = resolveCredTemplate(v, plain)
					}

					reqBody := map[string]any{}
					if reg.Extra != nil {
						for k, v := range reg.Extra {
							reqBody[k] = v
						}
					}
					setField(reqBody, reg.URLField, webhookURL)
					if reg.EventsField != "" && len(eventsList) > 0 {
						setField(reqBody, reg.EventsField, eventsList)
					}

					regURL := strings.TrimSuffix(app.BaseURL, "/") + reg.Path
					regBodyJSON, _ := json.Marshal(reqBody)
					req, err := http.NewRequest(reg.Method, regURL, strings.NewReader(string(regBodyJSON)))
					if err == nil {
						for k, v := range headers {
							req.Header.Set(k, v)
						}
						resp, err := http.DefaultClient.Do(req)
						if err != nil {
							log.Printf("[WEBHOOK-REG] error: %v", err)
						} else {
							respBody, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							if resp.StatusCode >= 200 && resp.StatusCode < 300 {
								autoRegistered = true
								if reg.IDField != "" {
									var respData map[string]any
									if json.Unmarshal(respBody, &respData) == nil {
										extID := extractJSONPath(respData, reg.IDField)
										if extID != "" {
											store.SetSubscriptionExternalID(sub.ID, extID)
										}
									}
								}
							} else {
								log.Printf("[WEBHOOK-REG] failed %d: %s", resp.StatusCode, string(respBody))
							}
						}
					}
				}
			} else {
			}

			return map[string]any{
				"id":              sub.ID,
				"webhook_url":     webhookURL,
				"events":          eventsList,
				"auto_registered": autoRegistered,
			}, nil

		case "list_subscriptions":
			subs, err := store.ListSubscriptions(userID)
			if err != nil {
				return nil, err
			}
			return subs, nil

		case "delete_subscription":
			id, _ := args["id"].(string)
			store.DeleteSubscription(userID, id)
			return map[string]string{"status": "deleted"}, nil

		// --- Providers ---
		case "list_providers":
			return store.ListProviders(userID)

		case "activate_provider":
			ptype, _ := args["type"].(string)
			pname, _ := args["name"].(string)

			data := map[string]string{}
			switch v := args["credentials"].(type) {
			case string:
				if v != "" {
					json.Unmarshal([]byte(v), &data)
				}
			case map[string]any:
				for k, val := range v {
					data[k] = fmt.Sprintf("%v", val)
				}
			}
			if len(data) == 0 {
				data = map[string]string{"_enabled": "true"}
			}

			dataJSON, _ := json.Marshal(data)
			encrypted, _ := Encrypt(secret, string(dataJSON))
			return store.CreateProvider(userID, 0, ptype, pname, encrypted)

		case "deactivate_provider":
			id, _ := parseIntArg(args["id"])
			store.DeleteProvider(userID, id)
			return map[string]string{"status": "deleted"}, nil

		default:
			return nil, fmt.Errorf("unknown tool %q", name)
		}
	}

	// Stdio JSON-RPC server loop
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

		if req.ID == nil {
			continue // notification
		}

		var result any
		var rpcErr *jsonRPCError

		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": "apteva-gateway", "version": "1.0.0"},
			}

		case "tools/list":
			result = map[string]any{"tools": tools}

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &params)

			res, err := handle(params.Name, params.Arguments)
			if err != nil {
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("error: %v", err)}},
					"isError": true,
				}
			} else {
				text, _ := json.MarshalIndent(res, "", "  ")
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(text)}},
					"isError": false,
				}
			}

		default:
			rpcErr = &jsonRPCError{Code: -32601, Message: "method not found"}
		}

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

func parseIntArg(v any) (int64, error) {
	switch val := v.(type) {
	case string:
		return strconv.ParseInt(val, 10, 64)
	case float64:
		return int64(val), nil
	default:
		return 0, fmt.Errorf("invalid ID")
	}
}

func splitArgs(s string) []string {
	var args []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			args = append(args, a)
		}
	}
	return args
}
