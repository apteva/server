package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func (s *Store) migrateAppSetup() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_setup_choices (
 install_id INTEGER PRIMARY KEY REFERENCES app_installs(id) ON DELETE CASCADE,
 features_json TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	return err
}

type appSetupNode struct {
	InstallID    int64          `json:"install_id,omitempty"`
	FeatureID    string         `json:"feature_id,omitempty"`
	Role         string         `json:"role,omitempty"`
	Label        string         `json:"label"`
	Status       string         `json:"status"`
	Message      string         `json:"message,omitempty"`
	ProjectID    string         `json:"project_id,omitempty"`
	ConfigureURL string         `json:"configure_url,omitempty"`
	Children     []appSetupNode `json:"children,omitempty"`
}

func setupFeatures(m *sdk.Manifest) []sdk.SetupFeature {
	if m.Setup != nil {
		return m.Setup.Features
	}
	f := sdk.SetupFeature{ID: "essentials", Label: "Basic setup", Default: true}
	for _, d := range m.Requires.Integrations {
		if d.Required {
			f.Requires = append(f.Requires, sdk.SetupRequirement{Role: d.Role})
		}
	}
	for _, d := range m.Requires.Apps {
		if !d.Optional {
			f.Requires = append(f.Requires, sdk.SetupRequirement{Role: d.Name})
		}
	}
	for _, c := range m.ConfigSchema {
		if c.Required {
			f.Fields = append(f.Fields, c.Name)
		}
	}
	return []sdk.SetupFeature{f}
}
func (s *Server) setupChoices(id int64, m *sdk.Manifest) []string {
	var raw string
	if err := s.store.db.QueryRow(`SELECT features_json FROM app_setup_choices WHERE install_id=?`, id).Scan(&raw); err == nil {
		var values []string
		if json.Unmarshal([]byte(raw), &values) == nil {
			valid := []string{}
			for _, f := range setupFeatures(m) {
				if contains(values, f.ID) {
					valid = append(valid, f.ID)
				}
			}
			return valid
		}
	}
	values := []string{}
	for _, f := range setupFeatures(m) {
		if f.Default {
			values = append(values, f.ID)
		}
	}
	return values
}
func (s *Server) setupAccess(r *http.Request, project string) bool {
	uid := getUserID(r)
	if uid <= 0 {
		return false
	}
	if s.store.GetPlatformRole(uid) == PlatformAdmin {
		return true
	}
	return project != "" && s.effectiveRoleOnProject(uid, project).Rank() >= ProjectViewer.Rank()
}

