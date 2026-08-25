package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const projectSetupPresetKind = "project_setup"

// ProjectSetupPresetDefinition is schema v2 of the project setup preset.
// Dashboard keeps the compact component list used by bundled schema-v1 files;
// DashboardLayout is the lossless representation used by captured presets.
type ProjectSetupPresetDefinition struct {
	Category        string                    `json:"category"`
	Match           []string                  `json:"match,omitempty"`
	Agents          []ProjectPresetAgent      `json:"agents"`
	Dashboard       []string                  `json:"dashboard,omitempty"`
	DashboardLayout []dashboardWidgetInstance `json:"dashboard_layout,omitempty"`
}

// Preset is the generic public envelope. New consumers add a kind and their
// own versioned definition without creating another top-level preset table.
type Preset struct {
	ID             string                       `json:"id"`
	Kind           string                       `json:"kind"`
	Scope          string                       `json:"scope"`
	Source         string                       `json:"source"`
	SchemaVersion  int                          `json:"schema_version"`
	Name           string                       `json:"name"`
	Description    string                       `json:"description"`
	OwnerID        int64                        `json:"owner_id,omitempty"`
	OwnerProjectID string                       `json:"owner_project_id,omitempty"`
	Revision       int                          `json:"revision,omitempty"`
	Definition     ProjectSetupPresetDefinition `json:"definition"`
	CreatedAt      *time.Time                   `json:"created_at,omitempty"`
	UpdatedAt      *time.Time                   `json:"updated_at,omitempty"`
}

type storedPreset struct {
	ID, ProjectID, Kind, Scope, Name, Description string
	UserID, SchemaVersion                         int64
	Revision                                      int64
	Definition                                    json.RawMessage
	CreatedAt, UpdatedAt                          time.Time
}

func scanStoredPreset(scanner interface{ Scan(...any) error }) (storedPreset, error) {
	var row storedPreset
	var definition, createdAt, updatedAt string
	err := scanner.Scan(&row.ID, &row.UserID, &row.ProjectID, &row.Kind, &row.Scope, &row.SchemaVersion, &row.Revision,
		&row.Name, &row.Description, &definition, &createdAt, &updatedAt)
	if err != nil {
		return row, err
	}
	row.Definition = json.RawMessage(definition)
	row.CreatedAt, _ = parseTime(createdAt)
	row.UpdatedAt, _ = parseTime(updatedAt)
	return row, nil
}

