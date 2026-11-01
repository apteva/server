package main

// agent_project_grants.go lets one agent use an attached app, or the Apteva
// Server tools, in projects other than its own. Without a grant nothing
// changes: every call runs in the agent's project exactly as before.
//
// A grant is read on every call, so revoking it takes effect at once. It never
// lets the agent do more in a project than the agent's owner, and the person
// who set the grant, could do there themselves: both need editor access in the
// target project, the same role a person needs to call app tools.
//
// Only globally installed apps qualify. The agent keeps calling the same
// install, which already serves each project separately; a project with its
// own install of the app keeps its data there and is out of reach.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// grantProjectParam names the project on a granted call. It is the argument
// the Apteva Server tools already take, and most apps that declare their own
// project_id mean the same thing by it.
const grantProjectParam = "project_id"

// grantProjectListLimit bounds the project names written into tool schemas.
const grantProjectListLimit = 30

// grantAllProjects as project_id runs a read-only tool in the agent's own
// project and every project its grant reaches, in one call. The limits match
// app-to-app batches.
const (
	grantAllProjects      = "*"
	grantFanOutLimit      = 32
	grantFanOutWorkers    = 4
	grantFanOutChildBytes = 16 << 20
	grantFanOutTotalBytes = 32 << 20
	grantToolHintTTL      = 2 * time.Minute
)

type AgentProjectGrant struct {
	AgentID     int64    `json:"agent_id"`
	Target      string   `json:"target"`
	AllProjects bool     `json:"all_projects"`
	ProjectIDs  []string `json:"project_ids"`
	Tools       []string `json:"tools"`
	CreatedBy   int64    `json:"created_by"`
	UpdatedAt   string   `json:"updated_at"`
}

type grantProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Store) GetAgentProjectGrant(agentID int64, target string) (*AgentProjectGrant, error) {
	grants, err := s.queryAgentProjectGrants(`WHERE agent_id=? AND target=?`, agentID, target)
	if err != nil || len(grants) == 0 {
		return nil, err
	}
	return &grants[0], nil
}

func (s *Store) ListAgentProjectGrants(agentID int64) ([]AgentProjectGrant, error) {
	return s.queryAgentProjectGrants(`WHERE agent_id=? ORDER BY target`, agentID)
}

