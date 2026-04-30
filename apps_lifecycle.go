package main

// Lifecycle hooks for the integration-binding system. Two responsibilities:
//
//   1. recomputePendingOptions — set/clear the has_pending_options flag
//      on every running install whose manifest has unbound optional
//      integration roles that NOW have a compatible target available.
//      Called whenever a connection is created/deleted or an app is
//      installed/uninstalled.
//
//   2. dependentsOfConnection — used by the connection-delete handler
//      to refuse deletion when one or more app_installs have the
//      connection bound. Operator gets a list of dependents instead
//      of a silent break + degraded apps.
//
// The recompute path is best-effort: failures for one install don't
// block the others. The delete protection is hard — we'd rather
// surface a 409 than have apps quietly stop working.

import (
	"encoding/json"
	"fmt"
	"log"

	sdk "github.com/apteva/app-sdk"
)

// recomputePendingOptions walks every running install and flips
// has_pending_options based on whether any optional, currently-unbound
// integration role has a compatible target available. Idempotent;
// safe to call as often as we like.
func (s *Server) recomputePendingOptions() {
	rows, err := s.store.db.Query(
		`SELECT i.id, COALESCE(i.project_id, ''), a.manifest_json,
		        COALESCE(i.integration_bindings, '{}')
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status = 'running'`,
	)
	if err != nil {
		log.Printf("[APPS-LIFECYCLE] recompute query failed: %v", err)
		return
	}
	defer rows.Close()
	type pending struct{ id int64; flag int }
	var updates []pending
	for rows.Next() {
		var (
			id                              int64
			projectID, manifestJSON, bindJSON string
		)
		if err := rows.Scan(&id, &projectID, &manifestJSON, &bindJSON); err != nil {
			continue
		}
		var manifest sdk.Manifest
		if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
			continue
		}
		var bindings map[string]any
		_ = json.Unmarshal([]byte(bindJSON), &bindings)
		if bindings == nil {
			bindings = map[string]any{}
		}
		flag := 0
		for _, dep := range manifest.Requires.Integrations {
			if dep.Required {
				continue // required deps are checked at install/upgrade time
			}
			raw, present := bindings[dep.Role]
			isNull := !present || raw == nil
			if !isNull {
				continue // already bound
			}
			// Optional + unbound: does a compatible target now exist?
			if hasCompatibleTarget(s, projectID, &dep) {
				flag = 1
				break
			}
		}
		updates = append(updates, pending{id: id, flag: flag})
	}
	for _, u := range updates {
		s.store.db.Exec(
			`UPDATE app_installs SET has_pending_options = ? WHERE id = ?`,
			u.flag, u.id,
		)
	}
}

// hasCompatibleTarget returns true when a currently-installed
// integration connection or running app exists that could fill the
// given role for the install's project.
func hasCompatibleTarget(s *Server, projectID string, dep *sdk.IntegrationDep) bool {
	kind := dep.Kind
	if kind == "" {
		kind = "integration"
	}
	if kind == "integration" {
		if len(dep.CompatibleSlugs) == 0 {
			return false
		}
		// Connections are user-scoped but for "is something available
		// in this project" we look across all users — the recompute
		// is just a hint to the operator, not an authorization check.
		// The user_id filter happens later when the operator picks
		// in the rebind UI.
		rows, err := s.store.db.Query(
			`SELECT app_slug FROM connections WHERE project_id = ? AND status = 'active'`,
			projectID,
		)
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var slug string
			if rows.Scan(&slug) == nil && contains(dep.CompatibleSlugs, slug) {
				return true
			}
		}
		return false
	}
	// kind = app
	if len(dep.CompatibleAppNames) == 0 {
		return false
	}
	rows, err := s.store.db.Query(
		`SELECT a.name FROM app_installs i JOIN apps a ON a.id=i.app_id
		 WHERE i.status='running' AND (i.project_id = ? OR i.project_id = '')`,
		projectID,
	)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && contains(dep.CompatibleAppNames, n) {
			return true
		}
	}
	return false
}

// dependentsOfConnection returns the app installs whose
// integration_bindings reference the given connection_id. Used by the
// connection-delete handler to refuse the delete when N installs
// would silently lose a bound dep.
type ConnectionDependent struct {
	InstallID int64  `json:"install_id"`
	AppName   string `json:"app_name"`
	Role      string `json:"role"`
}

func (s *Server) dependentsOfConnection(connID int64) ([]ConnectionDependent, error) {
	rows, err := s.store.db.Query(
		`SELECT i.id, a.name, COALESCE(i.integration_bindings, '{}')
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status = 'running'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectionDependent
	for rows.Next() {
		var (
			id     int64
			name   string
			bindJSON string
		)
		if err := rows.Scan(&id, &name, &bindJSON); err != nil {
			continue
		}
		var bindings map[string]any
		if json.Unmarshal([]byte(bindJSON), &bindings) != nil {
			continue
		}
		for role, raw := range bindings {
			if n, ok := raw.(float64); ok && int64(n) == connID {
				out = append(out, ConnectionDependent{
					InstallID: id, AppName: name, Role: role,
				})
			}
		}
	}
	return out, nil
}

// dependentsOfApp returns the app installs whose integration_bindings
// reference the given install_id (i.e. depend on it via kind=app).
func (s *Server) dependentsOfApp(targetInstallID int64) ([]ConnectionDependent, error) {
	rows, err := s.store.db.Query(
		`SELECT i.id, a.name, COALESCE(i.integration_bindings, '{}')
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status = 'running' AND i.id != ?`,
		targetInstallID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectionDependent
	for rows.Next() {
		var (
			id     int64
			name   string
			bindJSON string
		)
		if err := rows.Scan(&id, &name, &bindJSON); err != nil {
			continue
		}
		var bindings map[string]any
		if json.Unmarshal([]byte(bindJSON), &bindings) != nil {
			continue
		}
		for role, raw := range bindings {
			if n, ok := raw.(float64); ok && int64(n) == targetInstallID {
				out = append(out, ConnectionDependent{
					InstallID: id, AppName: name, Role: role,
				})
			}
		}
	}
	return out, nil
}

// formatDependents renders a "X apps depend on this — y, z" message
// for the cascade-protect 409 response.
func formatDependents(deps []ConnectionDependent) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, d := range deps {
		parts = append(parts, fmt.Sprintf("%s (role=%s, install=%d)", d.AppName, d.Role, d.InstallID))
	}
	return fmt.Sprintf("%d app install(s) depend on this: %s", len(deps), joinComma(parts))
}

func joinComma(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	out := xs[0]
	for _, x := range xs[1:] {
		out += ", " + x
	}
	return out
}
