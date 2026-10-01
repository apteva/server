package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

const platformMCPName = "apteva-server"
const platformMCPSource = "builtin"
const platformMCPPath = "/api/apps/apteva-server/mcp"
const platformMCPDisplayName = "Apteva Server"
const platformMCPDescription = "Manage agents, apps, and connections. Available in agent capabilities."

// Reuse the dashboard asset. A data URL also works for clients hosted on an
// app origin, where a dashboard-relative icon URL would resolve incorrectly.
//
//go:embed dashboard/apteva-server.svg
var platformMCPIconSVG []byte

var platformMCPIconURL = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(platformMCPIconSVG)

// A real inventory identity lets built-ins use the same attachment API as
// apps and integrations. Credentials and URLs are always generated at runtime.
func (s *Store) ensurePlatformMCPInventory(userID int64) error {
	_, err := s.db.Exec(`INSERT INTO mcp_servers
		(user_id,name,command,args,encrypted_env,description,project_id,source,transport,status)
		SELECT ?,?,'','[]','','Apteva Server','','builtin','http','running'
		WHERE NOT EXISTS (SELECT 1 FROM mcp_servers WHERE user_id=? AND source='builtin' AND name=?)`,
		userID, platformMCPName, userID, platformMCPName)
	return err
}

func platformMCPAttached(agent *Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Kind == "platform_helper" {
		return true
	}
	var config map[string]any
	_ = json.Unmarshal([]byte(agent.Config), &config)
	// Retain the saved flag as the server-owned grant, including legacy agents.
	attached, _ := config["include_apteva_server"].(bool)
	return attached
}

func (s *Server) authorizePlatformMCPAgent(agent *Agent) error {
	if agent == nil {
		return fmt.Errorf("agent required")
	}
	if agent.Kind == "platform_helper" || s.store.GetPlatformRole(agent.UserID) == PlatformAdmin {
		return nil
	}
	if agent.ProjectID == "" {
		return fmt.Errorf("global management capability requires a platform administrator")
	}
	role, err := s.store.GetProjectRole(agent.ProjectID, agent.UserID)
	if err != nil || role.Rank() < ProjectEditor.Rank() {
		return fmt.Errorf("agent owner requires editor access to the project")
	}
	return nil
}

// Bind the bearer capability to the agent and its saved project. A different
// agent header or a project move cannot reuse an issued credential.
func platformMCPToken(secret string, agent *Agent) string {
	return internalMCPCapability(secret, fmt.Sprintf("%s/agent/%d/project/%s", platformMCPPath, agent.ID, agent.ProjectID))
}