func (s *Store) queryAgentProjectGrants(where string, args ...any) ([]AgentProjectGrant, error) {
	rows, err := s.db.Query(`SELECT agent_id, target, all_projects, project_ids, tools, created_by, COALESCE(updated_at,'')
		FROM agent_project_grants `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentProjectGrant{}
	for rows.Next() {
		var g AgentProjectGrant
		var all int
		var projects, tools string
		if err := rows.Scan(&g.AgentID, &g.Target, &all, &projects, &tools, &g.CreatedBy, &g.UpdatedAt); err != nil {
			return nil, err
		}
		g.AllProjects = all == 1
		_ = json.Unmarshal([]byte(projects), &g.ProjectIDs)
		_ = json.Unmarshal([]byte(tools), &g.Tools)
		if g.ProjectIDs == nil {
			g.ProjectIDs = []string{}
		}
		if g.Tools == nil {
			g.Tools = []string{}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) SaveAgentProjectGrant(g AgentProjectGrant) error {
	projects, _ := json.Marshal(g.ProjectIDs)
	tools, _ := json.Marshal(g.Tools)
	all := 0
	if g.AllProjects {
		all = 1
	}
	_, err := s.db.Exec(`INSERT INTO agent_project_grants(agent_id, target, all_projects, project_ids, tools, created_by, updated_at)
		VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, target) DO UPDATE SET all_projects=excluded.all_projects, project_ids=excluded.project_ids,
			tools=excluded.tools, created_by=excluded.created_by, updated_at=CURRENT_TIMESTAMP`,
		g.AgentID, g.Target, all, string(projects), string(tools), g.CreatedBy)
	return err
}

func (s *Store) DeleteAgentProjectGrant(agentID int64, target string) error {
	_, err := s.db.Exec(`DELETE FROM agent_project_grants WHERE agent_id=? AND target=?`, agentID, target)
	return err
}

func (g *AgentProjectGrant) toolAllowed(tool string) bool {
	return len(g.Tools) == 0 || containsExact(g.Tools, tool)
}

// grantReachesProject is the whole runtime rule: the grant names the project
// (or all projects), the project uses the global install, and both the
// agent's owner and the grant's author can still edit it.
func (s *Server) grantReachesProject(agent *Agent, g *AgentProjectGrant, projectID string) bool {
	if g == nil || agent == nil || projectID == "" || projectID == agent.ProjectID {
		return false
	}
	if !g.AllProjects && !containsExact(g.ProjectIDs, projectID) {
		return false
	}
	if !s.projectUsesGlobalInstall(g.Target, projectID) {
		return false
	}
	return s.effectiveRoleOnProject(agent.UserID, projectID).Rank() >= ProjectEditor.Rank() &&
		s.effectiveRoleOnProject(g.CreatedBy, projectID).Rank() >= ProjectEditor.Rank()
}

// projectUsesGlobalInstall is false when the project has its own install of
// the app, running or not. The Apteva Server tools have no installs.
func (s *Server) projectUsesGlobalInstall(target, projectID string) bool {
	if target == platformMCPName {
		return true
	}
	var n int
	err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE a.name=? AND i.project_id=?`, target, projectID).Scan(&n)
	return err == nil && n == 0
}

// grantProjects lists the projects a grant reaches right now, sorted by name.
func (s *Server) grantProjects(agent *Agent, g *AgentProjectGrant) []grantProject {
	var candidates []grantProject
	if g.AllProjects {
		projects, err := s.store.ListProjectsForUser(agent.UserID)
		if err != nil {
			return nil
		}
		for _, p := range projects {
			candidates = append(candidates, grantProject{ID: p.ID, Name: p.Name})
		}
	} else {
		for _, id := range g.ProjectIDs {
			if name, ok := s.projectName(id); ok {
				candidates = append(candidates, grantProject{ID: id, Name: name})
			}
		}
	}
	out := make([]grantProject, 0, len(candidates))
	for _, p := range candidates {
		if s.grantReachesProject(agent, g, p.ID) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *Server) projectName(id string) (string, bool) {
	var name string
	if err := s.store.db.QueryRow(`SELECT name FROM projects WHERE id=?`, id).Scan(&name); err != nil {
		return "", false
	}
	return name, true
}

// resolveGrantProject accepts a project id or its exact name (any case). It
// returns "" for the agent's own project, which needs no grant.
func (s *Server) resolveGrantProject(agent *Agent, g *AgentProjectGrant, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == agent.ProjectID {
		return "", nil
	}
	if home, ok := s.projectName(agent.ProjectID); ok && strings.EqualFold(home, raw) {
		return "", nil
	}
	if s.grantReachesProject(agent, g, raw) {
		return raw, nil
	}
	var match string
	for _, p := range s.grantProjects(agent, g) {
		if strings.EqualFold(p.Name, raw) {
			if match != "" {
				return "", fmt.Errorf("more than one project is named %q; pass its id", raw)
			}
			match = p.ID
		}
	}
	if match == "" {
		return "", fmt.Errorf("this agent cannot run tools in project %q. %s", raw, grantProjectChoices(s.grantProjects(agent, g)))
	}
	return match, nil
}

// grantProjectChoices lists every project a refused call could use instead,
// so the agent can correct itself even when its tool list is out of date.
func grantProjectChoices(projects []grantProject) string {
	if len(projects) == 0 {
		return "Omit " + grantProjectParam + " to use this agent's own project."
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	return "It can use " + strings.Join(names, ", ") + ", or omit " + grantProjectParam + " for its own project."
}

func grantProjectDescription(home string, projects []grantProject) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The Apteva project to run this call in. Omit it for this agent's own project")
	if home != "" {
		fmt.Fprintf(&b, " (%s)", home)
	}
	b.WriteString(". Pass a project id or its exact name. Available: ")
	for i, p := range projects {
		if i == grantProjectListLimit {
			fmt.Fprintf(&b, " and %d more", len(projects)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%s)", p.Name, p.ID)
	}
	b.WriteString(".")
	return b.String()
}

// addGrantProjectParam describes project_id on every tool schema in a
// tools/list result, adding it where a tool had none. Tools outside a
// tool-limited grant are left unchanged. Tools readOnly accepts also offer
// "*"; a nil readOnly offers it nowhere.
func addGrantProjectParam(result map[string]any, description string, g *AgentProjectGrant, readOnly func(tool map[string]any) bool) {
	tools, _ := result["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if tool == nil || !g.toolAllowed(name) {
			continue
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object"}
			tool["inputSchema"] = schema
		}
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
			schema["properties"] = props
		}
		prop, _ := props[grantProjectParam].(map[string]any)
		if prop == nil {
			prop = map[string]any{"type": "string"}
			props[grantProjectParam] = prop
		}
		prop["description"] = description
		if readOnly != nil && readOnly(tool) {
			prop["description"] = description + ` Pass "*" to run it in this agent's project and every listed project at once; the reply has one entry per project.`
		}
	}
}

func toolReadOnly(tool map[string]any) bool {
	annotations, _ := tool["annotations"].(map[string]any)
	readOnly, _ := annotations["readOnlyHint"].(bool)
	return readOnly
}

// grantToolHint caches which of an install's tools declare the standard MCP
// readOnlyHint, the only tools "*" may run.
type grantToolHint struct {
	at       time.Time
	readOnly map[string]bool
}

func (s *Server) rememberToolHints(installID int64, tools []any) {
	hint := grantToolHint{at: time.Now(), readOnly: map[string]bool{}}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if name, _ := tool["name"].(string); name != "" && toolReadOnly(tool) {
			hint.readOnly[name] = true
		}
	}
	s.grantToolHints.Store(installID, hint)
}

