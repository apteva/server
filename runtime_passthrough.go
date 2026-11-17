package main

// runtime_passthrough.go — the boundary between a runtime and the apps
// outside it.
//
// Apps installed in an environment may call only apps installed in the same
// environment. The one exception is a grant on the runtime (app, tool and
// optional fixed arguments): such a call is forwarded to that app's install
// in the runtime's source project, only for read-only tools, only while the
// runtime's creator can still see that project. Each forwarded call is
// recorded as an edge call and published as a runtime.passthrough event, and
// follows the runtime's network mode: record mode stores the result in the
// cassette, replay mode answers from it without touching the source project.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const (
	maxRuntimeAppGrants         = 32
	maxRuntimeAppGrantArgsBytes = 4 << 10
	runtimePassthroughMethod    = "TOOL"
)

// validateRuntimeAppGrants checks grants from a runtime create request.
func validateRuntimeAppGrants(grants []sdk.RuntimeAppGrant) ([]sdk.RuntimeAppGrant, error) {
	if len(grants) > maxRuntimeAppGrants {
		return nil, fmt.Errorf("at most %d app grants", maxRuntimeAppGrants)
	}
	out := make([]sdk.RuntimeAppGrant, 0, len(grants))
	for _, g := range grants {
		g.App, g.Tool = strings.TrimSpace(g.App), strings.TrimSpace(g.Tool)
		if g.App == "" || g.Tool == "" {
			return nil, fmt.Errorf("app grant needs app and tool")
		}
		if strings.HasPrefix(g.App, "component:") || strings.ContainsAny(g.App+g.Tool, "/ ") {
			return nil, fmt.Errorf("invalid app grant %s.%s", g.App, g.Tool)
		}
		if raw, err := json.Marshal(g.Arguments); err != nil || len(raw) > maxRuntimeAppGrantArgsBytes {
			return nil, fmt.Errorf("app grant %s.%s arguments are invalid or too large", g.App, g.Tool)
		}
		if _, ok := g.Arguments["_project_id"]; ok {
			return nil, fmt.Errorf("app grant %s.%s may not fix _project_id", g.App, g.Tool)
		}
		out = append(out, g)
	}
	return out, nil
}

// appGrantFor returns the grant that admits tool with input, if any.
func (w *Environment) appGrantFor(app, tool string, input map[string]any) (sdk.RuntimeAppGrant, bool) {
	w.mu.Lock()
	grants := w.appGrants
	w.mu.Unlock()
	for _, g := range grants {
		if g.App != app || g.Tool != tool {
			continue
		}
		if fixedArgumentsMatch(g.Arguments, input) {
			return g, true
		}
	}
	return sdk.RuntimeAppGrant{}, false
}

// fixedArgumentsMatch compares as JSON values, so 3 and 3.0 match and a map
// key order never matters.
func fixedArgumentsMatch(fixed, input map[string]any) bool {
	for key, want := range fixed {
		got, ok := input[key]
		if !ok || !sameJSONValue(want, got) {
			return false
		}
	}
	return true
}

func sameJSONValue(a, b any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	var va, vb any
	if json.Unmarshal(ra, &va) != nil || json.Unmarshal(rb, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// ForInstall returns the live environment that owns installID, if any.
func (wm *EnvironmentManager) ForInstall(installID int64) *Environment {
	if wm == nil || installID <= 0 {
		return nil
	}
	for _, env := range wm.List() {
		for _, name := range env.InstallNames() {
			if inst, ok := env.Install(name); ok && inst.InstallID == installID {
				return env
			}
		}
	}
	return nil
}

func writeAppNotInRuntime(w http.ResponseWriter, app, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   sdk.RuntimeErrAppNotInRuntime,
		"message": fmt.Sprintf("app %q is not installed in this runtime%s", app, detail),
	})
}

// runtimeCallBoundary refuses a call from an environment install to any
// install outside its environment. It reads install rows rather than the
// live environment list, so it also holds while the environment is being
// created. Owned components resolve through their owner check instead.
func (s *Server) runtimeCallBoundary(w http.ResponseWriter, callerInstallID int64, targetApp string, selected int64) bool {
	if s.store == nil || strings.HasPrefix(targetApp, "component:") {
		return true
	}
	caller, err := s.appMetadata(callerInstallID)
	if err != nil || caller.source != "environment" || caller.project == "" {
		return true // not an environment install
	}
	projectID := caller.project
	var n int
	if selected > 0 {
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE id = ? AND project_id = ? AND source = 'environment'`, selected, projectID).Scan(&n)
	} else {
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs i JOIN apps a ON a.id = i.app_id
			WHERE a.name = ? AND i.project_id = ? AND i.source = 'environment'`, targetApp, projectID).Scan(&n)
	}
	if n == 0 {
		writeAppNotInRuntime(w, targetApp, "")
		return false
	}
	return true
}

