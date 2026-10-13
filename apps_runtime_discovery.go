package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Existing SDKs can opt into discovery through ordinary declared HTTP routes;
// a future provides.runtime_backends declaration can use the same protocol.
func (m *appRuntimeManager) discoverBackends(ctx context.Context) ([]runtimeBackendRegistration, error) {
	return m.discoverBackendsFor(ctx, 0)
}
func (m *appRuntimeManager) discoverBackendsFor(ctx context.Context, installID int64) ([]runtimeBackendRegistration, error) {
	rows, err := m.s.store.db.Query(`SELECT i.id,COALESCE(NULLIF(i.manifest_json,''),a.manifest_json) FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.status='running' AND (?=0 OR i.id=?) ORDER BY i.id`, installID, installID)
	if err != nil {
		return nil, err
	}
	var candidates []runtimeBackendRegistration
	for rows.Next() {
		var id int64
		var raw string
		if rows.Scan(&id, &raw) != nil {
			continue
		}
		var manifest struct {
			Provides struct {
				HTTPRoutes []struct {
					Prefix string `json:"prefix"`
				} `json:"http_routes"`
				Backends []struct {
					ID       string `json:"id"`
					Endpoint string `json:"endpoint"`
					Contract string `json:"contract"`
				} `json:"runtime_backends"`
			} `json:"provides"`
		}
		if json.Unmarshal([]byte(raw), &manifest) != nil {
			continue
		}
		seen := map[string]bool{}
		for _, route := range manifest.Provides.HTTPRoutes {
			prefix := strings.TrimRight(route.Prefix, "/")
			idPart := strings.TrimPrefix(prefix, runtimeControlPrefix+"backends/")
			if prefix == idPart || !runtimeID(idPart) || seen[idPart] {
				continue
			}
			seen[idPart] = true
			candidates = append(candidates, runtimeBackendRegistration{InstallID: id, ID: idPart, Endpoint: prefix})
		}
		for _, decl := range manifest.Provides.Backends {
			if decl.Contract != runtimeBackendContract || seen[decl.ID] {
				continue
			}
			seen[decl.ID] = true
			candidates = append(candidates, runtimeBackendRegistration{InstallID: id, ID: decl.ID, Endpoint: decl.Endpoint})
		}
	}
	rows.Close()
	if len(candidates) > 64 {
		return nil, fmt.Errorf("backend discovery exceeds batch limit; register providers individually")
	}
	var discovered []runtimeBackendRegistration
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return discovered, ctx.Err()
		}
		if err := m.registerBackendContext(ctx, candidate); err != nil {
			continue
		}
		registered, err := m.registration(runtimeBackendRef{candidate.InstallID, candidate.ID})
		if err == nil {
			discovered = append(discovered, registered)
		}
	}
	return discovered, nil
}
