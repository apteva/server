package main

// environment_derive.go — build a Environment from an agent's app bindings.
//
// app_agent_bindings is the environment/grant membership index used here.
// For MCP-capable apps it is derived from the agent's canonical mcp_servers
// config whenever attachments change; UI/worker-only apps have no MCP entry
// and remain directly bound. This turns the resulting membership into an
// EnvironmentSpec rather than maintaining a second runtime tool catalog.
//
// Today it derives the directly-bound apps. Sibling deps (manifest
// requires.apps, e.g. social→storage/jobs) are a documented follow-up; the
// common single-app case (your "files via storage" agent) needs nothing more.

import (
	"fmt"
	"os"
	"path/filepath"
)

// AppNamesForAgent returns the manifest names of the apps bound to the agent
// (enabled bindings only): app_agent_bindings → app_installs → apps.
func (s *Server) AppNamesForAgent(agentID int64) ([]string, error) {
	rows, err := s.store.db.Query(`
		SELECT a.name
		FROM app_agent_bindings b
		JOIN app_installs i ON i.id = b.install_id
		JOIN apps a ON a.id = i.app_id
		WHERE b.agent_id = ? AND b.enabled = 1
		ORDER BY a.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil && n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names, rows.Err()
}

// DeriveEnvironmentSpecForAgent builds a EnvironmentSpec from the agent's bound apps,
// resolving each app's local source dir. Scoped to environmentID as the project;
// edge defaults to block (no externals unless the caller adds fixtures).
func (s *Server) DeriveEnvironmentSpecForAgent(agent *Agent, environmentID string) (EnvironmentSpec, error) {
	if agent == nil {
		return EnvironmentSpec{}, fmt.Errorf("derive environment: nil agent")
	}
	if environmentID == "" {
		return EnvironmentSpec{}, fmt.Errorf("derive environment: environmentID required")
	}
	names, err := s.AppNamesForAgent(agent.ID)
	if err != nil {
		return EnvironmentSpec{}, fmt.Errorf("read bindings for agent %d: %w", agent.ID, err)
	}
	resolve := s.environments.ResolveSource
	if resolve == nil {
		resolve = defaultSourceResolver
	}
	appSrcDirs := make(map[string]string, len(names))
	for _, name := range names {
		dir, rerr := resolve(name)
		if rerr != nil {
			return EnvironmentSpec{}, fmt.Errorf("resolve source for bound app %q: %w", name, rerr)
		}
		appSrcDirs[name] = dir
	}
	return EnvironmentSpec{
		ID:         environmentID,
		ProjectID:  agent.ProjectID,
		GatewayURL: s.localGatewayURL(),
		AppSrcDirs: appSrcDirs,
		Mode:       EdgeBlock,
	}, nil
}

// CreateEnvironmentForAgent derives a environment spec from the agent's bindings and
// stands it up.
func (s *Server) CreateEnvironmentForAgent(agent *Agent, environmentID string) (*Environment, error) {
	spec, err := s.DeriveEnvironmentSpecForAgent(agent, environmentID)
	if err != nil {
		return nil, err
	}
	return s.environments.Create(spec)
}

// defaultSourceResolver locates an app's local working-copy dir by manifest
// name (dev/CI checkout layout). Returns an error when no source is present —
// environment derivation needs source to build the app.
func defaultSourceResolver(name string) (string, error) {
	candidates := []string{
		filepath.Join("..", "apps", "mcp", name),
		filepath.Join("apps", "mcp", name),
		filepath.Join("..", "app-"+name),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(filepath.Join(c, "apteva.yaml")); err == nil && !fi.IsDir() {
			if abs, aerr := filepath.Abs(c); aerr == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("no local source dir for app %q (looked in apps/mcp/%s)", name, name)
}
