package main

// Keyless integration calls from app sidecars.
//
//   POST /api/apps/callback/integrations/public/:slug/execute
//
// Public data sources (GDELT, Kalshi market reads, Wikipedia, ...) need
// no credentials, so requiring a connection only to reach them made apps
// keep their own HTTP clients for those APIs, duplicating the catalog.
// This route runs a catalog tool with no credentials and no connection,
// through the same executor as a connection call: the tool's pacing,
// timeout, retries, environment simulation and usage events all apply.
//
// A tool is callable here when its integration's auth types include
// "none", or when the tool itself is marked "public" (a keyless read on
// an integration whose other tools need credentials, e.g. Polymarket's
// market listings). Anything else still needs a connection.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/internal/admission"
)

// publicIntegrationTool returns the tool if it can run without
// credentials, or a reason it cannot.
func publicIntegrationTool(app *AppTemplate, name string) (*AppToolDef, int, string) {
	if app == nil {
		return nil, http.StatusNotFound, "integration not in catalog"
	}
	if app.Kind != "" && app.Kind != "rest" {
		return nil, http.StatusForbidden, "integration kind " + app.Kind + " needs a connection"
	}
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == name || app.Slug+"_"+t.Name == name {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		return nil, http.StatusNotFound, "tool not found on integration: " + name
	}
	if !tool.Public && !contains(app.Auth.Types, "none") {
		return nil, http.StatusForbidden, fmt.Sprintf("integration %q needs credentials for %q: use a connection", app.Slug, tool.Name)
	}
	// A credential placeholder left in the URL means the tool cannot be
	// addressed without one, whatever the catalog says.
	base := app.BaseURL
	if tool.BaseURL != "" {
		base = tool.BaseURL
	}
	if strings.Contains(base+tool.Path, "{{") {
		return nil, http.StatusForbidden, fmt.Sprintf("tool %q needs credentials in its URL: use a connection", tool.Name)
	}
	return tool, 0, ""
}

func (s *Server) handleCallbackPublicIntegration(w http.ResponseWriter, r *http.Request, slug string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermConnectionsExecute) {
		log.Printf("[INTEGRATIONS-PUBLIC] DENY install=%d slug=%s reason=missing-permission", installID, slug)
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsExecute), http.StatusForbidden)
		return
	}
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	_, input := sanitizeIntegrationCallbackInput(body.Input)
	if containsFileReference(input) {
		writeFileProblem(w, fileProblem(400, "unsupported_file_argument", "public integration calls do not take file arguments"))
		return
	}
	app := s.catalog.Get(slug)
	tool, status, reason := publicIntegrationTool(app, body.Tool)
	if tool == nil {
		log.Printf("[INTEGRATIONS-PUBLIC] DENY install=%d slug=%s tool=%s reason=%q", installID, slug, body.Tool, reason)
		http.Error(w, reason, status)
		return
	}

	conn := &Connection{AppSlug: app.Slug, ProjectID: installProjectID(s, installID)}
	callerApp := s.callerAppName(installID)
	environmentID := r.Header.Get("X-Apteva-Environment-Id")
	credentials := map[string]string{}
	if environmentID == "" {
		if err := s.prepareIntegrationExternalFetch(app, tool, credentials, input); err != nil {
			s.recordIntegrationUsage(integrationUsageFromResult(conn, installID, callerApp, tool.Name, input, nil, err))
			writeJSON(w, map[string]any{"success": false, "data": err.Error()})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	permit, err := s.automaticRuntime().get().Acquire(ctx, admission.Request{Key: "integration:public:" + app.Slug, Operation: tool.Name, Caller: "integration"})
	if err != nil {
		writeAdmissionError(w, err)
		return
	}
	started := time.Now()
	result, err := executeIntegrationToolWithRefreshContext(ctx, app, tool, credentials, input, environmentID, nil)
	permit.Finish(admission.Result{Duration: time.Since(started), Failed: err != nil || result == nil || !result.Success,
		Canceled: ctx.Err() != nil, Overloaded: result != nil && (result.Status == 429 || result.Status == 503)})
	s.recordIntegrationUsage(integrationUsageFromResult(conn, installID, callerApp, tool.Name, input, result, err))
	if err != nil {
		if isAdmissionFailure(err) {
			writeAdmissionError(w, err)
			return
		}
		log.Printf("[INTEGRATIONS-PUBLIC] ERROR install=%d slug=%s tool=%s error=%s", installID, slug, tool.Name, truncate(err.Error(), 500))
		writeJSON(w, map[string]any{"success": false, "data": err.Error()})
		return
	}
	if result == nil {
		writeJSON(w, map[string]any{"success": true})
		return
	}
	writeJSON(w, result)
}