// Access is checked at the router AND at every dependency traversal. This
// endpoint returns derived states only; never decrypted settings or tokens.
func (s *Server) handleAppSetup(w http.ResponseWriter, r *http.Request) {
	if getUserID(r) <= 0 || r.Header.Get("X-Apteva-App-Install-ID") != "" || r.Header.Get("X-Apteva-Subject-Type") != "" {
		http.Error(w, "a platform user session is required", http.StatusForbidden)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apps/installs/"), "/")
	if len(parts) != 2 || parts[1] != "setup" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid install", 400)
		return
	}
	need := ProjectViewer
	if r.Method == http.MethodPut {
		need = ProjectEditor
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, need); !ok {
		return
	}
	m, err := installManifest(s, id)
	if err != nil || m == nil {
		http.Error(w, "install not found", 404)
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Features []string `json:"features"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&body) != nil || body.Features == nil {
			http.Error(w, "features must be an array", 400)
			return
		}
		known := map[string]bool{}
		for _, f := range setupFeatures(m) {
			known[f.ID] = true
		}
		seen := map[string]bool{}
		for _, id := range body.Features {
			if !known[id] || seen[id] {
				http.Error(w, "unknown or duplicate feature", 400)
				return
			}
			seen[id] = true
		}
		raw, _ := json.Marshal(body.Features)
		_, err = s.store.db.Exec(`INSERT INTO app_setup_choices(install_id,features_json) VALUES(?,?) ON CONFLICT(install_id) DO UPDATE SET features_json=excluded.features_json,updated_at=CURRENT_TIMESTAMP`, id, string(raw))
		if err != nil {
			http.Error(w, "could not save setup choices", 500)
			return
		}
		writeJSON(w, map[string]any{"selected_features": body.Features})
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	selected := s.setupChoices(id, m)
	// A parent can ask to inspect one required child feature without changing
	// the shared dependency's saved choices.
	required := r.URL.Query().Get("feature")
	if required != "" {
		found := false
		for _, f := range setupFeatures(m) {
			if f.ID == required {
				found = true
			}
		}
		if !found {
			http.Error(w, "dependency does not declare this feature", 400)
			return
		}
		if !contains(selected, required) {
			selected = append(selected, required)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	walk := &setupWalk{s: s, r: r.WithContext(ctx), budget: 64, cache: map[string]appSetupNode{}}
	nodes := []appSetupNode{}
	for _, f := range setupFeatures(m) {
		if contains(selected, f.ID) {
			nodes = append(nodes, walk.feature(id, f.ID, "", map[string]bool{}))
		} else {
			nodes = append(nodes, appSetupNode{InstallID: id, FeatureID: f.ID, Label: f.Label, Status: "not_enabled"})
		}
	}
	var project, status string
	_ = s.store.db.QueryRow(`SELECT COALESCE(project_id,''),status FROM app_installs WHERE id=?`, id).Scan(&project, &status)
	writeJSON(w, map[string]any{"features": setupFeatures(m), "selected_features": s.setupChoices(id, m), "required_feature": required, "nodes": nodes, "project_id": project, "install_status": status, "shared": project == ""})
}

type setupWalk struct {
	s      *Server
	r      *http.Request
	budget int
	cache  map[string]appSetupNode
}

func (v *setupWalk) feature(id int64, feature, parentProject string, path map[string]bool) appSetupNode {
	n := appSetupNode{InstallID: id, FeatureID: feature, Label: feature, Status: "needs_setup"}
	var project, status string
	if v.s.store.db.QueryRow(`SELECT COALESCE(project_id,''),status FROM app_installs WHERE id=?`, id).Scan(&project, &status) != nil || (!v.s.setupAccess(v.r, project) && !(project == "" && parentProject != "" && v.s.setupAccess(v.r, parentProject))) || (parentProject != "" && project != "" && project != parentProject) {
		n.Message = "Dependency is unavailable or outside your access."
		return n
	}
	effectiveProject := project
	if effectiveProject == "" {
		effectiveProject = parentProject
	}
	key := fmt.Sprintf("%d:%s:%s", id, feature, effectiveProject)
	if path[key] {
		n.Status = "error"
		n.Message = "Circular setup dependency."
		return n
	}
	if cached, ok := v.cache[key]; ok {
		return cached
	}
	if v.budget <= 0 || len(path) >= 12 || v.r.Context().Err() != nil {
		n.Status = "pending"
		n.Message = "Setup check limit reached. Open this dependency to continue."
		return n
	}
	v.budget--
	path[key] = true
	defer delete(path, key)
	m, err := installManifest(v.s, id)
	if err != nil || m == nil {
		n.Message = "Dependency manifest unavailable."
		return n
	}
	var f *sdk.SetupFeature
	fs := setupFeatures(m)
	for i := range fs {
		if fs[i].ID == feature {
			f = &fs[i]
			break
		}
	}
	if f == nil {
		n.Message = "This dependency version does not declare the required feature."
		return n
	}
	n.Label = f.Label
	n.ProjectID = project
	if f.Configure != nil && sdk.ValidSetupPath(f.Configure.Entry) {
		n.ConfigureURL = "/api/apps/" + url.PathEscape(m.Name) + f.Configure.Entry + "?install_id=" + strconv.FormatInt(id, 10) + setupProjectQuery(effectiveProject)
	}
	if status != "running" {
		n.Message = "App is " + status + ". Start it before checking setup."
		return n
	}
	n.Status = "configured"
	bindings := bindingsForInstall(v.s, id)
	roles := v.s.buildPreflightRoles(m, project, getUserID(v.r))
	requirements := append([]sdk.SetupRequirement{}, f.Requires...)
	for _, role := range roles {
		if role.Required || (m.Setup == nil && appBindingIsSet(bindings[role.Role])) {
			found := false
			for _, req := range requirements {
				if req.Role == role.Role {
					found = true
				}
			}
			if !found {
				requirements = append(requirements, sdk.SetupRequirement{Role: role.Role})
			}
		}
	}
	for _, req := range requirements {
		child := appSetupNode{Role: req.Role, Label: req.Role, Status: "needs_setup", Message: "Choose a connection."}
		var role *preflightRole
		for i := range roles {
			if roles[i].Role == req.Role {
				role = &roles[i]
				break
			}
		}
		if role == nil {
			child.Message = "Required role is no longer declared."
			n.Children = append(n.Children, child)
			continue
		}
		if role.Label != "" {
			child.Label = role.Label
		}
		ids, _ := appBindingIDs(bindings[req.Role])
		if len(ids) == 0 {
			n.Children = append(n.Children, child)
			continue
		}
		child.Status = "configured"
		child.Message = ""
		for _, target := range ids {
			if role.Kind == "integration" {
				match := false
				for _, c := range role.IntegrationCands {
					if c.ConnectionID == target && c.Status == "active" {
						match = true
					}
				}
				if !match {
					child.Status = "needs_setup"
					child.Message = "Connection is unavailable or needs authentication."
				}
			} else {
				match := false
				for _, c := range role.AppCands {
					if c.InstallID == target {
						match = true
					}
				}
				if !match {
					child.Status = "needs_setup"
					child.Message = "Linked app is unavailable or incompatible."
					continue
				}
				dep, err := installManifest(v.s, target)
				if err != nil || dep == nil {
					child.Status = "needs_setup"
					child.Message = "Linked app manifest unavailable."
					continue
				}
				wanted := []string{req.Feature}
				if req.Feature == "" {
					wanted = v.s.setupChoices(target, dep)
				}
				// Even with all optional features off, an installed app must satisfy
				// its mandatory roles/config. Legacy apps expose essentials for this.
				if len(wanted) == 0 {
					child.Status = "needs_setup"
					child.Message = "Choose a feature in the linked app."
				}
				for _, fid := range wanted {
					sub := v.feature(target, fid, effectiveProject, path)
					child.Children = append(child.Children, sub)
					if setupBlocked(sub.Status) {
						child.Status = sub.Status
					}
				}
			}
		}
		n.Children = append(n.Children, child)
	}
	cfg, err := decryptInstallConfig(v.s, id)
	if err != nil {
		n.Status = "error"
		n.Message = "Could not read configuration."
		return n
	}
	for _, field := range m.ConfigSchema {
		required := field.Required || contains(f.Fields, field.Name) || (field.RequiredIfRoleBound != "" && appBindingIsSet(bindings[field.RequiredIfRoleBound]))
		if !required {
			continue
		}
		value, ok := cfg[field.Name]
		if !ok {
			value = field.Default
		}
		if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
			label := field.Label
			if label == "" {
				label = field.Name
			}
			n.Children = append(n.Children, appSetupNode{Label: label, Status: "needs_setup", Message: "Fill in this setting."})
		}
	}
	for _, c := range n.Children {
		if setupBlocked(c.Status) {
			n.Status = "needs_setup"
		}
	}
	if n.Status == "configured" && f.Readiness != nil {
		check := v.check(id, effectiveProject, f.Readiness.Route)
		n.Status = check.Status
		n.Message = check.Message
	} else if n.Status == "configured" {
		n.Message = "Connections and settings are configured; this app has no live verification for this feature."
	}
	v.cache[key] = n
	return n
}
func setupBlocked(status string) bool { return status != "ready" && status != "configured" }
func (v *setupWalk) check(id int64, project, route string) sdk.SetupStatus {
	fail := sdk.SetupStatus{Status: "error", Message: "App readiness check failed. Try again or open the app setup."}
	if v.r.Context().Err() != nil {
		return sdk.SetupStatus{Status: "pending", Message: "Readiness check timed out. Try again."}
	}
	if !sdk.ValidSetupPath(route) || v.s.installedApps == nil {
		return fail
	}
	inst := v.s.installedApps.Get(id)
	if inst == nil || inst.SidecarURL == "" {
		return sdk.SetupStatus{Status: "pending", Message: "App is starting or unavailable."}
	}
	token, err := v.s.appInstallToken(id)
	if err != nil {
		return fail
	}
	ctx, cancel := context.WithTimeout(v.r.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(inst.SidecarURL, "/")+route, nil)
	if err != nil {
		return fail
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-Project-ID", project)
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(id, 10))
	setTrustedAppPrincipalHeaders(req, token, v.s.trustedAppPrincipal(v.r, project))
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fail
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fail
	}
	var out sdk.SetupStatus
	if json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&out) != nil {
		return fail
	}
	switch out.Status {
	case "ready", "needs_setup", "pending", "error":
	default:
		return fail
	}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512]
	}
	return out
}

func setupProjectQuery(project string) string {
	if project == "" {
		return ""
	}
	return "&project_id=" + url.QueryEscape(project)
}
