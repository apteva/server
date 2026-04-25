package main

// Apteva Apps loader — the platform side of the Apps system declared
// in github.com/apteva/app-sdk. At server boot we:
//
//  1. Read every row in app_installs whose status='running'.
//  2. For each install, look up the orchestrator service URL.
//  3. Register a reverse proxy at /apps/<name>/* pointing at the sidecar.
//  4. Register an mcp_servers row of source='app' so the install's MCP
//     tools are available to instances on the same project.
//  5. Cache enabled prompt fragments per project so instance start can
//     concatenate them onto the directive.
//
// This file holds the boot-time wiring + the small RPC surface apps use
// to call back into the platform (see apps_handlers.go for the HTTP
// handlers; see callbacks_apps.go for the per-permission router).

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	sdk "github.com/apteva/app-sdk"
)

// InstalledAppsRegistry is the in-memory index built at boot from app_installs +
// orchestrator service URLs. Read by every place that needs to know
// "is app X mounted?" or "what's its sidecar URL?".
type InstalledAppsRegistry struct {
	mu      sync.RWMutex
	entries map[int64]*InstalledApp // keyed by install id
	byName  map[string]*InstalledApp
}

type InstalledApp struct {
	InstallID    int64
	AppName      string
	ProjectID    string
	Manifest     sdk.Manifest
	SidecarURL   string // http://<worker-ip>:<port> from orchestrator
	Permissions  []sdk.Permission
	Token        string // platform-issued APTEVA_APP_TOKEN for callbacks
}

func NewInstalledAppsRegistry() *InstalledAppsRegistry {
	return &InstalledAppsRegistry{entries: map[int64]*InstalledApp{}, byName: map[string]*InstalledApp{}}
}

func (r *InstalledAppsRegistry) Get(installID int64) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[installID]
}

func (r *InstalledAppsRegistry) GetByName(name string) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}

func (r *InstalledAppsRegistry) List() []*InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*InstalledApp, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// ListForProject returns installs visible to a given project — its
// own installs plus globals.
func (r *InstalledAppsRegistry) ListForProject(projectID string) []*InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*InstalledApp{}
	for _, e := range r.entries {
		if e.ProjectID == "" || e.ProjectID == projectID {
			out = append(out, e)
		}
	}
	return out
}

func (r *InstalledAppsRegistry) Add(e *InstalledApp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[e.InstallID] = e
	r.byName[e.AppName] = e
}

func (r *InstalledAppsRegistry) Remove(installID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[installID]; ok {
		delete(r.byName, e.AppName)
	}
	delete(r.entries, installID)
}

// LoadInstalledApps reads every running app_install from the DB and
// populates the in-memory registry. Called at server boot. Failures
// for one install are logged and skipped — they don't block boot.
func (s *Server) LoadInstalledApps() {
	rows, err := s.store.db.Query(
		`SELECT i.id, i.app_id, COALESCE(i.project_id, ''), i.service_name,
			COALESCE(i.sidecar_url_override, ''),
			i.permissions_json, i.version, a.name, a.manifest_json
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status = 'running'`)
	if err != nil {
		log.Printf("[APPS] load installs: %v", err)
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var (
			id, appID                                                            int64
			projectID, serviceName, sidecarOverride, permsJSON, version          string
			appName, manifestJSON                                                string
		)
		if err := rows.Scan(&id, &appID, &projectID, &serviceName, &sidecarOverride,
			&permsJSON, &version, &appName, &manifestJSON); err != nil {
			log.Printf("[APPS] scan: %v", err)
			continue
		}
		var manifest sdk.Manifest
		if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
			log.Printf("[APPS] %s: bad manifest: %v", appName, err)
			continue
		}
		var perms []sdk.Permission
		_ = json.Unmarshal([]byte(permsJSON), &perms)

		// URL precedence: explicit override (local dev) > orchestrator
		// service lookup. Override is the cheap escape hatch — paste a
		// literal http://host:port at install time and you don't need
		// the orchestrator at all.
		sidecarURL := sidecarOverride
		if sidecarURL == "" {
			sidecarURL = s.resolveSidecarURL(serviceName)
		}

		entry := &InstalledApp{
			InstallID:   id,
			AppName:     appName,
			ProjectID:   projectID,
			Manifest:    manifest,
			SidecarURL:  sidecarURL,
			Permissions: perms,
			Token:       "", // minted on demand
		}
		s.installedApps.Add(entry)
		count++
		log.Printf("[APPS] mounted %s (install=%d project=%q sidecar=%s)",
			appName, id, projectID, entry.SidecarURL)
	}
	log.Printf("[APPS] loaded %d installed apps", count)
}

// resolveSidecarURL asks the orchestrator where the named service is
// running and returns http://<ip>:<host_port>. Empty string if the
// orchestrator can't tell us — callers fall back gracefully.
func (s *Server) resolveSidecarURL(serviceName string) string {
	if serviceName == "" || s.orchestratorURL == "" {
		return ""
	}
	resp, err := http.Get(s.orchestratorURL + "/api/v1/services/" + serviceName)
	if err != nil {
		log.Printf("[APPS] orchestrator unreachable for %s: %v", serviceName, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var body struct {
		Data struct {
			Containers []struct {
				InstanceID string `json:"instance_id"`
				Ports      []struct {
					HostPort      int `json:"host_port"`
					ContainerPort int `json:"container_port"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	if len(body.Data.Containers) == 0 || len(body.Data.Containers[0].Ports) == 0 {
		return ""
	}
	c := body.Data.Containers[0]
	ip := s.workerIP(c.InstanceID)
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, c.Ports[0].HostPort)
}

// workerIP returns the public IP of the named worker instance from the
// orchestrator. Cached briefly. Empty string on failure.
func (s *Server) workerIP(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	resp, err := http.Get(s.orchestratorURL + "/api/v1/instances/" + instanceID)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var body struct {
		Data struct {
			PublicIP string `json:"public_ip"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Data.PublicIP
}

// AppProxy — single handler that reverse-proxies /apps/<name>/* to the
// sidecar URL the registry has on record. Auth is the same session
// the rest of the dashboard uses; the token sent to the sidecar is
// the install's APTEVA_APP_TOKEN, swapped in on the way through.
func (s *Server) handleAppProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/")
	if rest == "" {
		http.Error(w, "app name required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	appName := parts[0]
	tail := ""
	if len(parts) == 2 {
		tail = "/" + parts[1]
	}
	entry := s.installedApps.GetByName(appName)
	if entry == nil {
		http.Error(w, "app not installed: "+appName, http.StatusNotFound)
		return
	}
	if entry.SidecarURL == "" {
		http.Error(w, "app sidecar not reachable: "+appName, http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(entry.SidecarURL)
	if err != nil {
		http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Rewrite path so the sidecar sees its own routes (without the
	// /apps/<name> prefix). The token swap happens in Director.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = tail
		if entry.Token != "" {
			req.Header.Set("Authorization", "Bearer "+entry.Token)
		}
		req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprintf("%d", entry.InstallID))
	}
	proxy.ServeHTTP(w, r)
}