// appToolReadOnly reads the hint from the app's own tool list, which the
// agent's last listing usually left cached.
func (s *Server) appToolReadOnly(entry *InstalledApp, tool string) (bool, error) {
	if manifestToolReadOnly(entry, tool) {
		return true, nil
	}
	if cached, ok := s.grantToolHints.Load(entry.InstallID); ok {
		if hint := cached.(grantToolHint); time.Since(hint.at) < grantToolHintTTL {
			return hint.readOnly[tool], nil
		}
	}
	token, err := s.appInstallToken(entry.InstallID)
	if err != nil {
		return false, err
	}
	listed, err := listAppMCPTools(entry.SidecarURL+"/mcp", token)
	if err != nil {
		return false, err
	}
	tools := make([]any, 0, len(listed))
	for _, t := range listed {
		tools = append(tools, map[string]any{"name": t.Name, "annotations": t.Annotations})
	}
	s.rememberToolHints(entry.InstallID, tools)
	cached, _ := s.grantToolHints.Load(entry.InstallID)
	return cached.(grantToolHint).readOnly[tool], nil
}

// manifestToolReadOnly reports read_only in the installed manifest, the
// declaration the SDK also lists as readOnlyHint. The listed annotation still
// covers apps that set the hint only in code.
func manifestToolReadOnly(entry *InstalledApp, tool string) bool {
	for _, spec := range entry.Manifest.Provides.MCPTools {
		if spec.Name == tool && spec.ReadOnly {
			return true
		}
	}
	return false
}

// projectGrantRoute is the proxy's decision for one granted MCP request:
// either a tools/call run in another project by the same global install, or
// a tools/list whose schemas gain the project parameter.
type projectGrantRoute struct {
	entry       *InstalledApp
	project     string
	listing     *AgentProjectGrant
	description string
	listed      func(tools []any)
	readOnly    func(tool map[string]any) bool
}

