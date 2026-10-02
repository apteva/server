package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// This JSON contract is also consumed by apteva/update_job.go. Installation
// identity comes from the server, never from browser-supplied command arguments.
type platformUpdateJob struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Message   string    `json:"message"`
	Target    string    `json:"target_version"`
	Previous  string    `json:"previous_version"`
	UpdatedAt time.Time `json:"updated_at"`
	Backup    string    `json:"backup,omitempty"`
}
type platformUpdateView struct {
	Supported bool               `json:"supported"`
	Reason    string             `json:"reason,omitempty"`
	Job       *platformUpdateJob `json:"job,omitempty"`
}

func (s *Server) platformUpdater(ctx context.Context, start bool, target, policy string) (platformUpdateView, error) {
	view := platformUpdateView{}
	// Preserve the job across restarts, including when the candidate cannot
	// run its CLI. No sensitive updater input is returned to the browser.
	if raw, err := os.ReadFile(filepath.Join(s.dataDir, "platform-update.json")); err == nil {
		var job platformUpdateJob
		if json.Unmarshal(raw, &job) == nil {
			view.Job = &job
		}
	}
	method := detectInstallMethod()
	switch method {
	case "systemd-user", "systemd-system", "launchd-user", "launchd-system":
	default:
		view.Reason = "One-click updates require an Apteva service installation. Use the update instructions below for this " + method + " installation."
		return view, nil
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "dashboard", "index.html")); err == nil {
		view.Reason = "This installation serves a custom dashboard directory. Update it together with the server from the terminal."
		return view, nil
	}
	if os.Getenv("APTEVA_INTEGRATIONS_UI_DIR") != "" {
		view.Reason = "This installation uses an external integrations UI. Update its assets together with the server from the terminal."
		return view, nil
	}
	self, err := os.Executable()
	if err != nil {
		return view, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return view, err
	}
	cli := filepath.Join(filepath.Dir(self), "apteva")
	if _, err := os.Stat(cli); err != nil {
		view.Reason = "The matching Apteva CLI is missing. Update this installation from the terminal first."
		return view, nil
	}
	home, err := filepath.Abs(s.dataDir)
	if err != nil {
		return view, err
	}
	if core := os.Getenv("CORE_CMD"); core != "" && filepath.Clean(core) != filepath.Join(home, "bin", "apteva-core") {
		view.Reason = "This installation uses a custom Core executable. Update it together with the server from the terminal."
		return view, nil
	}
	dbPath, err := filepath.Abs(s.dbPath)
	if err != nil {
		return view, err
	}
	port, err := strconv.Atoi(s.port)
	if err != nil {
		return view, err
	}
	body, _ := json.Marshal(map[string]any{"home": home, "db_path": dbPath, "port": port, "server_pid": os.Getpid(), "method": method, "target_version": target, "agent_policy": policy})
	mode := "--dashboard-probe"
	if start {
		mode = "--dashboard-start"
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "update", mode)
	cmd.Env = append(os.Environ(), "APTEVA_HOME="+home)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return view, fmt.Errorf("updater: %w: %s", err, stderr.String())
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return view, fmt.Errorf("invalid updater response: %w", err)
	}
	return view, nil
}

func (s *Server) handlePlatformUpdate(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requirePlatformAdmin(w, r)
	if !ok {
		return
	}
	// App tokens and internal agent gateways can carry the installer's user
	// identity. Installing host executables requires an actual operator session
	// or private administrator API key, never an app/agent capability.
	if r.Header.Get("X-Apteva-Operator-ID") != strconv.FormatInt(uid, 10) {
		http.Error(w, "an administrator session or private API key is required", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := s.platformUpdater(r.Context(), false, "", "restart")
		if err != nil {
			view.Supported = false
			view.Reason = err.Error()
		}
		writeJSON(w, view)
	case http.MethodPost:
		var body struct {
			Version     string `json:"version"`
			AgentPolicy string `json:"agent_policy"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid update request", http.StatusBadRequest)
			return
		}
		if body.AgentPolicy == "" {
			body.AgentPolicy = "restart"
		}
		if body.AgentPolicy != "restart" && body.AgentPolicy != "rolling" && body.AgentPolicy != "preserve" {
			http.Error(w, "invalid agent policy", http.StatusBadRequest)
			return
		}
		status := s.platformStatus.View()
		if !status.UpdateAvailable || body.Version == "" || body.Version != status.BundleVersion {
			http.Error(w, "release changed or no update is available; check for updates again", http.StatusConflict)
			return
		}
		view, err := s.platformUpdater(r.Context(), true, body.Version, body.AgentPolicy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if !view.Supported {
			http.Error(w, view.Reason, http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, view)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
