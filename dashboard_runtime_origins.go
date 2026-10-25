package main

import (
	"encoding/json"
	"net/url"
)

// Managed browser endpoints are trusted deployment-controller output, not app
// registrations. Only the committed, running generation contributes to the
// dashboard policy; private control endpoints and pending candidates never do.
func (s *Server) dashboardRuntimeOrigins() ([]string, error) {
	rows, err := s.store.db.Query(`SELECT d.record_json FROM app_installs i
 JOIN app_runtime_deployments owner ON owner.id=i.runtime_active_deployment AND owner.install_id=i.id
 JOIN app_runtime_deployments d ON d.install_id=i.id AND d.generation=owner.generation
 WHERE i.runtime_managed=1 AND i.status='running' AND owner.status='active' AND d.status='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	origins := []string{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d runtimeStoredDeployment
		if err = json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		if d.Placement.local() {
			continue
		}
		endpoints := []string{d.Execution.BrowserEndpoint}
		for _, name := range []string{"browser", "media", "signaling"} {
			endpoints = append(endpoints, d.Execution.Endpoints[name])
		}
		for _, endpoint := range endpoints {
			if endpoint == "" || runtimeSecureURL(endpoint) != nil {
				continue
			}
			u, err := url.Parse(endpoint)
			if err != nil || u.Scheme != "https" {
				continue
			}
			normalized, err := normalizeDashboardConnectOrigins([]string{u.Scheme + "://" + u.Host})
			if err != nil {
				continue
			}
			origins = append(origins, normalized...)
		}
	}
	return origins, rows.Err()
}
