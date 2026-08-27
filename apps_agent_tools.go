package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

const (
	agentToolsTargetNotFound    = "target_agent_not_found"
	agentToolsCallerNotReady    = "caller_tools_not_ready"
	agentToolsAppNotDeclared    = "required_app_not_declared"
	agentToolsAppNotBound       = "required_app_not_bound"
	agentToolsMCPSurfaceMissing = "mcp_surface_missing"
	agentToolsScopeMismatch     = "scope_mismatch"
	agentToolsInvalidRequest    = "invalid_request"
)

type agentToolsProblem struct {
	status    int
	code      string
	message   string
	agentKind sdk.AgentKind
}

func (p *agentToolsProblem) Error() string { return p.message }

func writeAgentToolsProblem(w http.ResponseWriter, problem *agentToolsProblem) {
	if problem == nil {
		return
	}
	body := map[string]any{"code": problem.code, "error": problem.message}
	if problem.agentKind != "" {
		body["agent_kind"] = problem.agentKind
	}
	writeJSONStatus(w, problem.status, body)
}

func (s *Server) handleCallbackAgentTools(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodPost || len(parts) != 1 || parts[0] != "ensure-attached" {
		http.Error(w, "POST /agent-tools/ensure-attached only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermMCPAttach) {
		writeAgentToolsProblem(w, &agentToolsProblem{
			status: http.StatusForbidden, code: "permission_denied",
			message: "missing permission: " + string(sdk.PermMCPAttach),
		})
		return
	}

	var body sdk.EnsureAppToolsRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAgentToolsProblem(w, &agentToolsProblem{status: http.StatusBadRequest, code: agentToolsInvalidRequest, message: "invalid JSON"})
		return
	}
	hasID := body.AgentID > 0
	hasKind := strings.TrimSpace(string(body.AgentKind)) != ""
	if hasID == hasKind || body.AgentID < 0 {
		writeAgentToolsProblem(w, &agentToolsProblem{status: http.StatusBadRequest, code: agentToolsInvalidRequest, message: "exactly one of agent_id or agent_kind is required"})
		return
	}
	if hasKind && body.AgentKind != sdk.AgentKindPlatformHelper {
		writeAgentToolsProblem(w, &agentToolsProblem{status: http.StatusBadRequest, code: agentToolsInvalidRequest, message: fmt.Sprintf("unsupported agent_kind %q", body.AgentKind)})
		return
	}

	var target *Agent
	if body.AgentKind == sdk.AgentKindPlatformHelper {
		target, err = s.store.GetPlatformHelper(getUserID(r))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !platformHelperActivated(target)) {
			writeAgentToolsProblem(w, &agentToolsProblem{
				status: http.StatusConflict, code: agentToolsTargetNotFound,
				message: "Apteva Helper is not activated", agentKind: body.AgentKind,
			})
			return
		}
		if err != nil {
			http.Error(w, "load platform Helper: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		target, err = s.callbackAgentForInstall(r, installID, body.AgentID)
		if err != nil || target == nil || target.Kind == "platform_helper" {
			writeAgentToolsProblem(w, &agentToolsProblem{
				status: http.StatusNotFound, code: agentToolsTargetNotFound,
				message: "target agent was not found in the app installation scope",
			})
			return
		}
	}

	installIDs, mcpServerIDs, problem := s.resolveAgentToolsInstallMCPs(
		installID, body.IncludeRequiredApps, target.ProjectID, body.AgentKind,
	)
	if problem != nil {
		writeAgentToolsProblem(w, problem)
		return
	}

	var result sdk.EnsureAppToolsResult
	result.AgentID = target.ID
	result.AttachedInstallIDs = installIDs
	result.MCPServerIDs = mcpServerIDs
	if body.AgentKind == sdk.AgentKindPlatformHelper {
		result.Changed, result.Applied, result.AgentRunning, result.ResetThreads, err =
			s.ensurePlatformHelperMCPServerIDs(target, mcpServerIDs)
	} else {
		result.Changed, result.Applied, result.AgentRunning, err =
			s.ensureOrdinaryAgentMCPServerIDs(getUserID(r), target, mcpServerIDs)
	}
	if err != nil {
		http.Error(w, "attach app tools: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, result)
}

// resolveAgentToolsInstallMCPs converts the caller and explicitly requested
// requires.apps bindings into exact MCP inventory IDs. No caller-supplied
// install or MCP ids cross this authorization boundary.
func (s *Server) resolveAgentToolsInstallMCPs(callerInstallID int64, requested []string, targetProjectID string, targetKind sdk.AgentKind) ([]int64, []int64, *agentToolsProblem) {
	manifest, err := installManifest(s, callerInstallID)
	if err != nil {
		return nil, nil, &agentToolsProblem{status: http.StatusUnauthorized, code: agentToolsCallerNotReady, message: "calling app installation is not ready"}
	}
	declared := map[string]bool{}
	for _, dep := range manifest.Requires.Apps {
		declared[dep.Name] = true
	}
	seenNames := map[string]bool{}
	installIDs := []int64{callerInstallID}
	for _, rawName := range requested {
		name := strings.TrimSpace(rawName)
		if name == "" || seenNames[name] {
			return nil, nil, &agentToolsProblem{status: http.StatusBadRequest, code: agentToolsInvalidRequest, message: "include_required_apps must contain unique non-empty names"}
		}
		seenNames[name] = true
		if !declared[name] {
			return nil, nil, &agentToolsProblem{status: http.StatusForbidden, code: agentToolsAppNotDeclared, message: fmt.Sprintf("app %q is not declared in requires.apps", name)}
		}
		boundID := installBoundAppID(s, callerInstallID, name)
		if boundID <= 0 {
			return nil, nil, &agentToolsProblem{status: http.StatusConflict, code: agentToolsAppNotBound, message: fmt.Sprintf("required app %q is not bound and running", name)}
		}
		installIDs = append(installIDs, boundID)
	}

	seenInstalls := map[int64]bool{}
	uniqueInstallIDs := make([]int64, 0, len(installIDs))
	mcpServerIDs := make([]int64, 0, len(installIDs))
	for _, id := range installIDs {
		if seenInstalls[id] {
			continue
		}
		seenInstalls[id] = true
		var status, installProject string
		var serverID int64
		err := s.store.db.QueryRow(`
			SELECT i.status, COALESCE(i.project_id,''), COALESCE(m.id,0)
			FROM app_installs i
			LEFT JOIN mcp_servers m ON m.upstream_id=?
			WHERE i.id=?`, appMCPUpstreamID(id), id).Scan(&status, &installProject, &serverID)
		if err != nil || status != "running" {
			code := agentToolsAppNotBound
			message := fmt.Sprintf("app installation %d is not running", id)
			if id == callerInstallID {
				code = agentToolsCallerNotReady
				message = "calling app MCP tools are not ready; retry after activation"
			}
			return nil, nil, &agentToolsProblem{status: http.StatusConflict, code: code, message: message, agentKind: targetKind}
		}
		if targetKind == sdk.AgentKindPlatformHelper {
			if installProject != "" {
				return nil, nil, &agentToolsProblem{status: http.StatusForbidden, code: agentToolsScopeMismatch, message: fmt.Sprintf("app installation %d is project-scoped and cannot be attached to Helper", id), agentKind: targetKind}
			}
		} else if installProject != "" && installProject != targetProjectID {
			return nil, nil, &agentToolsProblem{status: http.StatusForbidden, code: agentToolsScopeMismatch, message: fmt.Sprintf("app installation %d belongs to project %q", id, installProject)}
		}
		if serverID <= 0 {
			code := agentToolsMCPSurfaceMissing
			message := fmt.Sprintf("app installation %d does not expose an MCP tool surface", id)
			if id == callerInstallID {
				code = agentToolsCallerNotReady
				message = "calling app MCP tools are not ready; retry after activation"
			}
			return nil, nil, &agentToolsProblem{status: http.StatusConflict, code: code, message: message, agentKind: targetKind}
		}
		uniqueInstallIDs = append(uniqueInstallIDs, id)
		mcpServerIDs = append(mcpServerIDs, serverID)
	}
	sort.Slice(uniqueInstallIDs, func(i, j int) bool { return uniqueInstallIDs[i] < uniqueInstallIDs[j] })
	sort.Slice(mcpServerIDs, func(i, j int) bool { return mcpServerIDs[i] < mcpServerIDs[j] })
	return uniqueInstallIDs, mcpServerIDs, nil
}

func mcpConfigListsEqual(left, right []map[string]any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

func int64ListsEqual(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (s *Server) ensureOrdinaryAgentMCPServerIDs(userID int64, agent *Agent, serverIDs []int64) (changed, applied, running bool, err error) {
	unlock := s.lockAgentConfig(agent.ID)
	defer unlock()
	port := s.agents.GetPort(agent.ID)
	current, err := s.currentAgentMCPServers(agent, port)
	if err != nil {
		return false, false, port > 0, err
	}
	selected, err := s.resolveAgentMCPConfigs(userID, agent, serverIDs)
	if err != nil {
		return false, false, port > 0, err
	}
	next := mutateMCPServers(current, selected, "add")
	return s.applyOrdinaryAgentMCPServersLocked(agent, port, current, next)
}

func (s *Server) applyOrdinaryAgentMCPServersLocked(agent *Agent, port int, current, next []map[string]any) (changed, applied, running bool, err error) {
	changed = !mcpConfigListsEqual(current, next)
	running = port > 0
	if changed {
		if running {
			body, _ := json.Marshal(map[string]any{"mcp_servers": mcpMapsAsAny(next)})
			url := fmt.Sprintf("http://127.0.0.1:%d/config", port)
			resp, requestErr := s.coreDoWithBootWait(agent.ID, http.MethodPut, url, body,
				s.agents.GetCoreAPIKey(agent.ID), http.Header{"Content-Type": []string{"application/json"}})
			if requestErr != nil {
				return changed, false, true, requestErr
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return changed, false, true, fmt.Errorf("core config HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			}
		} else if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
			cfg["mcp_servers"] = mcpMapsAsAny(next)
			return nil
		}); err != nil {
			return changed, false, false, err
		}
	}
	if err := s.syncAppBindingsFromMCPServers(agent.ID, agent.ProjectID, mcpMapsAsAny(next)); err != nil {
		return changed, false, running, err
	}
	return changed, true, running, nil
}

func (s *Server) ensurePlatformHelperMCPServerIDs(helper *Agent, addIDs []int64) (changed, applied, running bool, resetThreads int, err error) {
	unlock := s.lockAgentConfig(helper.ID)
	defer unlock()
	current, _, err := s.resolvePlatformHelperMCPs(helper.UserID, helperSelectedGlobalMCPServerIDs(helper), false)
	if err != nil {
		return false, false, false, 0, err
	}
	wanted := append([]int64{}, current...)
	wanted = append(wanted, addIDs...)
	wanted, _, err = s.resolvePlatformHelperMCPs(helper.UserID, wanted, true)
	if err != nil {
		return false, false, false, 0, err
	}
	changed = !int64ListsEqual(current, wanted)
	setHelperSelectedGlobalMCPServerIDs(helper, wanted)
	runtimeChanged, err := s.ensurePlatformHelperRuntimeConfig(helper)
	if err != nil {
		return changed, false, false, 0, err
	}
	running = s.agents.IsRunning(helper.ID)
	needsApply := changed || runtimeChanged || !s.platformHelperCapabilitiesApplied(helper, wanted)
	if runtimeChanged || changed {
		if err := s.store.UpdateAgent(helper); err != nil {
			return changed, false, running, 0, err
		}
	}
	if needsApply {
		if running {
			if err := s.applyPlatformHelperMCPConfig(helper); err != nil {
				return changed, false, true, 0, err
			}
			resetThreads, err = s.resetPlatformHelperConversationThreads(helper)
			if err != nil {
				return changed, false, true, resetThreads, err
			}
		} else if err := s.syncStoppedPlatformHelperMCPConfig(helper); err != nil {
			return changed, false, false, 0, err
		}
	}
	if err := s.syncAppBindingsFromMCPServers(helper.ID, "", helperConfiguredMCPServers(helper)); err != nil {
		return changed, false, running, resetThreads, err
	}
	return changed, true, running, resetThreads, nil
}

type appToolsDetachPlan struct {
	installID int64
	serverID  int64
	agentIDs  []int64
}

// captureAppToolsDetachPlan must run before uninstall deletes the inventory
// and derived binding rows. Helper selections are included even if an older
// server left their app_agent_bindings metadata out of sync.
func (s *Server) captureAppToolsDetachPlan(installID int64) appToolsDetachPlan {
	plan := appToolsDetachPlan{installID: installID}
	_ = s.store.db.QueryRow(`SELECT id FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(installID)).Scan(&plan.serverID)
	seen := map[int64]bool{}
	if rows, err := s.store.db.Query(`SELECT agent_id FROM app_agent_bindings WHERE install_id=? AND enabled=1`, installID); err == nil {
		for rows.Next() {
			var agentID int64
			if rows.Scan(&agentID) == nil && agentID > 0 && !seen[agentID] {
				seen[agentID] = true
				plan.agentIDs = append(plan.agentIDs, agentID)
			}
		}
		_ = rows.Close()
	}
	if plan.serverID > 0 {
		if rows, err := s.store.db.Query(`SELECT id, COALESCE(config,'{}') FROM agents WHERE kind='platform_helper'`); err == nil {
			for rows.Next() {
				var agentID int64
				var rawConfig string
				if rows.Scan(&agentID, &rawConfig) != nil || seen[agentID] {
					continue
				}
				helper := &Agent{Config: rawConfig}
				for _, selectedID := range helperSelectedGlobalMCPServerIDs(helper) {
					if selectedID == plan.serverID {
						seen[agentID] = true
						plan.agentIDs = append(plan.agentIDs, agentID)
						break
					}
				}
			}
			_ = rows.Close()
		}
	}
	sort.Slice(plan.agentIDs, func(i, j int) bool { return plan.agentIDs[i] < plan.agentIDs[j] })
	return plan
}

// detachUninstalledAppTools removes dead MCP references from live and stopped
// agents immediately after uninstall commits. The operation is best-effort:
// the database uninstall remains authoritative and startup reconciliation is a
// final safety net if a running Core is temporarily unreachable.
func (s *Server) detachUninstalledAppTools(plan appToolsDetachPlan) {
	if plan.installID <= 0 || len(plan.agentIDs) == 0 {
		return
	}
	for _, agentID := range plan.agentIDs {
		agent, err := s.store.GetAgentByID(agentID)
		if err != nil || agent == nil {
			continue
		}
		if agent.Kind == "platform_helper" {
			err = s.detachUninstalledAppFromHelper(agent, plan.serverID)
		} else {
			err = s.detachUninstalledAppFromOrdinaryAgent(agent, plan.installID)
		}
		if err != nil {
			log.Printf("[APPS-MCP] uninstall detach install=%d agent=%d: %v", plan.installID, agentID, err)
		}
	}
}

func (s *Server) detachUninstalledAppFromOrdinaryAgent(agent *Agent, installID int64) error {
	unlock := s.lockAgentConfig(agent.ID)
	defer unlock()
	port := s.agents.GetPort(agent.ID)
	current, err := s.currentAgentMCPServers(agent, port)
	if err != nil {
		return err
	}
	next := make([]map[string]any, 0, len(current))
	for _, config := range current {
		if appInstallIDFromMCPConfig(config) != installID {
			next = append(next, config)
		}
	}
	_, _, _, err = s.applyOrdinaryAgentMCPServersLocked(agent, port, current, next)
	return err
}

func (s *Server) detachUninstalledAppFromHelper(helper *Agent, serverID int64) error {
	unlock := s.lockAgentConfig(helper.ID)
	defer unlock()
	wanted := make([]int64, 0)
	for _, id := range helperSelectedGlobalMCPServerIDs(helper) {
		if id != serverID {
			wanted = append(wanted, id)
		}
	}
	setHelperSelectedGlobalMCPServerIDs(helper, wanted)
	if _, err := s.ensurePlatformHelperRuntimeConfig(helper); err != nil {
		return err
	}
	if err := s.store.UpdateAgent(helper); err != nil {
		return err
	}
	if s.agents.IsRunning(helper.ID) {
		if err := s.applyPlatformHelperMCPConfig(helper); err != nil {
			return err
		}
		if _, err := s.resetPlatformHelperConversationThreads(helper); err != nil {
			return err
		}
	} else if err := s.syncStoppedPlatformHelperMCPConfig(helper); err != nil {
		return err
	}
	return s.syncAppBindingsFromMCPServers(helper.ID, "", helperConfiguredMCPServers(helper))
}
