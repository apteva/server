package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// syncComposioProviderData pulls the user's existing Composio state —
// connected accounts and custom MCP servers — and mirrors it into our
// `connections` and `mcp_servers` tables. Runs on provider creation so
// the dashboard reflects what already exists on Composio without the
// user re-creating everything.
//
// Idempotent: re-runs upsert by external_id / upstream_id, so a later
// manual re-sync (when we add it) and accidental double-calls are safe.
//
// Scope: imported rows inherit the provider's projectID.
func (s *Server) syncComposioProviderData(userID, providerID int64, projectID string) {
	start := time.Now()
	log.Printf("[COMPOSIO-SYNC] begin user=%d provider=%d project=%s", userID, providerID, projectID)
	defer func() {
		log.Printf("[COMPOSIO-SYNC] end user=%d provider=%d project=%s dur=%s",
			userID, providerID, projectID, time.Since(start).Round(time.Millisecond))
	}()

	client, err := s.composioClientFor(userID, providerID)
	if err != nil {
		log.Printf("[COMPOSIO-SYNC] client resolve failed: %v", err)
		return
	}

	// Pull auth_configs first so we can correlate connected_accounts whose
	// payload lacks an inline toolkit slug back to a toolkit.
	authCfgs, err := client.ListAllAuthConfigs()
	if err != nil {
		log.Printf("[COMPOSIO-SYNC] list auth_configs failed: %v", err)
		return
	}
	slugByAuthCfg := map[string]string{}
	for _, ac := range authCfgs {
		if ac.ToolkitSlug != "" {
			slugByAuthCfg[ac.ID] = ac.ToolkitSlug
		}
	}
	log.Printf("[COMPOSIO-SYNC] fetched %d auth_configs", len(authCfgs))

	// Connected accounts → connections.
	accounts, err := client.ListConnectedAccounts()
	if err != nil {
		log.Printf("[COMPOSIO-SYNC] list connected_accounts failed: %v", err)
		return
	}
	log.Printf("[COMPOSIO-SYNC] fetched %d connected_accounts", len(accounts))

	existingConns, _ := s.store.ListConnections(userID, projectID)
	connByExt := map[string]Connection{}
	connIDBySlug := map[string]int64{}
	for _, c := range existingConns {
		if c.Source == "composio" && c.ProviderID == providerID && c.ExternalID != "" {
			connByExt[c.ExternalID] = c
		}
	}
	for _, a := range accounts {
		slug := a.ToolkitSlug
		if slug == "" {
			slug = slugByAuthCfg[a.AuthConfigID]
		}
		if slug == "" {
			log.Printf("[COMPOSIO-SYNC] skip connected_account id=%s: no toolkit slug", a.ID)
			continue
		}
		status := composioStatusToLocal(a.Status)
		if existing, ok := connByExt[a.ID]; ok {
			if existing.Status != status {
				if err := s.store.UpdateConnectionStatus(existing.ID, status); err != nil {
					log.Printf("[COMPOSIO-SYNC] update conn id=%d status=%s failed: %v", existing.ID, status, err)
				}
			}
			connIDBySlug[slug] = existing.ID
			continue
		}
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:     userID,
			AppSlug:    slug,
			AppName:    slug,
			Name:       slug,
			AuthType:   "composio",
			ProjectID:  projectID,
			Source:     "composio",
			Status:     status,
			ProviderID: providerID,
			ExternalID: a.ID,
		})
		if err != nil {
			log.Printf("[COMPOSIO-SYNC] create conn slug=%s external=%s failed: %v", slug, a.ID, err)
			continue
		}
		connIDBySlug[slug] = conn.ID
		log.Printf("[COMPOSIO-SYNC] imported conn id=%d slug=%s external=%s status=%s", conn.ID, slug, a.ID, status)
	}

	// MCP servers → mcp_servers (source=remote).
	servers, err := client.ListComposioMCPServers()
	if err != nil {
		log.Printf("[COMPOSIO-SYNC] list mcp_servers failed: %v", err)
		return
	}
	log.Printf("[COMPOSIO-SYNC] fetched %d mcp_servers", len(servers))

	existingMCP, _ := s.store.ListMCPServers(userID, projectID)
	mcpByUpstream := map[string]MCPServerRecord{}
	for _, r := range existingMCP {
		if r.Source == "remote" && r.ProviderID == providerID && r.UpstreamID != "" {
			mcpByUpstream[r.UpstreamID] = r
		}
	}
	for _, srv := range servers {
		// Link to a connection when we can pin a toolkit slug. A multi-toolkit
		// server picks the first matching connection we have on record; the
		// reconciler can re-key it if the scope changes later.
		var connID int64
		for _, slug := range srv.ToolkitSlugs {
			if id, ok := connIDBySlug[slug]; ok {
				connID = id
				break
			}
		}
		allowedJSON := ""
		if len(srv.AllowedTools) > 0 {
			b, _ := json.Marshal(srv.AllowedTools)
			allowedJSON = string(b)
		}
		if existing, ok := mcpByUpstream[srv.ID]; ok {
			_, err := s.store.db.Exec(
				`UPDATE mcp_servers SET name=?, url=?, allowed_tools=?, connection_id=? WHERE id=?`,
				srv.Name, srv.URL, allowedJSON, connID, existing.ID,
			)
			if err != nil {
				log.Printf("[COMPOSIO-SYNC] update mcp id=%d upstream=%s failed: %v", existing.ID, srv.ID, err)
			}
			continue
		}
		desc := "Composio MCP: " + strings.Join(srv.ToolkitSlugs, ", ")
		row, err := s.store.CreateMCPServerExt(MCPServerInput{
			UserID:       userID,
			Name:         srv.Name,
			Description:  desc,
			Source:       "remote",
			Transport:    "http",
			URL:          srv.URL,
			ProviderID:   providerID,
			ProjectID:    projectID,
			ConnectionID: connID,
			AllowedTools: srv.AllowedTools,
			UpstreamID:   srv.ID,
		})
		if err != nil {
			log.Printf("[COMPOSIO-SYNC] create mcp name=%s upstream=%s failed: %v", srv.Name, srv.ID, err)
			continue
		}
		log.Printf("[COMPOSIO-SYNC] imported mcp id=%d name=%s upstream=%s conn=%d", row.ID, srv.Name, srv.ID, connID)
	}
}

// composioStatusToLocal maps Composio's uppercase connected_account states
// onto the lowercase values our connections.status column uses.
func composioStatusToLocal(s string) string {
	switch strings.ToUpper(s) {
	case "ACTIVE":
		return "active"
	case "INITIATED":
		return "pending"
	case "FAILED", "EXPIRED":
		return "error"
	default:
		return "pending"
	}
}
