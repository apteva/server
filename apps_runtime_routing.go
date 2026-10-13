package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

type runtimeReplicaEndpoint struct {
	URL   string
	Token string
	ID    string
}

func (s *Server) loadRuntimeEndpoints(entry *InstalledApp) {
	if !s.runtimeManaged(entry.InstallID) {
		return
	}
	m := s.appRuntimeManager()
	d, err := m.active(entry.InstallID)
	if err != nil {
		return
	}
	token, err := m.token(d)
	if err != nil {
		return
	}
	entry.Token = token
	entry.SidecarURL = d.Execution.Endpoint
	ds, err := m.deployments(entry.InstallID)
	if err != nil {
		return
	}
	for _, rep := range ds {
		if rep.Generation != d.Generation || rep.Status != "active" {
			continue
		}
		t, e := m.token(rep)
		if e == nil {
			entry.RuntimeReplicas = append(entry.RuntimeReplicas, runtimeReplicaEndpoint{rep.Execution.Endpoint, t, rep.ID})
		}
	}
	entry.RuntimeRouting = d.AppCaps.Routing
}
func (s *Server) runtimeProxyEntry(entry *InstalledApp, r *http.Request) *InstalledApp {
	if len(entry.RuntimeReplicas) < 2 {
		return entry
	}
	index := atomic.AddUint64(&s.runtimeRoundRobin, 1) - 1
	if entry.RuntimeRouting == "key" {
		key := r.Header.Get("X-Apteva-Routing-Key")
		if key == "" {
			key = r.URL.Query().Get("runtime_routing_key")
		}
		if key == "" {
			return entry
		}
		digest := sha256.Sum256([]byte(key))
		index = binary.BigEndian.Uint64(digest[:8])
	}
	selected := entry.RuntimeReplicas[index%uint64(len(entry.RuntimeReplicas))]
	copy := *entry
	copy.SidecarURL, copy.Token = selected.URL, selected.Token
	return &copy
}
func (s *Server) runtimeInitRouteAllowed(token string, r *http.Request) bool {
	var raw string
	err := s.store.db.QueryRow(`SELECT d.record_json FROM app_runtime_credentials c JOIN app_runtime_deployments d ON d.id=c.deployment_id WHERE c.token_hash=?`, HashAPIKey(token)).Scan(&raw)
	if err != nil {
		return true
	}
	var d runtimeStoredDeployment
	if json.Unmarshal([]byte(raw), &d) != nil {
		return false
	}
	if d.Status == "retiring" {
		return true
	}
	active, err := s.appRuntimeManager().active(d.InstallID)
	if err == nil && d.Generation == active.Generation && d.Status == "active" {
		return true
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/apps/callback/") {
		return true
	}
	return false
}