// projectGrantForAppMCP returns nil whenever no grant applies, leaving the
// proxy's normal routing untouched. handled=true means a tool error was sent.
func (s *Server) projectGrantForAppMCP(w http.ResponseWriter, r *http.Request, appName string) (*projectGrantRoute, bool) {
	// Only the server-signed agent identity in the attachment URL counts; the
	// caller header alone could be set by any holder of the app token.
	if r.URL.Query().Get("file_auth") == "" {
		return nil, false
	}
	agentID, err := s.fileReferenceAgent(r)
	if err != nil {
		return nil, false
	}
	// Grants run only through a global install, which serves every project.
	homeInstallID, _ := strconv.ParseInt(r.Header.Get("X-Apteva-App-Install-ID"), 10, 64)
	home := s.installedApps.Get(homeInstallID)
	if home == nil || home.AppName != appName || home.ProjectID != "" {
		return nil, false
	}
	g, err := s.store.GetAgentProjectGrant(agentID, appName)
	if err != nil || g == nil {
		return nil, false
	}
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil || agent == nil || agent.ProjectID == "" {
		return nil, false
	}

	body, err := readRequestBody(r)
	if err != nil {
		return nil, false
	}
	var rpc map[string]any
	if decodeMCPProxyRequest(body, &rpc) != nil {
		return nil, false
	}
	switch rpc["method"] {
	case "tools/list":
		// Core lists tools while an attachment is being saved, before the
		// binding is recorded, so listing only needs the signed identity.
		projects := s.grantProjects(agent, g)
		if len(projects) == 0 {
			return nil, false
		}
		homeName, _ := s.projectName(agent.ProjectID)
		return &projectGrantRoute{listing: g, description: grantProjectDescription(homeName, projects),
			listed: func(tools []any) { s.rememberToolHints(home.InstallID, tools) },
			readOnly: func(tool map[string]any) bool {
				name, _ := tool["name"].(string)
				return toolReadOnly(tool) || manifestToolReadOnly(home, name)
			}}, false
	case "tools/call":
		var enabled int
		if err := s.store.db.QueryRow(`SELECT enabled FROM app_agent_bindings WHERE install_id=? AND agent_id=?`, home.InstallID, agent.ID).Scan(&enabled); err != nil || enabled != 1 {
			return nil, false
		}
	default:
		return nil, false
	}
	params, _ := rpc["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	raw, present := args[grantProjectParam]
	if !present {
		return nil, false
	}
	// The app then sees the call exactly as one made inside the chosen
	// project: no project_id argument, and that project as _project_id.
	delete(args, grantProjectParam)
	rewritten, err := json.Marshal(rpc)
	if err != nil {
		return nil, false
	}
	restoreRequestBody(r, rewritten)
	value, _ := raw.(string)
	if strings.TrimSpace(value) == grantAllProjects {
		tool, _ := params["name"].(string)
		s.fanOutGrantCall(w, r, body, rpc, agent, g, home, tool)
		return nil, true
	}
	target, err := s.resolveGrantProject(agent, g, value)
	if err != nil {
		writePlatformGatewayToolError(w, body, err)
		return nil, true
	}
	if target == "" {
		return nil, false
	}
	tool, _ := params["name"].(string)
	if !g.toolAllowed(tool) {
		writePlatformGatewayToolError(w, body, fmt.Errorf("tool %q is not granted outside this agent's project", tool))
		return nil, true
	}
	log.Printf("[PROJECT-GRANT] agent=%d app=%s tool=%s project=%s->%s install=%d",
		agent.ID, appName, tool, agent.ProjectID, target, home.InstallID)
	return &projectGrantRoute{entry: home, project: target}, false
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	restoreRequestBody(r, body)
	return body, err
}

func setResponseBody(resp *http.Response, data []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
}

// rewriteToolList adds the project parameter to a JSON tools/list reply.
// Streaming replies keep their existing behaviour.
func (route *projectGrantRoute) rewriteToolList(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") ||
		resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	setResponseBody(resp, raw)
	if err != nil {
		return err
	}
	var reply map[string]any
	if decodeMCPProxyRequest(raw, &reply) != nil {
		return nil
	}
	result, _ := reply["result"].(map[string]any)
	if result == nil {
		return nil
	}
	tools, _ := result["tools"].([]any)
	if route.listed != nil {
		route.listed(tools)
	}
	addGrantProjectParam(result, route.description, route.listing, route.readOnly)
	encoded, err := json.Marshal(reply)
	if err != nil {
		return nil
	}
	setResponseBody(resp, encoded)
	return nil
}

// platformGrantProject moves a granted Apteva Server tool call to the project
// named in its project_id argument. It returns "" when the call stays home.
func (s *Server) platformGrantProject(agent *Agent, body []byte) (string, error) {
	g, err := s.store.GetAgentProjectGrant(agent.ID, platformMCPName)
	if err != nil || g == nil {
		return "", nil
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if decodeMCPProxyRequest(body, &rpc) != nil || rpc.Method != "tools/call" {
		return "", nil
	}
	value, _ := rpc.Params.Arguments["project_id"].(string)
	target, err := s.resolveGrantProject(agent, g, value)
	if err != nil || target == "" {
		return "", err
	}
	if !g.toolAllowed(rpc.Params.Name) {
		return "", fmt.Errorf("tool %q is not granted outside this agent's project", rpc.Params.Name)
	}
	log.Printf("[PROJECT-GRANT] agent=%d app=%s tool=%s project=%s->%s",
		agent.ID, platformMCPName, rpc.Params.Name, agent.ProjectID, target)
	return target, nil
}

// platformGrantToolList describes project_id on the Apteva Server tools a
// granted agent can run elsewhere.
func (s *Server) platformGrantToolList(agent *Agent, result map[string]any) {
	g, err := s.store.GetAgentProjectGrant(agent.ID, platformMCPName)
	if err != nil || g == nil {
		return
	}
	projects := s.grantProjects(agent, g)
	if len(projects) == 0 {
		return
	}
	homeName, _ := s.projectName(agent.ProjectID)
	addGrantProjectParam(result, grantProjectDescription(homeName, projects), g, nil)
}

// handleAgentProjectGrants serves /instances/:id/project-grants.
//
// GET returns the agent's grants and the projects the caller may name.
// PUT {"target","all_projects","project_ids","tools"} sets one grant; no
// projects and all_projects=false removes it, restoring the default.
func (s *Server) handleAgentProjectGrants(w http.ResponseWriter, r *http.Request, agentID int64) {
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil || agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	userID := getUserID(r)
	warning := ""
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		if agent.ProjectID == "" {
			http.Error(w, "global agents already work across projects", http.StatusBadRequest)
			return
		}
		var body struct {
			Target      string   `json:"target"`
			AllProjects bool     `json:"all_projects"`
			ProjectIDs  []string `json:"project_ids"`
			Tools       []string `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		target := strings.TrimSpace(body.Target)
		if target == "" || len(target) > 128 {
			http.Error(w, "target app is required", http.StatusBadRequest)
			return
		}
		projectIDs := cleanStringSet(body.ProjectIDs)
		removing := !body.AllProjects && len(projectIDs) == 0
		// Removing always works, so a grant left behind by a changed install
		// can still be cleared.
		if !removing && !containsExact(s.grantTargets(agent), target) {
			http.Error(w, target+" is not a globally installed app attached to this agent", http.StatusBadRequest)
			return
		}
		for _, id := range projectIDs {
			if id == agent.ProjectID {
				http.Error(w, "the agent's own project needs no grant", http.StatusBadRequest)
				return
			}
			name, ok := s.projectName(id)
			if !ok {
				http.Error(w, "project not found: "+id, http.StatusBadRequest)
				return
			}
			if s.effectiveRoleOnProject(userID, id).Rank() < ProjectEditor.Rank() {
				http.Error(w, "editor access required in project "+id, http.StatusForbidden)
				return
			}
			if !s.projectUsesGlobalInstall(target, id) {
				http.Error(w, name+" has its own "+target+" install", http.StatusBadRequest)
				return
			}
		}
		tools := cleanStringSet(body.Tools)
		for _, tool := range tools {
			if len(tool) > 128 {
				http.Error(w, "tool names must be at most 128 characters", http.StatusBadRequest)
				return
			}
		}
		if removing {
			err = s.store.DeleteAgentProjectGrant(agent.ID, target)
		} else {
			if body.AllProjects {
				projectIDs = []string{}
			}
			err = s.store.SaveAgentProjectGrant(AgentProjectGrant{
				AgentID: agent.ID, Target: target, AllProjects: body.AllProjects,
				ProjectIDs: projectIDs, Tools: tools, CreatedBy: userID,
			})
		}
		if err != nil {
			http.Error(w, "save grant failed", http.StatusInternalServerError)
			return
		}
		log.Printf("[PROJECT-GRANT] set agent=%d target=%s all=%v projects=%v tools=%v by=%d",
			agent.ID, target, body.AllProjects, projectIDs, tools, userID)
		if reloadErr := s.reloadGrantedAttachment(r.Context(), agent.ID, target); reloadErr != nil {
			log.Printf("[PROJECT-GRANT] reload agent=%d target=%s: %v", agent.ID, target, reloadErr)
			warning = "Saved. The agent sees the change after its next restart."
		}
	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		return
	}
	grants, err := s.store.ListAgentProjectGrants(agent.ID)
	if err != nil {
		http.Error(w, "load grants failed", http.StatusInternalServerError)
		return
	}
	projects := s.grantableProjects(userID, agent.ProjectID)
	// targets maps each attachment a grant can apply to onto the ids, among
	// projects, that it can reach.
	targets := map[string][]string{}
	for _, target := range s.grantTargets(agent) {
		ids := []string{}
		for _, p := range projects {
			if s.projectUsesGlobalInstall(target, p.ID) {
				ids = append(ids, p.ID)
			}
		}
		targets[target] = ids
	}
	reply := map[string]any{
		"grants":          grants,
		"home_project_id": agent.ProjectID,
		"projects":        projects,
		"targets":         targets,
	}
	if warning != "" {
		reply["warning"] = warning
	}
	writeJSON(w, reply)
}

// reloadGrantedAttachment makes a running agent reconnect the attachment a
// grant changed, so its tool list gains or drops the project parameter now.
// A stopped agent lists its tools afresh when it starts.
func (s *Server) reloadGrantedAttachment(ctx context.Context, agentID int64, target string) error {
	unlock := s.lockAgentConfig(agentID)
	defer unlock()
	port := s.agents.GetPort(agentID)
	if port == 0 {
		return nil
	}
	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return err
	}
	current, err := s.currentAgentMCPServers(inst, port)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range current {
		raw, _ := entry["url"].(string)
		u, err := url.Parse(raw)
		if err != nil || strings.TrimPrefix(u.Path, "/api") != "/apps/"+target+"/mcp" {
			continue
		}
		// Only the reconnect matters; the value is never read.
		q := u.Query()
		q.Set("project_grant_rev", strconv.FormatInt(time.Now().UnixNano(), 36))
		u.RawQuery = q.Encode()
		entry["url"] = u.String()
		changed = true
	}
	if !changed {
		return nil
	}
	body, err := json.Marshal(map[string]any{"mcp_servers": mcpMapsAsAny(current)})
	if err != nil {
		return err
	}
	resp, err := s.coreDoWithBootWaitContext(ctx, agentID, http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/config", port), body,
		s.agents.GetCoreAPIKey(agentID), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return s.syncAppBindingsFromMCPServers(agentID, inst.ProjectID, mcpMapsAsAny(current))
}

// grantTargets are what a grant can apply to: the Apteva Server tools and
// every app the agent has attached through its global install.
func (s *Server) grantTargets(agent *Agent) []string {
	out := []string{platformMCPName}
	rows, err := s.store.db.Query(`SELECT a.name FROM app_agent_bindings b
		JOIN app_installs i ON i.id=b.install_id JOIN apps a ON a.id=i.app_id
		WHERE b.agent_id=? AND b.enabled=1 AND COALESCE(i.project_id,'')=''
		ORDER BY a.name`, agent.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && !containsExact(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// grantableProjects are the other projects the caller can name in a grant.
func (s *Server) grantableProjects(userID int64, home string) []grantProject {
	projects, err := s.store.ListProjectsForUser(userID)
	if err != nil {
		return []grantProject{}
	}
	out := []grantProject{}
	for _, p := range projects {
		if p.ID != home && s.effectiveRoleOnProject(userID, p.ID).Rank() >= ProjectEditor.Rank() {
			out = append(out, grantProject{ID: p.ID, Name: p.Name})
		}
	}
	return out
}
