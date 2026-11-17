package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

func manifestHasLocalDelivery(m *sdk.Manifest) bool {
	_, binary := m.Runtime.Binaries[localPlatform()]
	return m.Runtime.Kind == "static" || m.Runtime.Source != nil || hasLocalArtifact(m) || binary
}

type appReadiness struct {
	InstallID      int64    `json:"install_id"`
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	ProjectID      string   `json:"project_id"`
	Status         string   `json:"status"`
	State          string   `json:"state"`
	Phase          string   `json:"phase,omitempty"`
	OperationID    string   `json:"operation_id,omitempty"`
	Ready          bool     `json:"ready"`
	RuntimeReady   bool     `json:"runtime_ready"`
	ToolsAvailable bool     `json:"tools_available"`
	UIAvailable    bool     `json:"ui_available"`
	Widgets        []string `json:"widgets"`
	MissingSetup   []string `json:"missing_setup"`
	Error          string   `json:"error,omitempty"`
	Reused         bool     `json:"reused,omitempty"`
}

func (s *Server) installReadiness(id int64) (appReadiness, error) {
	return s.installReadinessContext(nil, id)
}
func (s *Server) installReadinessContext(parent context.Context, id int64) (appReadiness, error) {
	ctx, cancel := s.store.databaseContext(parent)
	defer cancel()
	out := appReadiness{InstallID: id, Widgets: []string{}, MissingSetup: []string{}}
	var raw, bindings, key, active string
	if err := s.store.dbQueryRowContext(ctx, `SELECT a.name,i.version,COALESCE(i.project_id,''),i.status,COALESCE(i.error_message,''),COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),COALESCE(i.integration_bindings,'{}'),i.installation_key,COALESCE(i.runtime_active_deployment,'') FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).Scan(&out.Name, &out.Version, &out.ProjectID, &out.Status, &out.Error, &raw, &bindings, &key, &active); err != nil {
		return out, err
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return out, err
	}
	out.State = out.Status
	out.RuntimeReady = out.Status == "running"
	if manifest.Runtime.Kind != "static" {
		out.RuntimeReady = out.RuntimeReady && s.installedApps != nil && s.installedApps.Get(id) != nil
	}
	out.Ready = out.RuntimeReady
	var operationRaw string
	err := s.store.dbQueryRowContext(ctx, `SELECT record_json FROM app_runtime_operations WHERE install_id=? AND kind IN ('deploy','upgrade','restart','activate','prepare') ORDER BY rowid DESC LIMIT 1`, id).Scan(&operationRaw)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil {
		var op runtimeOperation
		if err = json.Unmarshal([]byte(operationRaw), &op); err != nil {
			return out, err
		}
		out.OperationID, out.Phase = op.ID, op.Phase
		switch op.Status {
		case "queued", "running":
			out.State = op.Phase
			out.Ready = false
		case "failed", "cancelled":
			out.State = op.Status
			out.Error = op.Error
			out.Ready = false
		case "succeeded":
			out.Ready = out.Ready && active == op.DeploymentID
		}
	}
	cfg, err := decryptInstallConfig(s, id)
	if err != nil {
		return out, err
	}
	var bound map[string]any
	if err = json.Unmarshal([]byte(bindings), &bound); err != nil {
		return out, err
	}
	for _, field := range manifest.ConfigSchema {
		required := field.Required || field.RequiredIfRoleBound != "" && bound[field.RequiredIfRoleBound] != nil
		if required && (cfg[field.Name] == nil || strings.TrimSpace(fmt.Sprint(cfg[field.Name])) == "") && field.Default == "" {
			out.MissingSetup = append(out.MissingSetup, "config:"+field.Name)
		}
	}
	for _, role := range manifest.Requires.Integrations {
		if role.Required && bound[role.Role] == nil {
			out.MissingSetup = append(out.MissingSetup, "binding:"+role.Role)
		}
	}
	for _, dep := range manifest.Requires.Apps {
		if !dep.Optional && bound[dep.Name] == nil {
			out.MissingSetup = append(out.MissingSetup, "app:"+dep.Name)
		}
	}
	if len(out.MissingSetup) > 0 {
		out.Ready = false
		if out.State != "failed" && out.State != "cancelled" {
			out.State = "needs_configuration"
		}
	}
	if !out.RuntimeReady && !manifestHasLocalDelivery(&manifest) && out.OperationID == "" {
		out.MissingSetup = append(out.MissingSetup, "runtime:source_or_binary")
		out.State = "needs_configuration"
	}
	if out.Ready {
		out.State = "ready"
		out.Error = ""
	}
	out.ToolsAvailable = out.Ready && len(agentVisibleMCPTools(manifest.Provides.MCPTools)) > 0
	if out.ToolsAvailable {
		_, out.ToolsAvailable = s.storedAppMCPCapabilityRevision(id)
	}
	out.UIAvailable = out.Ready && (manifest.Runtime.Kind == "static" || manifest.Provides.UIApp != nil || len(manifest.Provides.UIPanels) > 0 || len(manifest.Provides.UIComponents) > 0)
	if out.Ready {
		for _, c := range manifest.Provides.UIComponents {
			if c.Entry != "" && (containsString(c.Slots, "dashboard.home") || containsString(c.Slots, "project.page")) {
				out.Widgets = append(out.Widgets, installationMCPName(out.Name, key, id)+":"+c.Name)
			}
		}
	}
	sort.Strings(out.Widgets)
	sort.Strings(out.MissingSetup)
	return out, nil
}

func (s *Server) handleAppReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", 405)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("install_id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "valid install_id required", 400)
		return
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectViewer); !ok {
		return
	}
	result, err := s.installReadinessContext(r.Context(), id)
	if err != nil {
		http.Error(w, "readiness unavailable", 503)
		return
	}
	writeJSON(w, result)
}

func (s *Server) writeInstallReadiness(w http.ResponseWriter, id int64, reused bool) {
	result, err := s.installReadiness(id)
	if err != nil {
		http.Error(w, fmt.Sprintf("installation %d exists; readiness unavailable", id), 503)
		return
	}
	result.Reused = reused
	writeJSON(w, result)
}

// Reuse is conservative: installing is never an implicit config change or upgrade.
func (s *Server) compatibleMCPInstall(id int64, manifest *sdk.Manifest, cfg map[string]string, bindings map[string]any, policy, repo, ref string) bool {
	saved, err := installManifest(s, id)
	if err != nil || runtimeJSON(saved) != runtimeJSON(manifest) {
		return false
	}
	if cfg != nil {
		existing, err := decryptInstallConfig(s, id)
		if err != nil || runtimeJSON(existing) != runtimeJSON(cfg) {
			return false
		}
	}
	var raw, savedPolicy, savedRepo, savedRef string
	if s.store.dbQueryRow(`SELECT integration_bindings,upgrade_policy,repo,ref FROM app_installs WHERE id=?`, id).Scan(&raw, &savedPolicy, &savedRepo, &savedRef) != nil {
		return false
	}
	if policy != "" && policy != savedPolicy {
		return false
	}
	if repo != "" && repo != savedRepo || ref != "" && ref != savedRef {
		return false
	}
	var existing map[string]any
	if json.Unmarshal([]byte(raw), &existing) != nil {
		return false
	}
	for k, v := range bindings {
		if runtimeJSON(existing[k]) != runtimeJSON(v) {
			return false
		}
	}
	return true
}
