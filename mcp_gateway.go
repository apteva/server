package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

	serverAPI := newGatewayAPIClient(userID)

	// The server binary path (for stdio server configs)
	selfPath, _ := os.Executable()
	_ = selfPath // used for stdio MCP servers

	// Tool definitions
	type toolParam struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	type toolSchema struct {
		Type       string               `json:"type"`
		Properties map[string]toolParam `json:"properties,omitempty"`
		Required   []string             `json:"required,omitempty"`
	}
	type toolDef struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		InputSchema toolSchema `json:"inputSchema"`
	}

	tools := []toolDef{
		// Agents
		{Name: "agents_list", Description: "List Apteva agents visible to this user. Defaults to the current project when the gateway was launched for one.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}}}},
		{Name: "agents_get", Description: "Get one Apteva agent by ID, including current running/stopped status.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Agent ID"}}, Required: []string{"id"}}},
		{Name: "agents_create", Description: "Create an Apteva agent using the same server path as the dashboard. Provide a clear name and directive. Prefer structured markdown headings such as # Role, # Goals, # Operating Rules, # Tools and Integrations, # Schedule, # Escalation and Safety, # Tone, and # Learning. By default the new agent starts immediately and receives only the channel MCP, so it can reply in chat but cannot manage Apteva itself.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"name": {Type: "string", Description: "Agent name"}, "directive": {Type: "string", Description: "Agent directive / system instructions. Prefer structured markdown with stable sections."}, "mode": {Type: "string", Description: "autonomous, cautious, or learn. Defaults to autonomous."}, "project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}, "start": {Type: "string", Description: "true/false. Defaults to true."}, "include_channels": {Type: "string", Description: "true/false. Defaults to true."}, "unconscious": {Type: "string", Description: "true/false. Optional background memory setting."}, "config": {Type: "string", Description: "Optional JSON object or JSON string for advanced agent config."}, "bound_app_install_ids": {Type: "string", Description: "Optional comma-separated installed app IDs to bind to the new agent."}, "bound_connection_ids": {Type: "string", Description: "Optional comma-separated integration connection IDs to attach as MCP servers."}}, Required: []string{"name", "directive"}}},
		{Name: "agents_update", Description: "Update an Apteva agent using the normal dashboard/server handlers. Supports rename, full directive/mode/config updates, markdown directive section edits, and MCP server attachment changes via mcp_server_ids from list_mcp_servers. For empty directives, prefer directive_section/directive_content edits so the directive starts as structured Markdown instead of plain text.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Agent ID"}, "name": {Type: "string", Description: "New display name"}, "directive": {Type: "string", Description: "New full directive. Use only when intentionally replacing the whole directive; prefer section edits for structured Markdown."}, "directive_edit_mode": {Type: "string", Description: "Optional section edit mode: section_append, section_replace, section_replace_line, or section_remove_line. Defaults to section_append when directive_section is provided. Ignored when directive is provided."}, "directive_section": {Type: "string", Description: "Markdown section name to edit, e.g. Learning or Tools and Integrations. With directive_content and no directive_edit_mode, creates/appends this section."}, "directive_match": {Type: "string", Description: "Line substring to match for section_replace_line or section_remove_line."}, "directive_content": {Type: "string", Description: "Content to append, replace, or use as the replacement line."}, "directive_edits": {Type: "string", Description: "Optional JSON array of section edits with mode, section, match, and content fields. Use this to initialize or update several Markdown sections at once."}, "mode": {Type: "string", Description: "autonomous, cautious, or learn"}, "config": {Type: "string", Description: "Optional JSON object or JSON string for advanced agent config"}, "mcp_server_ids": {Type: "string", Description: "Optional comma-separated MCP server IDs from list_mcp_servers"}, "mcp_action": {Type: "string", Description: "set, add, or remove MCP servers. Defaults to set when mcp_server_ids is provided."}}, Required: []string{"id"}}},
		{Name: "agents_start", Description: "Start a stopped Apteva agent using the server lifecycle handler.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Agent ID"}}, Required: []string{"id"}}},
		{Name: "agents_stop", Description: "Stop a running Apteva agent using the server lifecycle handler.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Agent ID"}}, Required: []string{"id"}}},
		{Name: "agents_delete", Description: "Delete an Apteva agent using the same server cleanup path as the dashboard.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Agent ID"}}, Required: []string{"id"}}},
		{Name: "agent_list_activity", Description: "List recent agent activity actions from stored telemetry. Returns merged thought, tool, thread, event, and error rows; chat reply actions are omitted. Use include_payloads=true for built thought text and tool args/results, include_raw=true only for debugging.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}, "agent_id": {Type: "string", Description: "Optional agent ID. Omit to list activity for all agents in the project."}, "thread_id": {Type: "string", Description: "Optional thread ID filter, e.g. main."}, "kind": {Type: "string", Description: "Optional filter: all, thought, tool, thread, event, or error."}, "status": {Type: "string", Description: "Optional filter: all, running, success, error, or info."}, "period": {Type: "string", Description: "Lookback window such as 1h, 24h, 7d, 30d, or a Go duration like 15m. Defaults to 24h."}, "since": {Type: "string", Description: "Optional RFC3339 timestamp; overrides period."}, "limit": {Type: "string", Description: "Maximum action rows to return, up to 320. Defaults to 100."}, "query": {Type: "string", Description: "Optional text search across agent, thread, title, detail, and included payloads."}, "include_payloads": {Type: "string", Description: "true/false. When true, includes built thought text and tool args/results. Defaults to false."}, "include_raw": {Type: "string", Description: "true/false. Include raw telemetry events used to build each row. Defaults to false."}}}},
		// Apps
		{Name: "apps_list", Description: "List installed Apteva apps visible in a project. Defaults to the current project and includes global installs.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}}}},
		{Name: "apps_marketplace", Description: "List marketplace apps, marking which are installed in the current project. Defaults to the current project and includes global installs.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}, "registry_url": {Type: "string", Description: "Optional registry URL override."}}}},
		{Name: "apps_install", Description: "Install an Apteva app using the same server path as the dashboard. Defaults to the current project. To install globally, pass global=true explicitly.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"manifest_url": {Type: "string", Description: "Manifest URL to install."}, "manifest_yaml": {Type: "string", Description: "Inline manifest YAML."}, "repo": {Type: "string", Description: "Optional source repo metadata."}, "ref": {Type: "string", Description: "Optional source ref metadata."}, "project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}, "global": {Type: "string", Description: "true/false. Required true for a global install when no project_id/current project is available."}, "config": {Type: "string", Description: "Optional JSON object or JSON string with app config."}, "upgrade_policy": {Type: "string", Description: "manual, auto-patch, or auto-minor."}, "bindings": {Type: "string", Description: "Optional JSON object mapping required roles to connection/install IDs."}}}},
		{Name: "apps_upgrade", Description: "Upgrade an installed Apteva app using the same server path as the dashboard.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"install_id": {Type: "string", Description: "App install ID"}, "approve_new_permissions": {Type: "string", Description: "true/false. Confirms new permissions shown to the operator."}}, Required: []string{"install_id"}}},
		{Name: "apps_uninstall", Description: "Uninstall an Apteva app from the explicitly named project using the same server cleanup path as the dashboard. Returns an authoritative receipt with app_name, display_name, install_id, app_id, project_id, version, and status. Treat a successful receipt as final; do not re-list apps to reinterpret whether the uninstall happened.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"install_id": {Type: "string", Description: "App install ID"}, "project_id": {Type: "string", Description: "Apteva project ID that owns this install. Must match the current dashboard project."}, "force": {Type: "string", Description: "true/false. Override dependency blockers when intentionally removing anyway."}}, Required: []string{"install_id", "project_id"}}},
		// Integrations
		{Name: "list_integrations", Description: "Browse available integrations. Returns name, slug, description, tool count.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"query": {Type: "string", Description: "Search query"}}}},
		{Name: "get_integration", Description: "Get full details of an integration including credential fields and tools.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"slug": {Type: "string", Description: "Integration slug"}}, Required: []string{"slug"}}},
		{Name: "list_connections", Description: "List active integration connections.", InputSchema: toolSchema{Type: "object"}},
		{Name: "create_connection", Description: "Create a new integration connection. Credentials are stored securely — after creating, use the returned connect_now instruction to access tools. NEVER pass API keys to threads or include them in messages/directives. Pass allowed_tools to scope the resulting MCP server row to a subset of the integration's tools (least-privilege). Omit or pass empty for all tools.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"slug": {Type: "string", Description: "Integration slug"}, "name": {Type: "string", Description: "Connection name"}, "credentials": {Type: "string", Description: "JSON string with credential fields matching the integration's auth config. Example: {\"api_key\": \"sk_...\"}"}, "allowed_tools": {Type: "string", Description: "Comma-separated list of tool names to expose. Leave empty to expose all tools. Use list_integrations + get_integration to see the full set before picking."}}, Required: []string{"slug", "credentials"}}},
		{Name: "delete_connection", Description: "Delete an integration connection.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Connection ID"}}, Required: []string{"id"}}},
		{Name: "create_mcp_server_from_connection", Description: "Create a second MCP server row over an existing connection with a different tool scope. Lets a team give some workers a read-only surface while others see the full tool set over the same credentials.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"connection_id": {Type: "string", Description: "Connection ID to attach to"}, "name": {Type: "string", Description: "Friendly name for this scoped server (e.g. \"sheets-readonly\")"}, "allowed_tools": {Type: "string", Description: "Comma-separated list of tool names this view exposes. Required — use list_integrations/get_integration to pick."}}, Required: []string{"connection_id", "allowed_tools"}}},
		{Name: "update_mcp_server_tools", Description: "Change the allowed_tools filter on an existing MCP server row. Pass an empty string to clear the filter (all tools re-enabled). Takes effect immediately for local MCP servers; remote (Composio) rows require a reconcile to propagate.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "MCP server ID"}, "allowed_tools": {Type: "string", Description: "Comma-separated tool names (empty = all)"}}, Required: []string{"id"}}},
		// MCP Servers
		{Name: "list_mcp_servers", Description: "List registered MCP servers with status, tool count, kind, source, and connection/app ownership metadata. Use kind=app to list only app MCP servers, kind=integration for integration MCP servers, kind=custom for manually registered MCP servers, or kind=remote for hosted MCP servers. Use mcp_url or proxy_config to connect to tools.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"project_id": {Type: "string", Description: "Optional Apteva project ID. Defaults to the current project."}, "kind": {Type: "string", Description: "Optional filter: app, integration, custom, remote, or all."}, "include_app_owned": {Type: "string", Description: "true/false. When false, hides app-owned MCP rows from unfiltered results. Defaults to true for backward compatibility."}}}},
		{Name: "create_mcp_server", Description: "Register a new custom MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"name": {Type: "string"}, "command": {Type: "string"}, "args": {Type: "string", Description: "Comma-separated arguments"}, "description": {Type: "string"}}, Required: []string{"name", "command"}}},
		{Name: "start_mcp_server", Description: "Start a registered MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "stop_mcp_server", Description: "Stop a running MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "delete_mcp_server", Description: "Delete an MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		{Name: "list_server_tools", Description: "List tools from a running MCP server.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Server ID"}}, Required: []string{"id"}}},
		// Subscriptions
		{Name: "list_subscribable", Description: "List connected integrations that support native or poll-backed webhook event subscriptions.", InputSchema: toolSchema{Type: "object"}},
		{Name: "create_subscription", Description: "Subscribe to events from a connected integration. Native events auto-register a webhook with the external service; poll-backed events are refreshed by Apteva. Use list_subscribable to see available events. Set thread_id to deliver webhook events directly to a specific thread instead of main.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"connection_id": {Type: "string", Description: "Connection ID"}, "name": {Type: "string", Description: "Subscription name"}, "events": {Type: "string", Description: "Comma-separated event names from list_subscribable. Use EXACT event names (e.g. 'messaging.inbound_message_processed'). Do NOT invent event names."}, "thread_id": {Type: "string", Description: "Target thread ID for webhook events. Must be an already-running thread (spawn it first). If omitted, events go to main thread."}, "interval_seconds": {Type: "string", Description: "Optional poll interval in seconds for poll-backed events."}, "poll_input": {Type: "string", Description: "Optional JSON object merged into the poll tool input."}}, Required: []string{"connection_id"}}},
		{Name: "list_subscriptions", Description: "List active webhook subscriptions for this instance.", InputSchema: toolSchema{Type: "object"}},
		{Name: "delete_subscription", Description: "Remove a webhook subscription.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Subscription ID"}}, Required: []string{"id"}}},
		// Providers
		{Name: "list_providers", Description: "List active providers.", InputSchema: toolSchema{Type: "object"}},
		{Name: "activate_provider", Description: "Activate a provider.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"type": {Type: "string"}, "name": {Type: "string"}, "credentials": {Type: "string", Description: "JSON object of credentials (optional)"}}, Required: []string{"type", "name"}}},
		{Name: "deactivate_provider", Description: "Deactivate a provider.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"id": {Type: "string", Description: "Provider ID"}}, Required: []string{"id"}}},
		// Credential-group suites (OmniKit, SocialCast, ...)
		{Name: "list_credential_groups", Description: "List integration suites (groups of apps that share one credential). Members, account/project scope support, and display metadata.", InputSchema: toolSchema{Type: "object"}},
		{Name: "add_account_credential", Description: "Add an account-wide credential for a suite, run project discovery, and cache the discovered project list. Use for OmniKit/SocialCast-style suites where one key unlocks many sub-services across many projects. NEVER echo the key in responses.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"group_id": {Type: "string", Description: "Suite id (e.g. omnikit, socialcast)"}, "credentials": {Type: "string", Description: "JSON object matching the group's account-scope credential_fields. Example: {\"api_key\":\"okt_acc_...\"}"}}, Required: []string{"group_id", "credentials"}}},
		{Name: "list_group_projects", Description: "List the projects discovered for a suite's account credential. Returns cached values; call refresh_group_projects to re-query upstream.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"group_id": {Type: "string", Description: "Suite id"}}, Required: []string{"group_id"}}},
		{Name: "refresh_group_projects", Description: "Re-run discovery for a suite's account credential, picking up any new projects on the remote side.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"group_id": {Type: "string"}}, Required: []string{"group_id"}}},
		{Name: "enable_apps_for_projects", Description: "Fan out a suite credential into project-scoped connections. selections is a JSON array of { app_slug, external_project_id, label }. One child connection per pair; idempotent on re-run.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"group_id": {Type: "string"}, "selections": {Type: "string", Description: "JSON array of { app_slug, external_project_id, label } objects"}, "replace": {Type: "string", Description: "'true' to remove child connections not in the new selection; defaults to false"}}, Required: []string{"group_id", "selections"}}},
		{Name: "delete_group_credential", Description: "Remove a suite's account credential and every child connection fanned out from it. Use with care — equivalent to the dashboard's 'Disconnect all'.", InputSchema: toolSchema{Type: "object", Properties: map[string]toolParam{"group_id": {Type: "string"}}, Required: []string{"group_id"}}},
	}

	// Handler dispatch
	handle := func(name string, args map[string]any) (any, error) {
		if strings.HasPrefix(name, "agents_") || name == "agent_list_activity" {
			return handleGatewayAgentTool(name, args, projectID, serverAPI, store, selfPath)
		}
		if strings.HasPrefix(name, "apps_") {
			return handleGatewayAppTool(name, args, projectID, serverAPI)
		}
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
				// One entry per CONNECTION (not per scoped MCP view).
				// The URL uses connection id which the HTTP endpoint
				// handles via legacy fallback (most-recent server for
				// this connection). Agents that need the list of every
				// scoped view should call list_mcp_servers instead.
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

			// Optional allowed_tools filter — scopes the resulting MCP server
			// row to a subset of the integration's tools. Agents should
			// prefer this over "all tools" to stay least-privilege.
			allowedTools := parseCSV(args["allowed_tools"])
			// Validate requested tools against the app's actual tool set —
			// otherwise a typo silently drops to "no tools exposed" and the
			// agent gets a confusing 0-tool MCP.
			if len(allowedTools) > 0 {
				valid := map[string]bool{}
				for _, t := range app.Tools {
					valid[t.Name] = true
					valid[slug+"_"+t.Name] = true
				}
				var bad []string
				for _, name := range allowedTools {
					if !valid[name] {
						bad = append(bad, name)
					}
				}
				if len(bad) > 0 {
					return nil, fmt.Errorf("unknown tool name(s) for %s: %s — call get_integration(slug=%q) to see the full list", slug, strings.Join(bad, ", "), slug)
				}
			}
			toolCount := len(app.Tools)
			if len(allowedTools) > 0 {
				toolCount = len(allowedTools)
			}
			srvID, _ := store.CreateMCPServerFromConnection(userID, conn, toolCount, allowedTools)

			// Return connection + server config for core to connect.
			// URL is keyed on the mcp_servers row id (not the connection
			// id) so this row gets a unique URL even if the user later
			// creates additional scoped views over the same connection.
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			mcpURL := fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, srvID)
			return map[string]any{
				"connection_id": conn.ID,
				"status":        "connected",
				"tools_count":   toolCount,
				"allowed_tools": allowedTools,
				"connect_now":   fmt.Sprintf("Use [[connect name=\"%s\" url=\"%s\" transport=\"http\"]] to access the tools. Credentials are securely stored — NEVER pass API keys to threads or include them in directives.", slug, mcpURL),
			}, nil

		case "create_mcp_server_from_connection":
			connID, _ := parseIntArg(args["connection_id"])
			scopedName, _ := args["name"].(string)
			allowedTools := parseCSV(args["allowed_tools"])
			if len(allowedTools) == 0 {
				return nil, fmt.Errorf("allowed_tools is required — this tool exists to make a narrower view of an existing connection. Use list_mcp_servers if you want the default full-tool view.")
			}
			conn, _, err := store.GetConnection(userID, connID)
			if err != nil {
				return nil, fmt.Errorf("connection %d not found", connID)
			}
			app := catalog.Get(conn.AppSlug)
			if app == nil {
				return nil, fmt.Errorf("app %q not found in catalog", conn.AppSlug)
			}
			// Validate tool names against the app catalog. Accept bare,
			// canonical-MCP-prefixed, and legacy app-slug-prefixed forms.
			canonPrefix := store.CanonicalMCPNameForConnection(conn.ID)
			valid := map[string]bool{}
			for _, t := range app.Tools {
				valid[t.Name] = true
				valid[canonPrefix+"_"+t.Name] = true
				valid[conn.AppSlug+"_"+t.Name] = true
			}
			var bad []string
			for _, name := range allowedTools {
				if !valid[name] {
					bad = append(bad, name)
				}
			}
			if len(bad) > 0 {
				return nil, fmt.Errorf("unknown tool name(s): %s", strings.Join(bad, ", "))
			}
			if scopedName == "" {
				scopedName = fmt.Sprintf("%s-scoped-%d", conn.AppSlug, len(allowedTools))
			}
			row, err := store.CreateMCPServerExt(MCPServerInput{
				UserID:       userID,
				Name:         scopedName,
				Description:  fmt.Sprintf("Scoped view of %s — %d tools", conn.AppName, len(allowedTools)),
				Source:       "local",
				Transport:    "http",
				ConnectionID: conn.ID,
				ProjectID:    conn.ProjectID,
				AllowedTools: allowedTools,
			})
			if err != nil {
				return nil, fmt.Errorf("create scoped mcp_server: %w", err)
			}
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			// URL is keyed on the mcp_servers row id (not the
			// connection id) so two scoped views over the same
			// connection get distinct URLs. The HTTP endpoint at
			// /mcp/{id} resolves the row → connection + allowed_tools.
			return map[string]any{
				"id":            row.ID,
				"name":          row.Name,
				"connection_id": conn.ID,
				"allowed_tools": allowedTools,
				"url":           fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, row.ID),
			}, nil

		case "update_mcp_server_tools":
			serverID, _ := parseIntArg(args["id"])
			allowedTools := parseCSV(args["allowed_tools"])
			if err := store.UpdateMCPServerAllowedTools(userID, serverID, allowedTools); err != nil {
				return nil, err
			}
			return map[string]any{
				"id":            serverID,
				"allowed_tools": allowedTools,
				"note":          "Local MCP servers take effect immediately. Composio (remote) servers need a reconcile to propagate — call composio reconcile from the dashboard or restart the instance.",
			}, nil

		case "delete_connection":
			id, _ := parseIntArg(args["id"])
			store.DeleteMCPServerByConnection(id)
			store.DeleteConnection(userID, id)
			return map[string]string{"status": "deleted"}, nil

		// --- MCP Servers ---
		case "list_mcp_servers":
			serverPort := os.Getenv("PORT")
			if serverPort == "" {
				serverPort = "8080"
			}
			return listGatewayMCPServers(store, userID, projectID, args, serverPort, selfPath)

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
						prefix := store.CanonicalMCPNameForConnection(conn.ID)
						var toolList []map[string]string
						for _, t := range app.Tools {
							toolList = append(toolList, map[string]string{
								"name":        prefix + "_" + t.Name,
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
			intervalSeconds, _ := parseIntArg(args["interval_seconds"])
			pollInput := map[string]any{}
			switch v := args["poll_input"].(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					if err := json.Unmarshal([]byte(v), &pollInput); err != nil {
						return nil, fmt.Errorf("poll_input must be a JSON object")
					}
				}
			case map[string]any:
				pollInput = v
			}
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

			if cfg, event, err := buildStoredPollConfig(app, eventsList, int(intervalSeconds), pollInput); err != nil {
				return nil, err
			} else if cfg != nil && event != nil {
				if len(eventsList) == 0 {
					eventsList = []string{event.Name}
				}
				cfgJSON, _ := json.Marshal(cfg)
				sub, err := store.CreatePollSubscription(userID, instanceID, conn.ID, subName, conn.AppSlug, event.Description, threadID, conn.ProjectID, eventsList, string(cfgJSON), time.Now().UTC())
				if err != nil {
					return nil, fmt.Errorf("create poll subscription: %w", err)
				}
				return map[string]any{
					"id":               sub.ID,
					"delivery":         "poll",
					"events":           eventsList,
					"auto_registered":  false,
					"interval_seconds": cfg.IntervalSeconds,
				}, nil
			}

			webhookPath := generateToken(16)
			// Resolve public base URL the same way the parent server does:
			// server_settings.public_url > PUBLIC_URL env > localhost. The
			// mcp-gateway runs as a subprocess with its own DB handle so we
			// query the settings table directly here.
			publicURL := store.GetSetting("public_url")
			if publicURL == "" {
				publicURL = os.Getenv("PUBLIC_URL")
			}
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

			sub, err := store.CreateSubscription(userID, instanceID, conn.ID, subName, conn.AppSlug, "", webhookPath, "", threadID, conn.ProjectID, eventsList)
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
			// Scope to the instance's project so agents only see what's
			// relevant to their workspace. ListSubscriptions already
			// includes legacy unscoped rows (project_id = '') in its
			// OR clause so older subs still surface.
			subs, err := store.ListSubscriptions(userID, projectID)
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

		// --- Credential-group (suite) management ---
		case "list_credential_groups":
			return catalog.ListGroups(), nil

		case "add_account_credential":
			groupID, _ := args["group_id"].(string)
			if groupID == "" {
				return nil, fmt.Errorf("group_id required")
			}
			g := catalog.GetGroup(groupID)
			if g == nil {
				return nil, fmt.Errorf("group %q not found", groupID)
			}
			app := catalog.Get(g.Members[0])
			if app == nil {
				return nil, fmt.Errorf("group %q has no resolvable members", groupID)
			}
			credsJSON, _ := args["credentials"].(string)
			var creds map[string]string
			if err := json.Unmarshal([]byte(credsJSON), &creds); err != nil {
				return nil, fmt.Errorf("credentials must be JSON object: %v", err)
			}
			projects, err := discoverProjects(app, &g.Meta, creds)
			if err != nil {
				return nil, err
			}
			blob := map[string]string{credKeyType: "master", credKeyGroup: groupID, credKeyScope: "account"}
			for k, v := range creds {
				blob[k] = v
			}
			cacheBytes, _ := json.Marshal(projects)
			blob[credKeyProjectsCache] = string(cacheBytes)
			encoded, _ := json.Marshal(blob)
			enc, err := Encrypt(secret, string(encoded))
			if err != nil {
				return nil, err
			}
			// Upsert
			existingID := int64(0)
			all, _ := store.ListConnections(userID, projectID)
			for _, c := range all {
				if c.AppSlug == MasterSlug(groupID) {
					existingID = c.ID
					break
				}
			}
			if existingID != 0 {
				if err := store.UpdateConnectionCredentials(existingID, enc); err != nil {
					return nil, err
				}
				return map[string]any{"master_id": existingID, "projects": projects, "updated": true}, nil
			}
			conn, err := store.CreateConnectionExt(ConnectionInput{
				UserID: userID, AppSlug: MasterSlug(groupID), AppName: g.Meta.Name,
				Name: g.Meta.Name + " master", AuthType: "api_key",
				EncryptedCreds: enc, Status: "active", Source: "local", ProjectID: projectID,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"master_id": conn.ID, "projects": projects, "created": true}, nil

		case "list_group_projects":
			groupID, _ := args["group_id"].(string)
			all, _ := store.ListConnections(userID, projectID)
			for _, c := range all {
				if c.AppSlug != MasterSlug(groupID) {
					continue
				}
				_, enc, err := store.GetConnection(userID, c.ID)
				if err != nil {
					return nil, err
				}
				plain, err := Decrypt(secret, enc)
				if err != nil {
					return nil, err
				}
				var blob map[string]string
				json.Unmarshal([]byte(plain), &blob)
				var projects []CachedProject
				json.Unmarshal([]byte(blob[credKeyProjectsCache]), &projects)
				return map[string]any{"master_id": c.ID, "projects": projects}, nil
			}
			return nil, fmt.Errorf("no master credential for group %q — call add_account_credential first", groupID)

		case "refresh_group_projects":
			groupID, _ := args["group_id"].(string)
			g := catalog.GetGroup(groupID)
			app := catalog.Get(g.Members[0])
			if g == nil || app == nil {
				return nil, fmt.Errorf("group %q not found", groupID)
			}
			all, _ := store.ListConnections(userID, projectID)
			for _, c := range all {
				if c.AppSlug != MasterSlug(groupID) {
					continue
				}
				_, enc, _ := store.GetConnection(userID, c.ID)
				plain, _ := Decrypt(secret, enc)
				var blob map[string]string
				json.Unmarshal([]byte(plain), &blob)
				real := stripReservedCreds(blob)
				projects, err := discoverProjects(app, &g.Meta, real)
				if err != nil {
					return nil, err
				}
				cacheBytes, _ := json.Marshal(projects)
				blob[credKeyProjectsCache] = string(cacheBytes)
				encoded, _ := json.Marshal(blob)
				enc2, _ := Encrypt(secret, string(encoded))
				store.UpdateConnectionCredentials(c.ID, enc2)
				return map[string]any{"projects": projects}, nil
			}
			return nil, fmt.Errorf("no master for group %q", groupID)

		case "enable_apps_for_projects":
			groupID, _ := args["group_id"].(string)
			g := catalog.GetGroup(groupID)
			if g == nil {
				return nil, fmt.Errorf("group %q not found", groupID)
			}
			selJSON, _ := args["selections"].(string)
			var selections []struct {
				AppSlug           string `json:"app_slug"`
				ExternalProjectID string `json:"external_project_id"`
				Label             string `json:"label"`
			}
			if err := json.Unmarshal([]byte(selJSON), &selections); err != nil {
				return nil, fmt.Errorf("selections must be JSON array: %v", err)
			}
			members := map[string]bool{}
			for _, m := range g.Members {
				members[m] = true
			}
			var masterID int64
			all, _ := store.ListConnections(userID, projectID)
			existing := map[string]int64{}
			for _, c := range all {
				if c.AppSlug == MasterSlug(groupID) {
					masterID = c.ID
					continue
				}
				if !members[c.AppSlug] {
					continue
				}
				_, enc, err := store.GetConnection(userID, c.ID)
				if err != nil {
					continue
				}
				plain, err := Decrypt(secret, enc)
				if err != nil {
					continue
				}
				var cb map[string]string
				json.Unmarshal([]byte(plain), &cb)
				if cb[credKeyType] != "child" {
					continue
				}
				existing[c.AppSlug+"|"+cb[credKeyProjectID]] = c.ID
			}
			if masterID == 0 {
				return nil, fmt.Errorf("no master for group %q — add_account_credential first", groupID)
			}
			created := []map[string]any{}
			for _, sel := range selections {
				if !members[sel.AppSlug] {
					continue
				}
				key := sel.AppSlug + "|" + sel.ExternalProjectID
				if _, ok := existing[key]; ok {
					continue
				}
				app := catalog.Get(sel.AppSlug)
				if app == nil {
					continue
				}
				blob := map[string]string{
					credKeyType: "child", credKeyMasterID: strconv.FormatInt(masterID, 10),
					credKeyProjectID: sel.ExternalProjectID,
				}
				encoded, _ := json.Marshal(blob)
				enc, err := Encrypt(secret, string(encoded))
				if err != nil {
					continue
				}
				connName := sel.Label
				if connName == "" {
					connName = sel.ExternalProjectID
				}
				conn, err := store.CreateConnectionExt(ConnectionInput{
					UserID: userID, AppSlug: sel.AppSlug, AppName: app.Name,
					Name: connName, AuthType: "api_key",
					EncryptedCreds: enc, Status: "active", Source: "local", ProjectID: projectID,
				})
				if err != nil {
					continue
				}
				// Register the MCP row so the connection is callable.
				// Slug base encodes both the service and project so
				// tool prefixes stay distinct across fan-outs.
				store.CreateMCPServerFromConnectionWithSlug(userID, conn, len(app.Tools), sel.AppSlug+"-"+connName)
				created = append(created, map[string]any{"id": conn.ID, "app_slug": conn.AppSlug, "project_id": sel.ExternalProjectID})
			}
			return map[string]any{"created": created, "already_exists": len(existing)}, nil

		case "delete_group_credential":
			groupID, _ := args["group_id"].(string)
			g := catalog.GetGroup(groupID)
			members := map[string]bool{}
			if g != nil {
				for _, m := range g.Members {
					members[m] = true
				}
			}
			all, _ := store.ListConnections(userID, projectID)
			var masterID int64
			masterSlug := MasterSlug(groupID)
			for _, c := range all {
				if c.AppSlug == masterSlug {
					masterID = c.ID
					break
				}
			}
			if masterID == 0 {
				return map[string]any{"removed": 0}, nil
			}
			masterIDStr := strconv.FormatInt(masterID, 10)
			removed := 0
			for _, c := range all {
				if !members[c.AppSlug] {
					continue
				}
				_, enc, err := store.GetConnection(userID, c.ID)
				if err != nil {
					continue
				}
				plain, err := Decrypt(secret, enc)
				if err != nil {
					continue
				}
				var cb map[string]string
				json.Unmarshal([]byte(plain), &cb)
				if cb[credKeyType] != "child" || cb[credKeyMasterID] != masterIDStr {
					continue
				}
				if err := store.DeleteConnection(userID, c.ID); err == nil {
					removed++
				}
			}
			store.DeleteConnection(userID, masterID)
			return map[string]any{"removed": removed + 1}, nil

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
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "apteva-gateway", "version": "1.0.0"},
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

type gatewayMCPServer struct {
	MCPServerRecord
	Kind              string         `json:"kind"`
	CreatedVia        string         `json:"created_via,omitempty"`
	OwnerAppInstallID int64          `json:"owner_app_install_id,omitempty"`
	MCPURL            string         `json:"mcp_url,omitempty"`
	ProxyConfig       map[string]any `json:"proxy_config,omitempty"`
}

func listGatewayMCPServers(store *Store, userID int64, defaultProjectID string, args map[string]any, serverPort, _ string) ([]gatewayMCPServer, error) {
	projectID, _ := args["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = defaultProjectID
	}

	kindFilter, err := normalizeGatewayMCPKind(args["kind"])
	if err != nil {
		return nil, err
	}

	includeAppOwned := true
	if v, ok, err := optionalBoolArg(args["include_app_owned"]); err != nil {
		return nil, fmt.Errorf("include_app_owned must be true or false")
	} else if ok {
		includeAppOwned = v
	}

	servers, err := store.ListMCPServers(userID, projectID)
	if err != nil {
		return nil, err
	}
	if projectID != "" {
		canViewManaged := store.GetPlatformRole(userID) == PlatformAdmin
		if !canViewManaged {
			if role, roleErr := store.GetProjectRole(projectID, userID); roleErr == nil && role.Rank() >= ProjectViewer.Rank() {
				canViewManaged = true
			}
		}
		if canViewManaged {
			if managedRows, managedErr := store.ListManagedMCPsInProject(projectID); managedErr == nil {
				seen := make(map[int64]bool, len(servers))
				for _, row := range servers {
					seen[row.ID] = true
				}
				for _, row := range managedRows {
					if !seen[row.ID] {
						servers = append(servers, row)
					}
				}
			}
		}
	}

	result := make([]gatewayMCPServer, 0, len(servers))
	for _, srv := range servers {
		createdVia, ownerID := gatewayMCPConnectionMeta(store, srv.ConnectionID)
		kind := classifyGatewayMCPServer(srv, createdVia, ownerID)
		if kindFilter != "" && kind != kindFilter {
			continue
		}
		if kindFilter == "" && !includeAppOwned && kind == "app" {
			continue
		}

		row := gatewayMCPServer{
			MCPServerRecord:   srv,
			Kind:              kind,
			CreatedVia:        createdVia,
			OwnerAppInstallID: ownerID,
		}

		switch {
		case srv.Source == "local" && srv.ConnectionID > 0:
			row.Status = "running"
			row.MCPURL = fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, srv.ID)
			row.ProxyConfig = map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       row.MCPURL,
			}
		case srv.Source == "app" && srv.URL != "":
			row.Status = "running"
			row.MCPURL = srv.URL
			if projectID != "" {
				row.MCPURL = addQueryParam(row.MCPURL, "project_id", projectID)
			}
			row.ProxyConfig = map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       row.MCPURL,
			}
		case srv.Source == "remote" && srv.URL != "":
			if row.Status == "" {
				row.Status = "unprobed"
			}
			row.MCPURL = srv.URL
			row.ProxyConfig = map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       srv.URL,
			}
		case srv.Source == "custom" || srv.Source == managedMCPSource:
			row.MCPURL = fmt.Sprintf("http://127.0.0.1:%s/mcp/custom/%d", serverPort, srv.ID)
			row.ProxyConfig = map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       row.MCPURL,
			}
		}

		result = append(result, row)
	}
	return result, nil
}

func gatewayMCPConnectionMeta(store *Store, connID int64) (string, int64) {
	if connID <= 0 {
		return "", 0
	}
	var createdVia string
	var ownerID int64
	_ = store.db.QueryRow(
		`SELECT COALESCE(created_via, ''), COALESCE(owner_app_install_id, 0) FROM connections WHERE id=?`,
		connID,
	).Scan(&createdVia, &ownerID)
	return createdVia, ownerID
}

func classifyGatewayMCPServer(srv MCPServerRecord, createdVia string, ownerID int64) string {
	if srv.Source == "app" || ownerID > 0 || createdVia == "app_install" {
		return "app"
	}
	if srv.Source == "remote" {
		return "remote"
	}
	if srv.Source == "local" && srv.ConnectionID > 0 {
		return "integration"
	}
	return "custom"
}

func normalizeGatewayMCPKind(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	raw, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("kind must be app, integration, custom, remote, or all")
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "", "all", "any":
		return "", nil
	case "app", "apps", "app_mcp", "app-mcp", "app_owned", "app-owned":
		return "app", nil
	case "integration", "integrations", "integration_mcp", "integration-mcp", "local":
		return "integration", nil
	case "custom", "manual", "stdio":
		return "custom", nil
	case "remote", "hosted":
		return "remote", nil
	default:
		return "", fmt.Errorf("kind must be app, integration, custom, remote, or all")
	}
}

func parseIntArg(v any) (int64, error) {
	switch val := v.(type) {
	case string:
		return strconv.ParseInt(val, 10, 64)
	case int:
		return int64(val), nil
	case int64:
		return val, nil
	case float64:
		return int64(val), nil
	default:
		return 0, fmt.Errorf("invalid ID")
	}
}

type gatewayAPIClient struct {
	baseURL        string
	userID         int64
	instanceSecret string
}

func newGatewayAPIClient(userID int64) gatewayAPIClient {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	baseURL := strings.TrimRight(os.Getenv("APTEVA_INTERNAL_SERVER_URL"), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:" + port
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/api"
	secret := os.Getenv("INSTANCE_SECRET")
	if secret == "" {
		secret = os.Getenv("AGENT_SECRET")
	}
	return gatewayAPIClient{baseURL: baseURL, userID: userID, instanceSecret: secret}
}

func (c gatewayAPIClient) do(method, path string, body any, out any) error {
	if c.instanceSecret == "" {
		return fmt.Errorf("internal server API auth unavailable: INSTANCE_SECRET is not set")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Secret", c.instanceSecret)
	req.Header.Set("X-Apteva-MCP-User-ID", strconv.FormatInt(c.userID, 10))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s failed: status=%d body=%s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func handleGatewayAppTool(name string, args map[string]any, defaultProjectID string, serverAPI gatewayAPIClient) (any, error) {
	switch name {
	case "apps_list":
		pid := gatewayProjectIDArg(args, defaultProjectID)
		path := "/apps"
		if pid != "" {
			path += "?project_id=" + urlQueryEscape(pid)
		}
		var out any
		if err := serverAPI.do(http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "apps_marketplace":
		pid := gatewayProjectIDArg(args, defaultProjectID)
		params := []string{}
		if pid != "" {
			params = append(params, "project_id="+urlQueryEscape(pid))
		}
		if registryURL, _ := args["registry_url"].(string); strings.TrimSpace(registryURL) != "" {
			params = append(params, "registry_url="+urlQueryEscape(strings.TrimSpace(registryURL)))
		}
		path := "/apps/marketplace"
		if len(params) > 0 {
			path += "?" + strings.Join(params, "&")
		}
		var out any
		if err := serverAPI.do(http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "apps_install":
		manifestURL, _ := args["manifest_url"].(string)
		manifestYAML, _ := args["manifest_yaml"].(string)
		manifestURL = strings.TrimSpace(manifestURL)
		manifestYAML = strings.TrimSpace(manifestYAML)
		if manifestURL == "" && manifestYAML == "" {
			return nil, fmt.Errorf("manifest_url or manifest_yaml is required")
		}

		global, globalSet, err := optionalBoolArg(args["global"])
		if err != nil {
			return nil, fmt.Errorf("global must be true or false")
		}
		pid := gatewayProjectIDArg(args, defaultProjectID)
		if pid == "" && (!globalSet || !global) {
			return nil, fmt.Errorf("project_id is required because this gateway has no current project; pass global=true only when intentionally installing globally")
		}
		if global {
			pid = ""
		}

		body := map[string]any{
			"manifest_url":  manifestURL,
			"manifest_yaml": manifestYAML,
			"project_id":    pid,
		}
		if repo, _ := args["repo"].(string); strings.TrimSpace(repo) != "" {
			body["repo"] = strings.TrimSpace(repo)
		}
		if ref, _ := args["ref"].(string); strings.TrimSpace(ref) != "" {
			body["ref"] = strings.TrimSpace(ref)
		}
		if policy, _ := args["upgrade_policy"].(string); strings.TrimSpace(policy) != "" {
			body["upgrade_policy"] = strings.TrimSpace(policy)
		}
		if cfg, ok, err := optionalStringMapArg(args["config"]); err != nil {
			return nil, fmt.Errorf("config must be a JSON object")
		} else if ok {
			body["config"] = cfg
		}
		if bindings, ok, err := optionalObjectArg(args["bindings"]); err != nil {
			return nil, fmt.Errorf("bindings must be a JSON object")
		} else if ok {
			body["bindings"] = bindings
		}

		var out any
		if err := serverAPI.do(http.MethodPost, "/apps/install", body, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "apps_upgrade":
		installID, err := parseInstallIDArg(args)
		if err != nil {
			return nil, err
		}
		body := map[string]any{}
		if approved, ok, err := optionalBoolArg(args["approve_new_permissions"]); err != nil {
			return nil, fmt.Errorf("approve_new_permissions must be true or false")
		} else if ok {
			body["approve_new_permissions"] = approved
		}
		var out any
		if err := serverAPI.do(http.MethodPost, fmt.Sprintf("/apps/installs/%d/upgrade", installID), body, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "apps_uninstall":
		installID, err := parseInstallIDArg(args)
		if err != nil {
			return nil, err
		}
		projectID := gatewayProjectIDArg(args, defaultProjectID)
		if projectID == "" {
			return nil, fmt.Errorf("project_id is required to uninstall an app")
		}
		params := url.Values{"project_id": []string{projectID}}
		if force, ok, err := optionalBoolArg(args["force"]); err != nil {
			return nil, fmt.Errorf("force must be true or false")
		} else if ok && force {
			params.Set("force", "1")
		}
		path := fmt.Sprintf("/apps/installs/%d?%s", installID, params.Encode())
		var out any
		if err := serverAPI.do(http.MethodDelete, path, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func gatewayProjectIDArg(args map[string]any, defaultProjectID string) string {
	pid, _ := args["project_id"].(string)
	pid = strings.TrimSpace(pid)
	if pid == "" {
		pid = strings.TrimSpace(defaultProjectID)
	}
	return pid
}

func parseInstallIDArg(args map[string]any) (int64, error) {
	if v, ok := args["install_id"]; ok {
		id, err := parseIntArg(v)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("install_id must be a positive integer")
		}
		return id, nil
	}
	id, err := parseIntArg(args["id"])
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("install_id is required")
	}
	return id, nil
}

func handleGatewayAgentTool(name string, args map[string]any, projectID string, serverAPI gatewayAPIClient, store *Store, selfPath string) (any, error) {
	switch name {
	case "agents_list":
		pid, _ := args["project_id"].(string)
		if pid == "" {
			pid = projectID
		}
		path := "/agents"
		if pid != "" {
			path += "?project_id=" + urlQueryEscape(pid)
		}
		var out any
		if err := serverAPI.do(http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "agent_list_activity":
		if store == nil {
			return nil, fmt.Errorf("store unavailable")
		}
		opts, err := gatewayAgentActivityOptions(args, projectID)
		if err != nil {
			return nil, err
		}
		return BuildAgentActivity(store, serverAPI.userID, opts)

	case "agents_get":
		id, _ := parseIntArg(args["id"])
		var out any
		if err := serverAPI.do(http.MethodGet, fmt.Sprintf("/agents/%d", id), nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "agents_create":
		agentName, _ := args["name"].(string)
		directive, _ := args["directive"].(string)
		agentName = strings.TrimSpace(agentName)
		directive = strings.TrimSpace(directive)
		if agentName == "" {
			return nil, fmt.Errorf("name is required")
		}
		if directive == "" {
			return nil, fmt.Errorf("directive is required")
		}
		pid, _ := args["project_id"].(string)
		if pid == "" {
			pid = projectID
		}
		body := map[string]any{
			"name":             agentName,
			"directive":        directive,
			"project_id":       pid,
			"include_channels": true,
		}
		if mode, _ := args["mode"].(string); mode != "" {
			body["mode"] = mode
		}
		if v, ok, err := optionalBoolArg(args["start"]); err != nil {
			return nil, fmt.Errorf("start must be true or false")
		} else if ok {
			body["start"] = v
		}
		if v, ok, err := optionalBoolArg(args["include_channels"]); err != nil {
			return nil, fmt.Errorf("include_channels must be true or false")
		} else if ok {
			body["include_channels"] = v
		}
		if v, ok, err := optionalBoolArg(args["unconscious"]); err != nil {
			return nil, fmt.Errorf("unconscious must be true or false")
		} else if ok {
			body["unconscious"] = v
		}
		if cfg, ok, err := optionalConfigArg(args["config"]); err != nil {
			return nil, err
		} else if ok {
			body["config"] = cfg
		}
		if ids, err := parseIntListArg(args["bound_app_install_ids"]); err != nil {
			return nil, fmt.Errorf("bound_app_install_ids must be comma-separated IDs")
		} else if len(ids) > 0 {
			body["bound_app_install_ids"] = ids
		}
		if ids, err := parseIntListArg(args["bound_connection_ids"]); err != nil {
			return nil, fmt.Errorf("bound_connection_ids must be comma-separated IDs")
		} else if len(ids) > 0 {
			body["bound_connection_ids"] = ids
		}
		var out any
		if err := serverAPI.do(http.MethodPost, "/agents", body, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "agents_update":
		id, _ := parseIntArg(args["id"])
		result := map[string]any{"id": id}
		if newName, _ := args["name"].(string); strings.TrimSpace(newName) != "" {
			var out any
			if err := serverAPI.do(http.MethodPut, fmt.Sprintf("/agents/%d", id), map[string]any{"name": strings.TrimSpace(newName)}, &out); err != nil {
				return nil, err
			}
			result["rename"] = out
		}
		configBody := map[string]any{}
		if directive, _ := args["directive"].(string); strings.TrimSpace(directive) != "" {
			configBody["directive"] = strings.TrimSpace(directive)
		} else if nextDirective, changed, err := directiveEditsForGatewayAgent(id, args, serverAPI); err != nil {
			return nil, err
		} else if changed {
			configBody["directive"] = nextDirective
		}
		if mode, _ := args["mode"].(string); strings.TrimSpace(mode) != "" {
			configBody["mode"] = strings.TrimSpace(mode)
		}
		if cfg, ok, err := optionalConfigArg(args["config"]); err != nil {
			return nil, err
		} else if ok {
			configBody["config"] = cfg
		}
		if len(configBody) > 0 {
			var out any
			if err := serverAPI.do(http.MethodPut, fmt.Sprintf("/agents/%d/config", id), configBody, &out); err != nil {
				return nil, err
			}
			result["config"] = out
		}
		if _, ok := args["mcp_server_ids"]; ok {
			if store == nil {
				return nil, fmt.Errorf("store unavailable")
			}
			serverIDs, err := parseIntListArg(args["mcp_server_ids"])
			if err != nil {
				return nil, fmt.Errorf("mcp_server_ids must be comma-separated IDs")
			}
			action, _ := args["mcp_action"].(string)
			action = strings.ToLower(strings.TrimSpace(action))
			if action == "" {
				action = "set"
			}
			if action != "set" && action != "add" && action != "remove" {
				return nil, fmt.Errorf("mcp_action must be set, add, or remove")
			}
			out, err := updateAgentMCPServersFromGateway(id, serverIDs, action, projectID, serverAPI, store, selfPath)
			if err != nil {
				return nil, err
			}
			result["mcp_servers"] = out
		}
		if len(result) == 1 {
			return nil, fmt.Errorf("nothing to update; pass name, directive, directive edit fields, mode, config, or mcp_server_ids")
		}
		return result, nil

	case "agents_start":
		id, _ := parseIntArg(args["id"])
		var out any
		if err := serverAPI.do(http.MethodPost, fmt.Sprintf("/agents/%d/start", id), nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "agents_stop":
		id, _ := parseIntArg(args["id"])
		var out any
		if err := serverAPI.do(http.MethodPost, fmt.Sprintf("/agents/%d/stop", id), nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "agents_delete":
		id, _ := parseIntArg(args["id"])
		if err := serverAPI.do(http.MethodDelete, fmt.Sprintf("/agents/%d", id), nil, nil); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true, "id": id}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func directiveEditsForGatewayAgent(agentID int64, args map[string]any, serverAPI gatewayAPIClient) (string, bool, error) {
	if !hasGatewayDirectiveEditArgs(args) {
		return "", false, nil
	}

	var current struct {
		Directive string `json:"directive"`
	}
	if err := serverAPI.do(http.MethodGet, fmt.Sprintf("/agents/%d", agentID), nil, &current); err != nil {
		return "", false, fmt.Errorf("load current directive for section edit: %w", err)
	}
	next, changed, err := applyServerDirectiveEdits(current.Directive, args)
	if err != nil {
		return "", false, err
	}
	return next, changed, nil
}

func gatewayAgentActivityOptions(args map[string]any, defaultProjectID string) (AgentActivityOptions, error) {
	var opts AgentActivityOptions
	if projectID, _ := args["project_id"].(string); strings.TrimSpace(projectID) != "" {
		opts.ProjectID = strings.TrimSpace(projectID)
	} else {
		opts.ProjectID = defaultProjectID
	}
	if _, ok := args["agent_id"]; ok {
		id, err := parseIntArg(args["agent_id"])
		if err != nil {
			return opts, fmt.Errorf("agent_id must be an integer")
		}
		opts.AgentID = id
	}
	if threadID, _ := args["thread_id"].(string); strings.TrimSpace(threadID) != "" {
		opts.ThreadID = strings.TrimSpace(threadID)
	}
	if kind, _ := args["kind"].(string); strings.TrimSpace(kind) != "" {
		opts.Kind = strings.ToLower(strings.TrimSpace(kind))
	}
	if opts.Kind != "" && opts.Kind != "all" && opts.Kind != "thought" && opts.Kind != "tool" && opts.Kind != "thread" && opts.Kind != "event" && opts.Kind != "error" {
		return opts, fmt.Errorf("kind must be all, thought, tool, thread, event, or error")
	}
	if status, _ := args["status"].(string); strings.TrimSpace(status) != "" {
		opts.Status = strings.ToLower(strings.TrimSpace(status))
	}
	if opts.Status != "" && opts.Status != "all" && opts.Status != "running" && opts.Status != "success" && opts.Status != "error" && opts.Status != "info" {
		return opts, fmt.Errorf("status must be all, running, success, error, or info")
	}
	if period, _ := args["period"].(string); strings.TrimSpace(period) != "" {
		opts.Period = strings.TrimSpace(period)
	}
	if since, _ := args["since"].(string); strings.TrimSpace(since) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(since))
		if err != nil {
			return opts, fmt.Errorf("since must be RFC3339")
		}
		opts.Since = t
	}
	if _, ok := args["limit"]; ok {
		limit, err := parseIntArg(args["limit"])
		if err != nil {
			return opts, fmt.Errorf("limit must be an integer")
		}
		opts.Limit = int(limit)
	}
	if query, _ := args["query"].(string); strings.TrimSpace(query) != "" {
		opts.Query = strings.TrimSpace(query)
	}
	if v, ok, err := optionalBoolArg(args["include_payloads"]); err != nil {
		return opts, fmt.Errorf("include_payloads must be true or false")
	} else if ok {
		opts.IncludePayloads = v
	}
	if v, ok, err := optionalBoolArg(args["include_raw"]); err != nil {
		return opts, fmt.Errorf("include_raw must be true or false")
	} else if ok {
		opts.IncludeRaw = v
	}
	return opts, nil
}

func hasGatewayDirectiveEditArgs(args map[string]any) bool {
	for _, key := range []string{"directive_edits", "directive_edit_mode", "directive_section", "directive_match", "directive_content"} {
		if _, ok := args[key]; ok {
			return true
		}
	}
	return false
}

func updateAgentMCPServersFromGateway(agentID int64, serverIDs []int64, action, defaultProjectID string, serverAPI gatewayAPIClient, store *Store, selfPath string) (any, error) {
	// Attachment mutation belongs to the server API so dashboard and agent
	// tools share one atomic, project-scoped implementation. In particular,
	// do not GET + replace the full list here: two callers could otherwise
	// erase each other's changes.
	body := map[string]any{
		"action":         action,
		"mcp_server_ids": serverIDs,
	}
	var mutation any
	if err := serverAPI.do(http.MethodPost, fmt.Sprintf("/agents/%d/mcp-servers", agentID), body, &mutation); err != nil {
		return nil, err
	}
	var current struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := serverAPI.do(http.MethodGet, fmt.Sprintf("/agents/%d/config", agentID), nil, &current); err != nil {
		return nil, err
	}
	return map[string]any{
		"id":             agentID,
		"action":         action,
		"mcp_server_ids": serverIDs,
		"mcp_servers":    current.MCPServers,
		"count":          len(current.MCPServers),
		"result":         mutation,
	}, nil
}

func gatewayMCPConfigFromRecord(record MCPServerRecord, projectID, serverPort, _ string) (map[string]any, error) {
	switch {
	case record.Source == "local" && record.ConnectionID > 0:
		return map[string]any{
			"name":      record.Name,
			"transport": "http",
			"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, record.ID),
		}, nil
	case record.Source == "app" && record.URL != "":
		u := record.URL
		if projectID != "" {
			u = addQueryParam(u, "project_id", projectID)
		}
		return map[string]any{
			"name":      record.Name,
			"transport": "http",
			"url":       u,
		}, nil
	case record.Source == "remote" && record.URL != "":
		return map[string]any{
			"name":      record.Name,
			"transport": "http",
			"url":       record.URL,
		}, nil
	case record.Source == "custom" || record.Source == managedMCPSource:
		return map[string]any{
			"name":      record.Name,
			"transport": "http",
			"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/custom/%d", serverPort, record.ID),
		}, nil
	default:
		return nil, fmt.Errorf("missing URL or command")
	}
}

func filterSystemMCPConfigs(servers []map[string]any) []map[string]any {
	var out []map[string]any
	for _, srv := range servers {
		if gatewayMCPConfigIsSystem(srv) {
			out = append(out, srv)
		}
	}
	return out
}

func removeMatchingMCPConfigs(servers []map[string]any, targets []map[string]any) []map[string]any {
	identities := map[string]bool{}
	for _, target := range targets {
		for _, identity := range mcpConfigIdentities(target) {
			identities[identity] = true
		}
	}
	out := make([]map[string]any, 0, len(servers))
	for _, srv := range servers {
		remove := false
		for _, identity := range mcpConfigIdentities(srv) {
			if identities[identity] {
				remove = true
				break
			}
		}
		if remove {
			continue
		}
		out = append(out, srv)
	}
	return out
}

func mcpConfigIdentities(config map[string]any) []string {
	var out []string
	if name, _ := config["name"].(string); strings.TrimSpace(name) != "" {
		out = append(out, "name:"+strings.TrimSpace(name))
	}
	rawURL, _ := config["url"].(string)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return out
	}
	if installID := strings.TrimSpace(parsed.Query().Get("install_id")); installID != "" {
		out = append(out, "app-install:"+installID)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 2 && parts[0] == "mcp" {
		if _, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			out = append(out, "registry:"+parts[1])
		}
	}
	if len(parts) == 3 && parts[0] == "mcp" && parts[1] == "custom" {
		if _, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
			out = append(out, "registry:"+parts[2])
		}
	}
	return out
}

func gatewayMCPConfigIsSystem(srv map[string]any) bool {
	name, _ := srv["name"].(string)
	return name == "apteva-server" || isServerOwnedOutputMCP(name)
}

func optionalBoolArg(v any) (bool, bool, error) {
	if v == nil {
		return false, false, nil
	}
	switch val := v.(type) {
	case bool:
		return val, true, nil
	case string:
		if strings.TrimSpace(val) == "" {
			return false, false, nil
		}
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return parsed, true, err
	default:
		return false, false, fmt.Errorf("invalid boolean")
	}
}

func optionalConfigArg(v any) (string, bool, error) {
	if v == nil {
		return "", false, nil
	}
	switch val := v.(type) {
	case string:
		val = strings.TrimSpace(val)
		if val == "" {
			return "", false, nil
		}
		if !json.Valid([]byte(val)) {
			return "", false, fmt.Errorf("config must be valid JSON")
		}
		return val, true, nil
	case map[string]any:
		data, err := json.Marshal(val)
		if err != nil {
			return "", false, err
		}
		return string(data), true, nil
	default:
		return "", false, fmt.Errorf("config must be a JSON object or JSON string")
	}
}

func optionalObjectArg(v any) (map[string]any, bool, error) {
	if v == nil {
		return nil, false, nil
	}
	switch val := v.(type) {
	case string:
		val = strings.TrimSpace(val)
		if val == "" {
			return nil, false, nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(val), &out); err != nil {
			return nil, false, err
		}
		if out == nil {
			out = map[string]any{}
		}
		return out, true, nil
	case map[string]any:
		return val, true, nil
	default:
		return nil, false, fmt.Errorf("must be a JSON object or object string")
	}
}

func optionalStringMapArg(v any) (map[string]string, bool, error) {
	obj, ok, err := optionalObjectArg(v)
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make(map[string]string, len(obj))
	for k, val := range obj {
		switch typed := val.(type) {
		case string:
			out[k] = typed
		case nil:
			out[k] = ""
		default:
			out[k] = fmt.Sprintf("%v", typed)
		}
	}
	return out, true, nil
}

func parseIntListArg(v any) ([]int64, error) {
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case string:
		if strings.TrimSpace(val) == "" {
			return nil, nil
		}
		parts := strings.Split(val, ",")
		out := make([]int64, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	case []any:
		out := make([]int64, 0, len(val))
		for _, item := range val {
			id, err := parseIntArg(item)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid integer list")
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

// parseCSV accepts either a comma-separated string, a JSON string array, or
// a native []any / []string value (depending on how the MCP client encoded
// the arg) and returns a clean de-duped slice of names. Used by the gateway
// tools that take an allowed_tools list.
func parseCSV(v any) []string {
	if v == nil {
		return nil
	}
	var raw []string
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		// Accept JSON array syntax ["a","b"] too.
		if strings.HasPrefix(s, "[") {
			_ = json.Unmarshal([]byte(s), &raw)
		}
		if len(raw) == 0 {
			raw = splitArgs(s)
		}
	case []any:
		for _, item := range t {
			if str, ok := item.(string); ok {
				raw = append(raw, str)
			}
		}
	case []string:
		raw = t
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
