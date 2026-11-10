package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var fleetUpdateID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (s *Server) fleetUpdatePolicyEnabled() bool {
	_, err := os.Lstat(filepath.Join(s.dataDir, "fleet-update-grant"))
	// An unreadable policy must not re-enable the ordinary API route.
	return !errors.Is(err, os.ErrNotExist)
}

func (s *Server) authenticateFleetUpdate(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer fup_")
	if !ok || len(token) != 64 {
		return false
	}
	grant, err := os.ReadFile(filepath.Join(s.dataDir, "fleet-update-grant"))
	if err != nil || len(grant) != 64 {
		return false
	}
	expected, err := hex.DecodeString(string(grant))
	actual := sha256.Sum256([]byte("fup_" + token))
	return err == nil && subtle.ConstantTimeCompare(expected, actual[:]) == 1
}

func (s *Server) fleetUpdateJob(id string) (*platformUpdateJob, error) {
	current := filepath.Join(s.dataDir, "platform-update.json")
	for _, path := range []string{current, filepath.Join(s.dataDir, "fleet-update-jobs", id+".json")} {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var job platformUpdateJob
		if err := json.Unmarshal(raw, &job); err != nil {
			return nil, err
		}
		if job.Request != nil && job.Request.ID == id {
			job.Backup = ""
			return &job, nil
		}
	}
	return nil, nil
}

type fleetPlatformUpdateView struct {
	Supported        bool               `json:"supported"`
	Reason           string             `json:"reason,omitempty"`
	AvailableVersion string             `json:"available_version,omitempty"`
	Job              *platformUpdateJob `json:"job,omitempty"`
	Code             string             `json:"code,omitempty"`
}

func (s *Server) handleFleetPlatformUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authenticateFleetUpdate(r) {
		http.Error(w, "Fleet update access is disabled or the credential is invalid", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id := r.URL.Query().Get("request_id"); id != "" {
			if !fleetUpdateID.MatchString(id) {
				http.Error(w, "invalid update request ID", http.StatusBadRequest)
				return
			}
			job, err := s.fleetUpdateJob(id)
			if err != nil {
				http.Error(w, "cannot read update job", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, fleetPlatformUpdateView{Job: job})
			return
		}
		writeJSON(w, s.fleetUpdateAvailability(r))
	case http.MethodPost:
		s.startFleetPlatformUpdate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) fleetUpdateAvailability(r *http.Request) fleetPlatformUpdateView {
	if !fleetUpdateServiceSupported() {
		return fleetPlatformUpdateView{Reason: "Fleet updates require a supported Linux systemd installation."}
	}
	view, err := s.platformUpdater(r.Context(), false, "", "preserve", "")
	if err != nil {
		return fleetPlatformUpdateView{Reason: "Apteva could not check the local updater. Inspect the service on the host."}
	}
	if view.FleetProtocol != 1 && view.Supported {
		return fleetPlatformUpdateView{Reason: "Update the Apteva CLI and server together before enabling Fleet updates."}
	}
	result := fleetPlatformUpdateView{Supported: view.Supported, Reason: view.Reason}
	if view.Job != nil {
		switch view.Job.State {
		case "succeeded", "failed", "rolled_back", "recovery_required", "reconciled":
		default:
			result.Supported = false
			result.Reason = "Another update is in progress."
		}
	}
	if status := s.platformStatus.View(); status.UpdateAvailable {
		result.AvailableVersion = status.BundleVersion
	}
	return result
}

func fleetUpdateServiceSupported() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	method := detectInstallMethod()
	return method == "systemd-system" || method == "systemd-user"
}

func (s *Server) startFleetPlatformUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID   string `json:"request_id"`
		Version     string `json:"version"`
		AgentPolicy string `json:"agent_policy"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || !fleetUpdateID.MatchString(req.RequestID) || req.Version == "" {
		http.Error(w, "invalid Fleet update request", http.StatusBadRequest)
		return
	}
	if req.AgentPolicy != "preserve" && req.AgentPolicy != "rolling" && req.AgentPolicy != "restart" {
		http.Error(w, "invalid agent policy", http.StatusBadRequest)
		return
	}
	job, err := s.fleetUpdateJob(req.RequestID)
	if err != nil {
		http.Error(w, "cannot read update job", http.StatusServiceUnavailable)
		return
	}
	if job != nil {
		if job.State != "canceled" && (job.Target != req.Version || job.Request.AgentPolicy != req.AgentPolicy) {
			http.Error(w, "request ID is already bound to another update", http.StatusConflict)
			return
		}
		writeFleetPlatformUpdate(w, http.StatusAccepted, fleetPlatformUpdateView{Job: job})
		return
	}
	if !fleetUpdateServiceSupported() {
		writeFleetPlatformUpdate(w, http.StatusConflict, fleetPlatformUpdateView{
			Code: "not_submitted", Reason: "Fleet updates require a supported Linux systemd installation.",
		})
		return
	}
	status := s.platformStatus.View()
	if !status.UpdateAvailable || status.BundleVersion != req.Version {
		writeFleetPlatformUpdate(w, http.StatusConflict, fleetPlatformUpdateView{
			Code: "not_submitted", Reason: "The available release changed. Review the current offer.",
		})
		return
	}
	if !s.authenticateFleetUpdate(r) {
		http.Error(w, "Fleet update access was revoked", http.StatusUnauthorized)
		return
	}
	view, err := s.platformUpdater(r.Context(), true, req.Version, req.AgentPolicy, req.RequestID)
	if err != nil || !view.Supported || view.Job == nil || view.Job.Request == nil || view.Job.Request.ID != req.RequestID {
		http.Error(w, "update submission could not be verified; look up the request before retrying", http.StatusServiceUnavailable)
		return
	}
	view.Job.Backup = ""
	writeFleetPlatformUpdate(w, http.StatusAccepted, fleetPlatformUpdateView{Job: view.Job})
}

func writeFleetPlatformUpdate(w http.ResponseWriter, status int, view fleetPlatformUpdateView) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(view)
}