// serveRuntimePassthrough answers a call from a runtime app to an app that is
// not installed in the runtime. It returns false when the caller is not a
// runtime app or the target is installed in the runtime, so the regular
// app-call path handles it.
func (s *Server) serveRuntimePassthrough(w http.ResponseWriter, r *http.Request, callerInstallID int64, app, tool string, selected int64, input map[string]any) bool {
	if s.environments == nil || s.store == nil || strings.HasPrefix(app, "component:") {
		return false
	}
	// Most callers are not runtime installs; the cached metadata says so
	// without walking the live environments.
	if caller, err := s.appMetadata(callerInstallID); err != nil || caller.source != "environment" {
		return false
	}
	runtime := s.environments.ForInstall(callerInstallID)
	if runtime == nil {
		return false
	}
	if _, installed := runtime.Install(app); installed {
		return false
	}
	// A grant widens where a call may go, never who may call.
	if caller, err := s.appMetadata(callerInstallID); err != nil || !caller.permissions[sdk.PermAppsCall] {
		http.Error(w, "missing permission: "+string(sdk.PermAppsCall), http.StatusForbidden)
		return true
	}
	if selected > 0 {
		writeAppNotInRuntime(w, app, "")
		return true
	}
	delete(input, "_project_id")
	if _, ok := runtime.appGrantFor(app, tool, input); !ok {
		writeAppNotInRuntime(w, app, " and "+app+"."+tool+" is not granted with these arguments")
		return true
	}
	sourceProject := runtime.ProjectID
	if role := s.effectiveRoleOnProject(runtime.CreatorUserID(), sourceProject); role.Rank() < ProjectViewer.Rank() {
		http.Error(w, "runtime creator no longer has access to the source project", http.StatusForbidden)
		return true
	}
	var target *InstalledApp
	if s.installedApps != nil {
		target = s.installedApps.GetByNameAndProject(app, sourceProject)
	}
	if target == nil || target.SidecarURL == "" || (target.ProjectID != "" && target.ProjectID != sourceProject) {
		http.Error(w, "granted app is not available in the source project: "+app, http.StatusServiceUnavailable)
		return true
	}
	readOnly, err := s.appToolReadOnly(target, tool)
	if err != nil {
		http.Error(w, "check granted tool: "+err.Error(), http.StatusBadGateway)
		return true
	}
	if !readOnly {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "passthrough_not_read_only", "message": app + "." + tool + " is not declared read-only"})
		return true
	}
	if err := s.projectArgConflict(input, sourceProject); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return true
	}
	injectProjectArgAny(input, sourceProject)

	key, _ := json.Marshal(map[string]any{"tool": tool, "input": input})
	host, path := "app:"+app, "/"+tool
	edge := runtime.Edge()
	call := InterceptedCall{Host: host, Path: path, Method: runtimePassthroughMethod, Allowed: true, ReqBody: truncate(string(key), 2048), Timestamp: time.Now()}
	if runtime.clock != nil {
		call.LogicalTime = runtime.clock.Now()
	}
	var status int
	var respBody []byte
	contentType := "application/json"
	replayed := false
	if edge != nil && edge.mode == EdgeReplay {
		ent, ok := edge.cassette.lookup(runtimePassthroughMethod, host, path, key)
		if !ok {
			call.Blocked, call.Status = true, http.StatusBadGateway
			edge.record(call)
			http.Error(w, "no recorded result for "+app+"."+tool+" in the runtime cassette", http.StatusBadGateway)
			return true
		}
		status, respBody, replayed = ent.Status, []byte(ent.Body), true
		if ct := ent.Headers["Content-Type"]; ct != "" {
			contentType = ct
		}
	} else {
		status, contentType, respBody, err = s.forwardPassthroughCall(r, target, tool, input)
		if err != nil {
			call.Blocked, call.Status = true, http.StatusBadGateway
			if edge != nil {
				edge.record(call)
			}
			http.Error(w, "granted app unreachable: "+err.Error(), http.StatusBadGateway)
			return true
		}
		if edge != nil && edge.mode == EdgeRecord && edge.cassette != nil {
			edge.cassette.put(runtimePassthroughMethod, host, path, key, status, map[string]string{"Content-Type": contentType}, respBody)
			call.Recorded = true
		}
	}
	call.Status, call.RespBody = status, truncate(string(respBody), 2048)
	if edge != nil {
		edge.record(call)
	}
	if s.appBus != nil {
		data, _ := json.Marshal(map[string]any{
			"runtime_id": runtime.ID, "caller_install_id": callerInstallID,
			"app": app, "tool": tool, "source_project_id": sourceProject,
			"target_install_id": target.InstallID, "status": status, "replayed": replayed,
		})
		s.appBus.Publish("runtime", runtime.ID, callerInstallID, "runtime.passthrough", data)
	}
	log.Printf("[RUNTIME] passthrough runtime=%s caller=%d %s.%s status=%d replayed=%v", runtime.ID, callerInstallID, app, tool, status, replayed)
	if status >= 200 && status < 300 && r.Header.Get(sdk.HeaderAppCallResult) == "inner" && r.Header.Get(sdk.HeaderAppResultFormat) != sdk.AppResultJSON {
		respBody = unwrapAppCallResult(respBody)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
	return true
}

// forwardPassthroughCall runs tools/call on the source project's install. It
// sends no bound-caller identity: the caller belongs to another project, so
// the target treats the call like any public tool call, never app-only.
func (s *Server) forwardPassthroughCall(r *http.Request, target *InstalledApp, tool string, input map[string]any) (int, string, []byte, error) {
	rpc, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": input},
	})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.SidecarURL+"/mcp", bytes.NewReader(rpc))
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}
	resp, err := s.appCallbackClient().Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAppCallResponseBytes+1))
	if err != nil {
		return 0, "", nil, err
	}
	if len(body) > maxAppCallResponseBytes {
		return 0, "", nil, fmt.Errorf("app response too large")
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, nil
}
