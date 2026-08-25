package main

import (
	"encoding/json"
	"net/http"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

func (s *Server) callbackInstallCanReadProject(installID int64, projectID string) bool {
	var installProject string
	var installedBy int64
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,''),COALESCE(installed_by,0) FROM app_installs WHERE id=?`, installID).
		Scan(&installProject, &installedBy); err != nil {
		return false
	}
	if installProject != "" {
		return installProject == projectID
	}
	projects, err := s.store.ListProjects(installedBy)
	if err != nil {
		return false
	}
	for _, project := range projects {
		if project.ID == projectID {
			return true
		}
	}
	return false
}

func projectTemplateForApp(template Preset) (sdk.ProjectTemplate, error) {
	definition, err := json.Marshal(template.Definition)
	if err != nil {
		return sdk.ProjectTemplate{}, err
	}
	return sdk.ProjectTemplate{
		ID: template.ID, Kind: template.Kind, Name: template.Name, Description: template.Description,
		Source: template.Source, OwnerProjectID: template.OwnerProjectID,
		SchemaVersion: template.SchemaVersion, Revision: template.Revision, Definition: definition,
	}, nil
}

// GET /api/apps/callback/templates[/:id]?project_id=... is the read-only app
// projection of the template system. It never exposes owner user ids and it
// uses the install's approved permission snapshot plus its project projection.
func (s *Server) handleCallbackProjectTemplates(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermTemplatesRead) {
		http.Error(w, "platform.templates.read permission required", http.StatusForbidden)
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		http.Error(w, "project_id required", http.StatusBadRequest)
		return
	}
	if !s.callbackInstallCanReadProject(installID, projectID) {
		http.Error(w, "project is outside install scope", http.StatusForbidden)
		return
	}
	catalog, err := s.projectTemplateCatalog(projectID)
	if err != nil {
		http.Error(w, "load templates", http.StatusInternalServerError)
		return
	}
	requestedID := ""
	if len(parts) > 0 {
		requestedID = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 || strings.Contains(requestedID, "/") {
		http.NotFound(w, r)
		return
	}
	if requestedID != "" {
		for _, template := range catalog {
			if template.ID != requestedID {
				continue
			}
			out, err := projectTemplateForApp(template)
			if err != nil {
				http.Error(w, "encode template", http.StatusInternalServerError)
				return
			}
			writeJSON(w, out)
			return
		}
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}
	includeSystem := r.URL.Query().Get("include_system") == "true"
	out := make([]sdk.ProjectTemplate, 0, len(catalog))
	for _, template := range catalog {
		if template.Source == "system" && !includeSystem {
			continue
		}
		projected, err := projectTemplateForApp(template)
		if err != nil {
			http.Error(w, "encode template", http.StatusInternalServerError)
			return
		}
		out = append(out, projected)
	}
	writeJSON(w, map[string]any{"templates": out})
}
