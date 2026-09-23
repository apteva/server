package main

import (
	"encoding/json"
	"net/http"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func (s *Server) handleRuntimeClock(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) != 0 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, runtime.clock.State())
	case http.MethodPost:
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
			return
		}
		var req struct {
			To time.Time `json:"to"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&req); err != nil {
			http.Error(w, "invalid clock advancement", http.StatusBadRequest)
			return
		}
		state, err := runtime.clock.Advance(req.To)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, state)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// A cloned install can read only the clock of its own runtime. The request
// contains no caller-selected runtime id or timestamp.
func (s *Server) handleCallbackEnvironmentTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var projectID string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&projectID); err != nil {
		http.NotFound(w, r)
		return
	}
	if s.environments == nil {
		http.NotFound(w, r)
		return
	}
	clock, ok := s.environments.Clock(projectID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if runtime, ready := s.environments.Get(projectID); ready {
		member := false
		for _, name := range runtime.InstallNames() {
			if install, ok := runtime.Install(name); ok && install.InstallID == installID {
				member = true
				break
			}
		}
		if !member {
			http.NotFound(w, r)
			return
		}
	}
	writeJSON(w, map[string]time.Time{"current_time": clock.Now()})
}
