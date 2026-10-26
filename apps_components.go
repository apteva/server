package main

// Components use the shared SDK manifest contract and the existing runtime
// manager. Server owns placement and installation identity.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type appComponentDeclaration = sdk.ComponentDeclaration
type appComponentDeclarations = sdk.ComponentDeclarations

type appComponentDeployment struct {
	OwnerInstallID int64          `json:"owner_install_id"`
	Name           string         `json:"name"`
	Protocol       string         `json:"protocol"`
	Capabilities   runtimeAppCaps `json:"capabilities"`
}

// Keep one strict SDK parser for app and component declarations.
func parseServerAppManifest(raw []byte) (*sdk.Manifest, error) { return sdk.ParseManifest(raw) }

// Versioned declaration snapshots follow the parent's committed version,
// including installations created before SDK manifest serialization supported them.
func (s *Store) migrateAppComponents() error {
	for _, query := range []string{
		`ALTER TABLE app_installs ADD COLUMN owner_install_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE app_installs ADD COLUMN component_name TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(query); err != nil && !isRuntimeDuplicateColumn(err) {
			return err
		}
	}
	for _, query := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS app_component_owner_name ON app_installs(owner_install_id,component_name) WHERE owner_install_id!=0`,
		`CREATE TABLE IF NOT EXISTS app_component_declarations (install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE, parent_version TEXT NOT NULL, name TEXT NOT NULL, declaration_json TEXT NOT NULL, PRIMARY KEY(install_id,parent_version,name))`,
		`CREATE TRIGGER IF NOT EXISTS app_component_valid_owner BEFORE INSERT ON app_installs WHEN NEW.owner_install_id!=0 BEGIN SELECT CASE WHEN NEW.component_name='' OR NOT EXISTS(SELECT 1 FROM app_installs p WHERE p.id=NEW.owner_install_id AND p.owner_install_id=0 AND p.project_id=NEW.project_id AND p.installed_by=NEW.installed_by) THEN RAISE(ABORT,'invalid component owner or scope') END; END`,
		`CREATE TRIGGER IF NOT EXISTS app_component_retain_owner BEFORE DELETE ON app_installs WHEN EXISTS(SELECT 1 FROM app_installs c WHERE c.owner_install_id=OLD.id) BEGIN SELECT RAISE(ABORT,'remove owned components before their owner'); END`,
		`CREATE TRIGGER IF NOT EXISTS app_component_scope_guard BEFORE UPDATE OF project_id,installed_by,owner_install_id,component_name ON app_installs WHEN NEW.project_id!=OLD.project_id OR NEW.installed_by!=OLD.installed_by OR NEW.owner_install_id!=OLD.owner_install_id OR NEW.component_name!=OLD.component_name BEGIN SELECT CASE WHEN NEW.owner_install_id!=OLD.owner_install_id OR NEW.component_name!=OLD.component_name OR OLD.owner_install_id!=0 OR EXISTS(SELECT 1 FROM app_installs c WHERE c.owner_install_id=OLD.id) THEN RAISE(ABORT,'owned component scope and ownership are immutable') END; END`,
	} {
		if _, err := s.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

func saveAppComponentDeclarations(tx *sql.Tx, id int64, version string, components appComponentDeclarations) error {
	for name, def := range components {
		raw := runtimeJSON(def)
		var previous string
		err := tx.QueryRow(`SELECT declaration_json FROM app_component_declarations WHERE install_id=? AND parent_version=? AND name=?`, id, version, name).Scan(&previous)
		if err == nil {
			if previous != raw {
				return fmt.Errorf("component %s release declaration changed without a parent version change", name)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO app_component_declarations VALUES(?,?,?,?)`, id, version, name, raw); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) persistAppComponentDeclarations(id int64, version string, components appComponentDeclarations) error {
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveAppComponentDeclarations(tx, id, version, components); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) componentDeclarations(id int64) (appComponentDeclarations, error) {
	rows, err := s.store.db.Query(`SELECT d.name,d.declaration_json FROM app_component_declarations d JOIN app_installs i ON i.id=d.install_id WHERE d.install_id=? AND d.parent_version=i.version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := appComponentDeclarations{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var def appComponentDeclaration
		if err := json.Unmarshal([]byte(raw), &def); err != nil {
			return nil, err
		}
		out[name] = def
	}
	return out, rows.Err()
}
func (s *Server) componentOwner(id int64) (int64, string, error) {
	var parent int64
	var name string
	err := s.store.db.QueryRow(`SELECT owner_install_id,component_name FROM app_installs WHERE id=?`, id).Scan(&parent, &name)
	return parent, name, err
}
func (s *Server) isOwnedComponent(id int64) bool {
	owner, _, err := s.componentOwner(id)
	return err == nil && owner != 0
}
func (s *Server) hasOwnedComponents(id int64) bool {
	var n int
	err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE owner_install_id=?`, id).Scan(&n)
	return err != nil || n != 0
}
func (s *Server) ownedComponentsBusy(id int64) bool {
	var n int
	err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs c WHERE c.owner_install_id=? AND (c.status!='disabled' OR EXISTS(SELECT 1 FROM app_runtime_operations o WHERE o.install_id=c.id AND o.status IN ('queued','running')))`, id).Scan(&n)
	return err != nil || n != 0
}

func (s *Server) componentDeployment(id int64, manifest *sdk.Manifest) (*appComponentDeployment, error) {
	parent, name, err := s.componentOwner(id)
	if err != nil {
		return nil, err
	}
	if parent == 0 {
		return nil, nil
	}
	defs, err := s.componentDeclarations(parent)
	if err != nil {
		return nil, err
	}
	def, ok := defs[name]
	if !ok || runtimeJSON(def.Manifest) != runtimeJSON(manifest) {
		// Rollback and restart may reuse an exact previously approved release.
		previous, err := s.appRuntimeManager().deployments(id)
		if err != nil {
			return nil, err
		}
		for _, d := range previous {
			if d.Component != nil && d.Component.OwnerInstallID == parent && d.Component.Name == name && runtimeJSON(d.Manifest) == runtimeJSON(manifest) {
				copy := *d.Component
				return &copy, nil
			}
		}
		return nil, errors.New("component release must match its owner's declaration or a retained approved deployment")
	}
	return &appComponentDeployment{parent, name, def.Protocol, def.Capabilities}, nil
}

type appComponentSummary struct {
	Name        string                    `json:"name"`
	Protocol    string                    `json:"protocol"`
	Required    bool                      `json:"required"`
	InstallID   int64                     `json:"install_id,omitempty"`
	Status      string                    `json:"status"`
	Version     string                    `json:"version"`
	Permissions []sdk.Permission          `json:"permissions"`
	Deployment  *runtimeDeploymentSummary `json:"deployment,omitempty"`
}

func (s *Server) componentSummaries(parent int64) ([]appComponentSummary, error) {
	defs, err := s.componentDeclarations(parent)
	if err != nil {
		return nil, err
	}
	out := make([]appComponentSummary, 0, len(defs))
	// Include removed declarations with retained children so cleanup never vanishes.
	rows, err := s.store.db.Query(`SELECT component_name,id,status,version FROM app_installs WHERE owner_install_id=?`, parent)
	if err != nil {
		return nil, err
	}
	children := map[string]appComponentSummary{}
	for rows.Next() {
		var c appComponentSummary
		if err := rows.Scan(&c.Name, &c.InstallID, &c.Status, &c.Version); err != nil {
			rows.Close()
			return nil, err
		}
		children[c.Name] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for name, def := range defs {
		c := children[name]
		c.Name = name
		c.Protocol = def.Protocol
		c.Required = def.Required
		c.Permissions = def.Manifest.Requires.Permissions
		if c.InstallID == 0 {
			c.Status = "not_configured"
			c.Version = def.Manifest.Version
		}
		children[name] = c
	}
	for _, c := range children {
		if _, ok := defs[c.Name]; !ok {
			c.Status = "declaration_removed"
		}
		if c.InstallID != 0 {
			summary := s.appRuntimeManager().deploymentSummary(c.InstallID)
			c.Deployment = &summary
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Same installation identity on retries; the runtime manager supplies durable
// operation idempotency, package preparation and confirmed lifecycle ownership.
func (s *Server) ensureComponent(parent int64, name string, config map[string]string, approved []sdk.Permission) (int64, *sdk.Manifest, error) {
	defs, err := s.componentDeclarations(parent)
	if err != nil {
		return 0, nil, err
	}
	def, ok := defs[name]
	if !ok {
		return 0, nil, errors.New("component is not declared by this installed release")
	}
	if runtimeFingerprint(approvedPermissions(approved)) != runtimeFingerprint(approvedPermissions(def.Manifest.Requires.Permissions)) {
		return 0, nil, errors.New("explicit approval of the component permissions is required")
	}
	var project string
	var user, owner int64
	if err := s.store.db.QueryRow(`SELECT project_id,installed_by,owner_install_id FROM app_installs WHERE id=?`, parent).Scan(&project, &user, &owner); err != nil {
		return 0, nil, err
	}
	if owner != 0 {
		return 0, nil, errors.New("nested owned components are not supported")
	}
	if !manifestAllowsScope(&def.Manifest, map[bool]sdk.Scope{true: sdk.ScopeGlobal, false: sdk.ScopeProject}[project == ""]) {
		return 0, nil, errors.New("component does not support its parent's scope")
	}
	if len(def.Manifest.Requires.Apps) != 0 || len(def.Manifest.Requires.Integrations) != 0 {
		return 0, nil, errors.New("owned components currently receive scoped job inputs; implicit dependency installation or credential inheritance is not supported")
	}
	rawConfig := ""
	if len(config) > 0 {
		rawConfig, err = Encrypt(s.secret, runtimeJSON(config))
		if err != nil {
			return 0, nil, err
		}
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	var parentStatus string
	var stopping int
	if err := tx.QueryRow(`SELECT status,EXISTS(SELECT 1 FROM app_runtime_operations WHERE install_id=? AND kind IN ('stop','delete') AND status IN ('queued','running')) FROM app_installs WHERE id=?`, parent, parent).Scan(&parentStatus, &stopping); err != nil {
		return 0, nil, err
	}
	if parentStatus == "disabled" || parentStatus == "error" || stopping != 0 {
		return 0, nil, errors.New("component owner is stopped or stopping")
	}
	var child int64
	err = tx.QueryRow(`SELECT id FROM app_installs WHERE owner_install_id=? AND component_name=?`, parent, name).Scan(&child)
	if err == nil {
		if len(config) > 0 {
			var stored string
			if err := tx.QueryRow(`SELECT config_encrypted FROM app_installs WHERE id=?`, child).Scan(&stored); err != nil {
				return 0, nil, err
			}
			plain, err := Decrypt(s.secret, stored)
			if err != nil {
				return 0, nil, errors.New("existing component configuration unavailable")
			}
			var current map[string]string
			if json.Unmarshal([]byte(plain), &current) != nil || runtimeJSON(current) != runtimeJSON(config) {
				return 0, nil, errors.New("component configuration differs; use its existing configuration endpoint")
			}
		}
		if _, err := tx.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, runtimeJSON(approved), child); err != nil {
			return 0, nil, err
		}
		if err := tx.Commit(); err != nil {
			return 0, nil, err
		}
		return child, &def.Manifest, nil
	}
	if err != sql.ErrNoRows {
		return 0, nil, err
	}
	privateName := fmt.Sprintf("component-%d-%s", parent, name)
	_, err = tx.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES(?,'component',?) ON CONFLICT(name) DO NOTHING`, privateName, runtimeJSON(def.Manifest))
	if err != nil {
		return 0, nil, err
	}
	var appID int64
	if err = tx.QueryRow(`SELECT id FROM apps WHERE name=?`, privateName).Scan(&appID); err != nil {
		return 0, nil, err
	}
	res, err := tx.Exec(`INSERT INTO app_installs(app_id,project_id,installed_by,status,version,manifest_json,permissions_json,config_encrypted,source,owner_install_id,component_name) VALUES(?,?,?,'pending',?,?,?,?,'component',?,?)`, appID, project, user, def.Manifest.Version, runtimeJSON(def.Manifest), runtimeJSON(approved), rawConfig, parent, name)
	if err != nil {
		return 0, nil, err
	}
	child, err = res.LastInsertId()
	if err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return child, &def.Manifest, nil
}
func approvedPermissions(in []sdk.Permission) []string {
	out := make([]string, 0, len(in))
	seen := map[sdk.Permission]bool{}
	for _, p := range in {
		if !seen[p] {
			out = append(out, string(p))
			seen[p] = true
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleAppComponents(w http.ResponseWriter, r *http.Request, parent int64) {
	base := fmt.Sprintf("/apps/installs/%d/components", parent)
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, base), "/")
	if r.Method == http.MethodGet && rest == "" {
		out, err := s.componentSummaries(parent)
		if err != nil {
			http.Error(w, "components unavailable", 503)
			return
		}
		writeJSON(w, out)
		return
	}
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	parts := strings.Split(rest, "/")
	if sdk.ValidateComponentName(parts[0]) != nil {
		http.Error(w, "invalid component name", 400)
		return
	}
	if len(parts) > 1 || r.Method == http.MethodDelete {
		var child int64
		if s.store.db.QueryRow(`SELECT id FROM app_installs WHERE owner_install_id=? AND component_name=?`, parent, parts[0]).Scan(&child) != nil {
			http.Error(w, "component is not configured", 404)
			return
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		if r.Method == http.MethodDelete && len(parts) == 1 {
			copy.URL.Path = fmt.Sprintf("/apps/installs/%d", child)
			s.handleUninstallApp(w, copy)
			return
		}
		if len(parts) > 1 && (parts[1] == "deployment" || parts[1] == "placement") {
			copy.URL.Path = fmt.Sprintf("/apps/installs/%d/%s", child, strings.Join(parts[1:], "/"))
			s.handleAppDeployment(w, copy, child)
			return
		}
		http.Error(w, "unsupported component operation", 404)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !runtimeID(r.Header.Get("Idempotency-Key")) {
		http.Error(w, "Idempotency-Key is required", 400)
		return
	}
	var body struct {
		Placement runtimePlacement  `json:"placement"`
		Config    map[string]string `json:"config"`
		Approved  []sdk.Permission  `json:"approved_permissions"`
	}
	if err := runtimeDecode(r, &body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.Placement.Ingress != nil || body.Placement.PoolID != "" {
		http.Error(w, "components use private single-execution placement", 400)
		return
	}
	if err := s.appRuntimeManager().normalizePlacement(&body.Placement); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := body.Placement.validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	runtimeConsoleInstallMu.Lock()
	defer runtimeConsoleInstallMu.Unlock()
	child, manifest, err := s.ensureComponent(parent, parts[0], body.Config, body.Approved)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	op, err := s.appRuntimeManager().begin(child, "deploy", r.Header.Get("Idempotency-Key"), body.Placement, manifest)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSONStatus(w, 202, map[string]any{"install_id": child, "owner_install_id": parent, "component_name": parts[0], "operation": op})
}

func (s *Server) resolveComponentCall(parent int64, name string) (int64, error) {
	if sdk.ValidateComponentName(name) != nil {
		return 0, errors.New("invalid component name")
	}
	defs, err := s.componentDeclarations(parent)
	if err != nil {
		return 0, err
	}
	def, ok := defs[name]
	if !ok {
		return 0, errors.New("component not declared by this installed release")
	}
	var child int64
	var status, project, parentProject string
	var owner, parentOwner int64
	err = s.store.db.QueryRow(`SELECT c.id,c.status,c.project_id,p.project_id,c.installed_by,p.installed_by FROM app_installs c JOIN app_installs p ON p.id=c.owner_install_id WHERE c.owner_install_id=? AND c.component_name=?`, parent, name).Scan(&child, &status, &project, &parentProject, &owner, &parentOwner)
	if err != nil || status != "running" {
		return 0, errors.New("component not ready")
	}
	if project != parentProject || owner != parentOwner {
		return 0, errors.New("component ownership or scope mismatch")
	}
	d, err := s.appRuntimeManager().active(child)
	if err != nil || d.Status != "active" || d.Component == nil || d.Component.OwnerInstallID != parent || d.Component.Name != name || d.Component.Protocol != def.Protocol {
		return 0, errors.New("component has no compatible active deployment")
	}
	return child, nil
}

func (m *appRuntimeManager) verifyComponentWorker(d runtimeDeployment, endpoint, token string) error {
	if d.Component == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(endpoint, sdk.ComponentDescribePath), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return errors.New("component protocol verification unavailable")
	}
	defer resp.Body.Close()
	var actual sdk.ComponentDescriptor
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, runtimeResponseLimit)).Decode(&actual) != nil || actual.Protocol != d.Component.Protocol {
		return errors.New("running component protocol differs from its declared release")
	}
	return m.verifyAppLifecycleCapabilities(endpoint, token, d.Component.Capabilities)
}
