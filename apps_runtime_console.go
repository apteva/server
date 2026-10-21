package main

// Operator-facing placement discovery and status. No provider names or resource
// type switches belong here: execution apps supply their own target catalog.
import (
	"bytes"
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

type runtimeTarget struct {
	Ref       runtimeResourceRef `json:"ref"`
	Name      string             `json:"name"`
	Status    string             `json:"status"`
	Available bool               `json:"available"`
	Reason    string             `json:"reason,omitempty"`
	Platform  string             `json:"platform,omitempty"`
	Region    string             `json:"region,omitempty"`
}
type runtimeTargetPage struct {
	Targets    []runtimeTarget `json:"targets"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// A compatibility adapter for existing execution apps. The operator explicitly
// registers a same-install private GET route; new backends expose GET /targets.
type runtimeTargetDiscovery struct {
	Path         string `json:"path"`
	Items        string `json:"items,omitempty"`
	IDField      string `json:"id_field"`
	NameField    string `json:"name_field"`
	StatusField  string `json:"status_field,omitempty"`
	ReadyValue   string `json:"ready_value,omitempty"`
	ResourceType string `json:"resource_type"`
}
type runtimeDeploymentSummary struct {
	Type           string             `json:"type"`
	Managed        bool               `json:"managed"`
	Backend        *runtimeBackendRef `json:"backend,omitempty"`
	BackendName    string             `json:"backend_name,omitempty"`
	Target         runtimeResourceRef `json:"target"`
	TargetName     string             `json:"target_name,omitempty"`
	State          string             `json:"state"`
	Message        string             `json:"message,omitempty"`
	CheckedAt      string             `json:"checked_at,omitempty"`
	AdmissionState string             `json:"admission_state,omitempty"`
	PublicURL      string             `json:"public_url,omitempty"`
}

func (s *Store) migrateRuntimeConsole() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS app_runtime_install_receipts (user_id INTEGER NOT NULL,request_key TEXT NOT NULL,fingerprint TEXT NOT NULL,install_id INTEGER NOT NULL,PRIMARY KEY(user_id,request_key))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_target_catalog (backend_install_id INTEGER NOT NULL,backend_id TEXT NOT NULL,resource_type TEXT NOT NULL,resource_id TEXT NOT NULL,record_json TEXT NOT NULL,PRIMARY KEY(backend_install_id,backend_id,resource_type,resource_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_observations (deployment_id TEXT PRIMARY KEY,checked_at TEXT NOT NULL DEFAULT '',state TEXT NOT NULL DEFAULT 'unknown',message TEXT NOT NULL DEFAULT '',admission_state TEXT NOT NULL DEFAULT 'unknown')`,
	} {
		if _, e := s.db.Exec(q); e != nil {
			return e
		}
	}
	return nil
}
func validateTargetDiscovery(d *runtimeTargetDiscovery) error {
	if d == nil {
		return nil
	}
	u, e := url.Parse(d.Path)
	if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(d.Path, "/") || strings.HasPrefix(d.Path, "//") || strings.ContainsAny(d.Path, "?#\\%") || strings.Contains(d.Path, "..") || d.ResourceType == "" || d.IDField == "" || d.NameField == "" {
		return fmt.Errorf("target discovery requires a private relative GET path and field mapping")
	}
	return nil
}
func (m *appRuntimeManager) backendName(ref runtimeBackendRef) string {
	manifest, e := installManifest(m.s, ref.InstallID)
	if e == nil && manifest != nil {
		if manifest.DisplayName != "" {
			return manifest.DisplayName
		}
		return manifest.Name
	}
	return fmt.Sprintf("Backend #%d", ref.InstallID)
}
func (m *appRuntimeManager) listTargets(ctx context.Context, ref runtimeBackendRef, project, search, cursor string) (runtimeTargetPage, error) {
	page := runtimeTargetPage{Targets: []runtimeTarget{}}
	reg, e := m.registration(ref)
	if e != nil {
		return page, e
	}
	if !reg.Caps.TargetListing && reg.TargetDiscovery == nil {
		return page, fmt.Errorf("backend does not advertise target listing; enter a target ID or configure target discovery")
	}
	var owner, base, status string
	if e = m.s.store.db.QueryRow(`SELECT COALESCE(project_id,''),sidecar_url_override,status FROM app_installs WHERE id=?`, ref.InstallID).Scan(&owner, &base, &status); e != nil {
		return page, e
	}
	if owner != "" && owner != project {
		return page, fmt.Errorf("backend belongs to another project")
	}
	if status != "running" || runtimeSecureURL(base) != nil {
		return page, fmt.Errorf("backend is not reachable")
	}
	token, e := m.s.runtimeControlToken(ref.InstallID)
	if e != nil {
		return page, e
	}
	path := reg.Endpoint + "/targets"
	if reg.TargetDiscovery != nil {
		path = reg.TargetDiscovery.Path
	}
	u, _ := url.Parse(runtimeEndpoint(base, path))
	query := u.Query()
	query.Set("project_id", project)
	if reg.TargetDiscovery == nil {
		query.Set("q", search)
		query.Set("cursor", cursor)
	}
	u.RawQuery = query.Encode()
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return page, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, e := m.http.Do(req)
	if e != nil {
		return page, fmt.Errorf("target catalog unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return page, fmt.Errorf("backend target catalog returned HTTP %d", response.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, runtimeResponseLimit+1))
	if e != nil || len(raw) > runtimeResponseLimit {
		return page, fmt.Errorf("target catalog exceeds limit")
	}
	if reg.TargetDiscovery == nil {
		if e = json.Unmarshal(raw, &page); e != nil {
			return page, fmt.Errorf("invalid target catalog")
		}
	} else {
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&decoded) != nil {
			return page, fmt.Errorf("invalid target catalog")
		}
		d := reg.TargetDiscovery
		for _, key := range strings.Split(d.Items, ".") {
			if key == "" {
				continue
			}
			v, ok := decoded.(map[string]any)
			if !ok {
				return page, fmt.Errorf("target catalog mapping does not match response")
			}
			decoded = v[key]
		}
		entries, ok := decoded.([]any)
		if !ok {
			return page, fmt.Errorf("target catalog must contain a list")
		}
		if len(entries) > 5000 {
			return page, fmt.Errorf("target catalog exceeds item limit")
		}
		for _, entry := range entries {
			v, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			id := fmt.Sprint(v[d.IDField])
			name, _ := v[d.NameField].(string)
			st, _ := v[d.StatusField].(string)
			ready := d.ReadyValue == "" || st == d.ReadyValue
			t := runtimeTarget{Ref: runtimeResourceRef{ref.InstallID, d.ResourceType, id}, Name: name, Status: st, Available: ready}
			if !ready {
				t.Reason = "Target is not ready"
			}
			t.Platform, _ = v["platform"].(string)
			t.Region, _ = v["region"].(string)
			page.Targets = append(page.Targets, t)
		}
	}
	if len(page.Targets) > 5000 || len(page.NextCursor) > 512 {
		return page, fmt.Errorf("target catalog exceeds item limit")
	}
	filtered := []runtimeTarget{}
	for _, t := range page.Targets {
		if t.Ref.InstallID != ref.InstallID || t.Ref.Type == "" || t.Ref.ID == "" || t.Ref.ID == "<nil>" || len(t.Ref.ID) > 256 || len(t.Name) > 256 || len(t.Reason) > 1024 {
			return page, fmt.Errorf("invalid target ownership or metadata")
		}
		if t.Name == "" {
			t.Name = t.Ref.ID
		}
		_, e = m.s.store.db.Exec(`INSERT INTO app_runtime_target_catalog VALUES(?,?,?,?,?) ON CONFLICT(backend_install_id,backend_id,resource_type,resource_id) DO UPDATE SET record_json=excluded.record_json`, ref.InstallID, ref.ID, t.Ref.Type, t.Ref.ID, runtimeJSON(t))
		if e != nil {
			return page, e
		}
		if search == "" || strings.Contains(strings.ToLower(t.Name+" "+t.Region+" "+t.Platform), strings.ToLower(search)) {
			filtered = append(filtered, t)
		}
	}
	if reg.TargetDiscovery != nil {
		offset, cursorErr := strconv.Atoi(cursor)
		if cursor != "" && cursorErr != nil {
			return page, fmt.Errorf("invalid target cursor")
		}
		if offset < 0 || offset > len(filtered) {
			return page, fmt.Errorf("invalid target cursor")
		}
		end := min(offset+100, len(filtered))
		page.NextCursor = ""
		if end < len(filtered) {
			page.NextCursor = strconv.Itoa(end)
		}
		filtered = filtered[offset:end]
	}
	page.Targets = filtered
	return page, nil
}
func (s *Server) registerRuntimeConsoleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/apps/deployment-targets", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		id, _ := strconv.ParseInt(r.URL.Query().Get("backend_install_id"), 10, 64)
		ref := runtimeBackendRef{id, r.URL.Query().Get("backend_id")}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		page, e := s.appRuntimeManager().listTargets(ctx, ref, r.URL.Query().Get("project_id"), r.URL.Query().Get("q"), r.URL.Query().Get("cursor"))
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		writeJSON(w, page)
	}))
	mux.HandleFunc("/apps/deployment-preflight", s.authMiddleware(s.handleRuntimeConsolePreflight))
}
func (s *Server) handleRuntimeConsolePreflight(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct {
		InstallID    int64            `json:"install_id"`
		ManifestURL  string           `json:"manifest_url"`
		ManifestYAML string           `json:"manifest_yaml"`
		ProjectID    string           `json:"project_id"`
		Placement    runtimePlacement `json:"placement"`
	}
	if runtimeDecode(r, &body) != nil {
		http.Error(w, "invalid preflight request", 400)
		return
	}
	var manifest *sdk.Manifest
	var e error
	if body.InstallID > 0 {
		if _, ok := s.requireAppInstallAccess(w, r, body.InstallID, ProjectViewer); !ok {
			return
		}
		manifest, e = installManifest(s, body.InstallID)
		if e == nil {
			e = s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, body.InstallID).Scan(&body.ProjectID)
		}
	} else {
		var raw []byte
		raw, e = s.fetchManifestBytes(body.ManifestURL, body.ManifestYAML)
		if e == nil {
			manifest, e = sdk.ParseManifest(raw)
		}
	}
	if e != nil || manifest == nil {
		http.Error(w, "valid app manifest required", 400)
		return
	}
	p, checks := s.appRuntimeManager().preflight(r.Context(), body.InstallID, body.ProjectID, *manifest, body.Placement)
	writeJSON(w, map[string]any{"compatible": firstRuntimeBlock(checks) == nil, "placement": p, "checks": checks})
}
func (m *appRuntimeManager) deploymentSummary(id int64) runtimeDeploymentSummary {
	v := runtimeDeploymentSummary{Type: "local", State: "unknown", TargetName: "This Server"}
	var raw, active, pending, status, statusMsg string
	var managed int
	if m.s.store.db.QueryRow(`SELECT runtime_managed,runtime_placement_json,runtime_active_deployment,runtime_pending_deployment,status,COALESCE(status_message,'') FROM app_installs WHERE id=?`, id).Scan(&managed, &raw, &active, &pending, &status, &statusMsg) != nil {
		return v
	}
	var p runtimePlacement
	_ = json.Unmarshal([]byte(raw), &p)
	v.Managed = managed != 0
	d, e := m.loadDeployment(active)
	if e != nil && pending != "" {
		if candidate, err := m.loadDeployment(pending); err == nil {
			p = candidate.Placement
		}
	}
	if e == nil {
		p = d.Placement
		v.State = d.Status
	}
	if p.Type == "backend" {
		v.Type = "backend"
		v.Backend = &p.Backend
		v.BackendName = m.backendName(p.Backend)
		v.Target = p.Target
		v.TargetName = p.Target.ID
		var tr string
		if m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_target_catalog WHERE backend_install_id=? AND backend_id=? AND resource_type=? AND resource_id=?`, p.Backend.InstallID, p.Backend.ID, p.Target.Type, p.Target.ID).Scan(&tr) == nil {
			var t runtimeTarget
			if json.Unmarshal([]byte(tr), &t) == nil {
				v.TargetName = t.Name
			}
		}
	}
	if p.Ingress != nil {
		v.PublicURL = "https://" + p.Ingress.Hostname
	}
	if managed == 0 {
		v.State = status
		return v
	}
	if e == nil {
		var checked, state, message, admission string
		if m.s.store.db.QueryRow(`SELECT checked_at,state,message,admission_state FROM app_runtime_observations WHERE deployment_id=?`, d.ID).Scan(&checked, &state, &message, &admission) == nil {
			v.CheckedAt = checked
			v.State = state
			v.Message = message
			v.AdmissionState = admission
			if t, er := time.Parse(time.RFC3339Nano, checked); er != nil || time.Since(t) > runtimeObservationMaxAge {
				v.State = "unknown"
				v.Message = "Last readiness observation is stale"
			}
		} else {
			v.State = "unknown"
			v.Message = "Waiting for a verified readiness observation"
		}
	}
	if status == "disabled" {
		v.State = "stopped"
	}
	if statusMsg != "" {
		v.Message = statusMsg
	}
	if pending != "" {
		var count int
		m.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE install_id=? AND status IN ('queued','running')`, id).Scan(&count)
		if count > 0 {
			v.State = "deploying"
		}
	}
	return v
}
func (m *appRuntimeManager) consoleOperations(id int64) []runtimeOperation {
	out := []runtimeOperation{}
	rows, e := m.s.store.db.Query(`SELECT record_json FROM app_runtime_operations WHERE install_id=? ORDER BY rowid DESC LIMIT 20`, id)
	if e != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var op runtimeOperation
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &op) == nil {
			out = append(out, op)
		}
	}
	return out
}