func (s *Store) listPresets(userID int64, kind string, admin ...bool) ([]storedPreset, error) {
	isAdmin := len(admin) > 0 && admin[0]
	rows, err := s.db.Query(`
		SELECT id,user_id,COALESCE(project_id,''),kind,scope,schema_version,revision,name,description,definition_json,created_at,updated_at
		FROM presets
		WHERE kind=? AND (?=1 OR scope='shared' OR (scope='personal' AND user_id=?) OR (scope='project' AND EXISTS(
			SELECT 1 FROM project_members pm WHERE pm.project_id=presets.project_id AND pm.user_id=?)))
		ORDER BY scope, name COLLATE NOCASE, id`, kind, isAdmin, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storedPreset
	for rows.Next() {
		row, err := scanStoredPreset(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) listProjectTemplates(projectID, kind string) ([]storedPreset, error) {
	rows, err := s.db.Query(`SELECT id,user_id,COALESCE(project_id,''),kind,scope,schema_version,revision,name,description,definition_json,created_at,updated_at
		FROM presets WHERE kind=? AND scope='project' AND project_id=? ORDER BY name COLLATE NOCASE,id`, kind, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storedPreset
	for rows.Next() {
		row, err := scanStoredPreset(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) getPreset(_ int64, id string) (storedPreset, error) {
	return scanStoredPreset(s.db.QueryRow(`
		SELECT id,user_id,COALESCE(project_id,''),kind,scope,schema_version,revision,name,description,definition_json,created_at,updated_at
		FROM presets WHERE id=?`, id))
}

func (s *Store) insertPreset(row storedPreset) (storedPreset, error) {
	_, err := s.db.Exec(`
		INSERT INTO presets(id,user_id,project_id,kind,scope,schema_version,revision,name,description,definition_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, row.ID, row.UserID, nullablePresetProjectID(row.ProjectID), row.Kind, row.Scope, row.SchemaVersion, 1,
		row.Name, row.Description, string(row.Definition))
	if err != nil {
		return storedPreset{}, err
	}
	return s.getPreset(row.UserID, row.ID)
}

func nullablePresetProjectID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) updatePreset(userID int64, row storedPreset) (storedPreset, error) {
	result, err := s.db.Exec(`
		UPDATE presets SET project_id=?,scope=?,schema_version=?,revision=revision+1,name=?,description=?,definition_json=?,updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, nullablePresetProjectID(row.ProjectID), row.Scope, row.SchemaVersion, row.Name, row.Description,
		string(row.Definition), row.ID)
	if err != nil {
		return storedPreset{}, err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return storedPreset{}, sql.ErrNoRows
	}
	return s.getPreset(userID, row.ID)
}

func (s *Store) deletePreset(_ int64, id string) error {
	result, err := s.db.Exec(`DELETE FROM presets WHERE id=?`, id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func presetFromStored(row storedPreset) (Preset, error) {
	var definition ProjectSetupPresetDefinition
	if row.Kind != projectSetupPresetKind {
		return Preset{}, fmt.Errorf("unsupported preset kind %q", row.Kind)
	}
	if err := json.Unmarshal(row.Definition, &definition); err != nil {
		return Preset{}, err
	}
	createdAt, updatedAt := row.CreatedAt, row.UpdatedAt
	return Preset{
		ID: row.ID, Kind: row.Kind, Scope: row.Scope, Source: "user",
		SchemaVersion: int(row.SchemaVersion), Name: row.Name, Description: row.Description,
		OwnerID: row.UserID, OwnerProjectID: row.ProjectID, Revision: int(row.Revision), Definition: definition, CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}, nil
}

func systemPresetEnvelope(preset ProjectPreset) Preset {
	return Preset{
		ID: preset.ID, Kind: projectSetupPresetKind, Scope: "system", Source: "system",
		SchemaVersion: 1, Name: preset.Name, Description: preset.Description,
		Definition: ProjectSetupPresetDefinition{
			Category: preset.Category, Match: preset.Match, Agents: preset.Agents, Dashboard: preset.Dashboard,
		},
	}
}

func projectPresetFromEnvelope(preset Preset) ProjectPreset {
	return ProjectPreset{
		ID: preset.ID, Kind: preset.Kind, Scope: preset.Scope, Source: preset.Source,
		SchemaVersion: preset.SchemaVersion, OwnerID: preset.OwnerID, OwnerProjectID: preset.OwnerProjectID, Revision: preset.Revision,
		Category: preset.Definition.Category, Name: preset.Name, Description: preset.Description,
		Match: preset.Definition.Match, Agents: preset.Definition.Agents,
		Dashboard: preset.Definition.Dashboard, DashboardLayout: preset.Definition.DashboardLayout,
	}
}

func validatePresetEnvelope(preset Preset) error {
	preset.Name = strings.TrimSpace(preset.Name)
	if preset.Kind != projectSetupPresetKind {
		return fmt.Errorf("unsupported preset kind %q", preset.Kind)
	}
	if preset.SchemaVersion != 2 {
		return fmt.Errorf("unsupported schema_version %d", preset.SchemaVersion)
	}
	if preset.Scope != "personal" && preset.Scope != "shared" && preset.Scope != "project" {
		return errors.New("scope must be personal, shared, or project")
	}
	if preset.Scope == "project" && preset.OwnerProjectID == "" {
		return errors.New("owner_project_id is required for project templates")
	}
	if preset.Name == "" || len(preset.Name) > 120 || len(preset.Description) > 1000 {
		return errors.New("name is required and preset text is too long")
	}
	project := projectPresetFromEnvelope(preset)
	if project.ID == "" {
		project.ID = "pending"
	}
	if err := validateProjectPreset(project); err != nil {
		return err
	}
	if len(preset.Definition.Agents) > 50 || len(preset.Definition.DashboardLayout) > 50 {
		return errors.New("preset may contain at most 50 agents and 50 widgets")
	}
	for _, widget := range preset.Definition.DashboardLayout {
		if widget.Component == "" || len(widget.Component) > 160 || (widget.Size != "half" && widget.Size != "full") {
			return errors.New("preset contains an invalid dashboard widget")
		}
	}
	raw, _ := json.Marshal(preset.Definition)
	if len(raw) > 256<<10 {
		return errors.New("preset definition is too large")
	}
	return nil
}

func presetSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == ' ' || r == '-' || r == '_':
			return '-'
		}
		return -1
	}, value)
	value = strings.Trim(strings.Join(strings.FieldsFunc(value, func(r rune) bool { return r == '-' }), "-"), "-")
	if value == "" {
		value = "preset"
	}
	if len(value) > 45 {
		value = strings.Trim(value[:45], "-")
	}
	return value
}

func (s *Store) availablePresetID(userID int64, scope, projectID, name string) string {
	prefix := "usr-" + i64s(userID) + "-"
	if scope == "shared" {
		prefix = "shared-"
	} else if scope == "project" {
		prefix = "tpl-" + presetSlug(projectID) + "-"
	}
	base := prefix + presetSlug(name)
	for suffix := int64(1); ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate += "-" + i64s(suffix)
		}
		var exists int
		if s.db.QueryRow(`SELECT 1 FROM presets WHERE id=?`, candidate).Scan(&exists) == sql.ErrNoRows {
			return candidate
		}
	}
}

func (s *Server) genericPresetCatalog(userID int64) ([]Preset, error) {
	bundled, err := loadProjectPresetCatalog()
	if err != nil {
		return nil, err
	}
	result := make([]Preset, 0, len(bundled.Presets))
	for _, preset := range bundled.Presets {
		result = append(result, systemPresetEnvelope(preset))
	}
	rows, err := s.store.listPresets(userID, projectSetupPresetKind, s.store.GetPlatformRole(userID) == PlatformAdmin)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		preset, err := presetFromStored(row)
		if err != nil {
			return nil, fmt.Errorf("preset %s: %w", row.ID, err)
		}
		result = append(result, preset)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Scope != result[j].Scope {
			order := map[string]int{"project": 0, "personal": 1, "shared": 2, "system": 3}
			return order[result[i].Scope] < order[result[j].Scope]
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

func (s *Server) projectPresetCatalog(userID int64) (projectPresetCatalog, error) {
	presets, err := s.genericPresetCatalog(userID)
	if err != nil {
		return projectPresetCatalog{}, err
	}
	catalog := projectPresetCatalog{ByID: map[string]ProjectPreset{}}
	for _, preset := range presets {
		project := projectPresetFromEnvelope(preset)
		catalog.Presets = append(catalog.Presets, project)
		catalog.ByID[project.ID] = project
	}
	return catalog, nil
}

func filterSystemTemplates(catalog []Preset) []Preset {
	result := make([]Preset, 0, len(catalog))
	for _, template := range catalog {
		if template.Source == "system" {
			result = append(result, template)
		}
	}
	return result
}

func (s *Server) projectTemplateCatalog(projectID string) ([]Preset, error) {
	bundled, err := loadProjectPresetCatalog()
	if err != nil {
		return nil, err
	}
	result := make([]Preset, 0, len(bundled.Presets))
	for _, preset := range bundled.Presets {
		result = append(result, systemPresetEnvelope(preset))
	}
	rows, err := s.store.listProjectTemplates(projectID, projectSetupPresetKind)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		preset, err := presetFromStored(row)
		if err != nil {
			return nil, err
		}
		result = append(result, preset)
	}
	return result, nil
}

type presetWriteRequest struct {
	Kind          string                        `json:"kind"`
	Scope         string                        `json:"scope"`
	SchemaVersion int                           `json:"schema_version"`
	Name          string                        `json:"name"`
	Description   string                        `json:"description"`
	Definition    *ProjectSetupPresetDefinition `json:"definition"`
}

type presetPatchRequest struct {
	Scope       *string                       `json:"scope"`
	Name        *string                       `json:"name"`
	Description *string                       `json:"description"`
	Definition  *ProjectSetupPresetDefinition `json:"definition"`
}

func (s *Server) createProjectTemplate(userID int64, projectID string, body presetWriteRequest) (Preset, error) {
	if body.Definition == nil {
		return Preset{}, errors.New("definition is required")
	}
	template := Preset{Kind: body.Kind, Scope: "project", Source: "user", SchemaVersion: body.SchemaVersion,
		Name: strings.TrimSpace(body.Name), Description: strings.TrimSpace(body.Description), OwnerProjectID: projectID, Definition: *body.Definition}
	if template.Kind == "" {
		template.Kind = projectSetupPresetKind
	}
	if template.SchemaVersion == 0 {
		template.SchemaVersion = 2
	}
	template.ID = s.store.availablePresetID(userID, template.Scope, projectID, template.Name)
	if err := validatePresetEnvelope(template); err != nil {
		return Preset{}, err
	}
	definition, _ := json.Marshal(template.Definition)
	row, err := s.store.insertPreset(storedPreset{ID: template.ID, UserID: userID, ProjectID: projectID, Kind: template.Kind,
		Scope: "project", SchemaVersion: int64(template.SchemaVersion), Name: template.Name, Description: template.Description, Definition: definition})
	if err != nil {
		return Preset{}, err
	}
	return presetFromStored(row)
}

// handleProjectTemplates owns custom template lifecycle. The owning project is
// explicit in the URL and controls read/write permissions; applying it to a
// different target remains a separate editor-authorized setup operation.
func (s *Server) handleProjectTemplates(w http.ResponseWriter, r *http.Request, projectID, rest string) {
	need := ProjectViewer
	if r.Method != http.MethodGet {
		need = ProjectEditor
	}
	userID, _, ok := s.requireProjectAccess(w, r, projectID, need)
	if !ok {
		return
	}
	if rest == "templates" {
		switch r.Method {
		case http.MethodGet:
			catalog, err := s.projectTemplateCatalog(projectID)
			if err != nil {
				http.Error(w, "load templates", http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"templates": catalog})
		case http.MethodPost:
			var body presetWriteRequest
			r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid template", http.StatusBadRequest)
				return
			}
			created, err := s.createProjectTemplate(userID, projectID, body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSONStatus(w, http.StatusCreated, created)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}
	if rest == "templates/capture" && r.Method == http.MethodPost {
		var body presetCaptureRequest
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid template", http.StatusBadRequest)
			return
		}
		body.ProjectID, body.Scope, body.OwnerProjectID = projectID, "project", projectID
		template, err := s.captureProjectPreset(userID, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		definition, _ := json.Marshal(template.Definition)
		row, err := s.store.insertPreset(storedPreset{ID: template.ID, UserID: userID, ProjectID: projectID, Kind: template.Kind,
			Scope: "project", SchemaVersion: 2, Name: template.Name, Description: template.Description, Definition: definition})
		if err != nil {
			http.Error(w, "create template", http.StatusConflict)
			return
		}
		created, _ := presetFromStored(row)
		writeJSONStatus(w, http.StatusCreated, created)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) canReadStoredPreset(userID int64, row storedPreset) bool {
	if s.store.GetPlatformRole(userID) == PlatformAdmin || row.Scope == "shared" || (row.Scope == "personal" && row.UserID == userID) {
		return true
	}
	if row.Scope != "project" || row.ProjectID == "" {
		return false
	}
	role, err := s.store.GetProjectRole(row.ProjectID, userID)
	return err == nil && role.Rank() >= ProjectViewer.Rank()
}

func (s *Server) canEditStoredPreset(userID int64, row storedPreset) bool {
	if s.store.GetPlatformRole(userID) == PlatformAdmin {
		return true
	}
	if row.Scope != "project" {
		return row.UserID == userID
	}
	role, err := s.store.GetProjectRole(row.ProjectID, userID)
	return err == nil && role.Rank() >= ProjectEditor.Rank()
}

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	switch r.Method {
	case http.MethodGet:
		catalog, err := s.genericPresetCatalog(userID)
		if err != nil {
			http.Error(w, "load presets", http.StatusInternalServerError)
			return
		}
		if r.URL.Query().Get("system_only") == "true" {
			catalog = filterSystemTemplates(catalog)
		}
		key := "presets"
		if strings.HasPrefix(r.URL.Path, "/templates") {
			key = "templates"
		}
		writeJSON(w, map[string]any{key: catalog})
	case http.MethodPost:
		if strings.HasPrefix(r.URL.Path, "/templates") {
			http.Error(w, "create custom templates under /projects/:id/templates", http.StatusBadRequest)
			return
		}
		var body presetWriteRequest
		r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Definition == nil {
			http.Error(w, "invalid preset", http.StatusBadRequest)
			return
		}
		preset := Preset{Kind: body.Kind, Scope: body.Scope, SchemaVersion: body.SchemaVersion,
			Name: strings.TrimSpace(body.Name), Description: strings.TrimSpace(body.Description), Definition: *body.Definition}
		if preset.Kind == "" {
			preset.Kind = projectSetupPresetKind
		}
		if preset.Scope == "" {
			preset.Scope = "personal"
		}
		if preset.SchemaVersion == 0 {
			preset.SchemaVersion = 2
		}
		if preset.Scope == "shared" && s.store.GetPlatformRole(userID) != PlatformAdmin {
			http.Error(w, "admin required for shared presets", http.StatusForbidden)
			return
		}
		preset.ID = s.store.availablePresetID(userID, preset.Scope, preset.OwnerProjectID, preset.Name)
		if err := validatePresetEnvelope(preset); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		definition, _ := json.Marshal(preset.Definition)
		row, err := s.store.insertPreset(storedPreset{ID: preset.ID, UserID: userID, Kind: preset.Kind,
			Scope: preset.Scope, SchemaVersion: int64(preset.SchemaVersion), Name: preset.Name,
			Description: preset.Description, Definition: definition})
		if err != nil {
			http.Error(w, "create preset", http.StatusConflict)
			return
		}
		created, _ := presetFromStored(row)
		writeJSONStatus(w, http.StatusCreated, created)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePresetByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/presets/")
	if strings.HasPrefix(r.URL.Path, "/templates/") {
		id = strings.TrimPrefix(r.URL.Path, "/templates/")
	}
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	userID := getUserID(r)
	row, err := s.store.getPreset(userID, id)
	if err != nil {
		bundled, loadErr := loadProjectPresetCatalog()
		system, found := bundled.ByID[id]
		if loadErr != nil || !found {
			http.Error(w, "preset not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, systemPresetEnvelope(system))
			return
		}
		http.Error(w, "system presets are read-only", http.StatusForbidden)
		return
	}
	if !s.canReadStoredPreset(userID, row) {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}
	preset, err := presetFromStored(row)
	if err != nil {
		http.Error(w, "invalid stored preset", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, preset)
	case http.MethodPatch:
		if !s.canEditStoredPreset(userID, row) {
			http.Error(w, "project editor required", http.StatusForbidden)
			return
		}
		var body presetPatchRequest
		r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.Name != nil {
			preset.Name = strings.TrimSpace(*body.Name)
		}
		if body.Description != nil {
			preset.Description = strings.TrimSpace(*body.Description)
		}
		if body.Scope != nil {
			preset.Scope = *body.Scope
		}
		if body.Definition != nil {
			preset.Definition = *body.Definition
		}
		if preset.Scope == "shared" && s.store.GetPlatformRole(userID) != PlatformAdmin {
			http.Error(w, "admin required for shared presets", http.StatusForbidden)
			return
		}
		if err := validatePresetEnvelope(preset); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		definition, _ := json.Marshal(preset.Definition)
		row.Scope, row.Name, row.Description, row.Definition = preset.Scope, preset.Name, preset.Description, definition
		updated, err := s.store.updatePreset(userID, row)
		if err != nil {
			http.Error(w, "update preset", http.StatusInternalServerError)
			return
		}
		result, _ := presetFromStored(updated)
		writeJSON(w, result)
	case http.MethodDelete:
		if !s.canEditStoredPreset(userID, row) {
			http.Error(w, "project editor required", http.StatusForbidden)
			return
		}
		if err := s.store.deletePreset(userID, id); err != nil {
			http.Error(w, "delete preset", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "GET, PATCH, or DELETE", http.StatusMethodNotAllowed)
	}
}

type presetCaptureRequest struct {
	ProjectID      string `json:"project_id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Category       string `json:"category"`
	Scope          string `json:"scope"`
	OwnerProjectID string `json:"-"`
}

func (s *Server) handlePresetCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body presetCaptureRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProjectID == "" {
		http.Error(w, "project_id is required", http.StatusBadRequest)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectEditor); !ok {
		return
	}
	userID := getUserID(r)
	if body.Scope == "shared" && s.store.GetPlatformRole(userID) != PlatformAdmin {
		http.Error(w, "admin required for shared presets", http.StatusForbidden)
		return
	}
	preset, err := s.captureProjectPreset(userID, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	definition, _ := json.Marshal(preset.Definition)
	row, err := s.store.insertPreset(storedPreset{ID: preset.ID, UserID: userID, Kind: preset.Kind,
		Scope: preset.Scope, SchemaVersion: 2, Name: preset.Name, Description: preset.Description,
		Definition: definition})
	if err != nil {
		http.Error(w, "create preset", http.StatusConflict)
		return
	}
	created, _ := presetFromStored(row)
	writeJSONStatus(w, http.StatusCreated, created)
}

func (s *Server) captureProjectPreset(userID int64, body presetCaptureRequest) (Preset, error) {
	project, err := s.store.GetProjectAny(body.ProjectID)
	if err != nil {
		return Preset{}, errors.New("project not found")
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = project.Name
	}
	category := body.Category
	if category == "" {
		category = "work"
	}
	scope := body.Scope
	if scope == "" {
		scope = "personal"
	}
	rows, err := s.store.db.Query(`SELECT id,name,directive,COALESCE(mode,'autonomous'),COALESCE(config,'{}')
		FROM agents WHERE project_id=? AND COALESCE(kind,'user')='user' ORDER BY id`, body.ProjectID)
	if err != nil {
		return Preset{}, err
	}
	defer rows.Close()
	var agents []ProjectPresetAgent
	keys := map[string]int{}
	for rows.Next() {
		var id int64
		var agent ProjectPresetAgent
		var configRaw string
		if err := rows.Scan(&id, &agent.Name, &agent.Directive, &agent.Mode, &configRaw); err != nil {
			return Preset{}, err
		}
		base := presetSlug(agent.Name)
		keys[base]++
		agent.Key = base
		if keys[base] > 1 {
			agent.Key += "-" + i64s(int64(keys[base]))
		}
		var config map[string]any
		if json.Unmarshal([]byte(configRaw), &config) == nil {
			agent.Unconscious, _ = config["unconscious"].(bool)
		}
		appRows, err := s.store.db.Query(`SELECT a.name FROM app_agent_bindings b
			JOIN app_installs i ON i.id=b.install_id JOIN apps a ON a.id=i.app_id
			WHERE b.agent_id=? AND b.enabled=1 ORDER BY a.name`, id)
		if err != nil {
			return Preset{}, err
		}
		for appRows.Next() {
			var app string
			if appRows.Scan(&app) == nil {
				agent.Apps = append(agent.Apps, app)
			}
		}
		appRows.Close()
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return Preset{}, err
	}
	document, _ := s.store.GetUserUILayoutWithRevision(userID)
	layout := resolvedDashboardHomeLayout(document, body.ProjectID)
	for i := range layout {
		layout[i].ID = "captured:" + i64s(int64(i+1))
	}
	preset := Preset{Kind: projectSetupPresetKind, Scope: scope, Source: "user", SchemaVersion: 2,
		Name: name, Description: strings.TrimSpace(body.Description), OwnerProjectID: body.OwnerProjectID,
		Definition: ProjectSetupPresetDefinition{Category: category, Agents: agents, DashboardLayout: layout}}
	preset.ID = s.store.availablePresetID(userID, scope, preset.OwnerProjectID, name)
	if err := validatePresetEnvelope(preset); err != nil {
		return Preset{}, err
	}
	return preset, nil
}
