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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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
	InstallID   int64
	AppName     string
	ProjectID   string
	Manifest    sdk.Manifest
	SidecarURL  string            // http://<worker-ip>:<port> from orchestrator (sidecar apps only)
	StaticDir   string            // absolute path on disk (kind=static apps only)
	MountPath   string            // URL prefix this app is served at (kind=static apps only, e.g. "/client")
	Config      map[string]string // decrypted config_json — used for branding / kiosk-key injection
	Permissions []sdk.Permission
	Token       string // platform-issued APTEVA_APP_TOKEN for callbacks
}

func NewInstalledAppsRegistry() *InstalledAppsRegistry {
	return &InstalledAppsRegistry{entries: map[int64]*InstalledApp{}, byName: map[string]*InstalledApp{}}
}

func (r *InstalledAppsRegistry) Get(installID int64) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[installID]
}

// GetByName returns *some* install with the given app name. Returns
// the LAST-registered one when multiple exist (one per project).
//
// IMPORTANT: for apps that can be installed in more than one project
// (storage, media, anything declaring scopes:[project,global] in its
// manifest), prefer GetByNameAndProject — this method's last-wins
// resolution silently misroutes requests across project boundaries.
//
// Safe for callers that target single-instance apps (routes, certs,
// host-routing for static apps, the dashboard itself).
func (r *InstalledAppsRegistry) GetByName(name string) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}

// GetByNameAndProject returns the install of the given app name that
// is reachable from a request scoped to projectID. Resolution order:
//
//  1. Exact match on (name, projectID).
//  2. Global install for the name (project_id == "").
//  3. nil.
//
// When projectID is "" only the global install is considered. This
// matches the schema's UNIQUE(app_id, project_id) — exactly one
// install per (app, project) combination, plus optionally one global.
//
// Used by handleAppProxy to dispatch /api/apps/<name>/...?project_id=...
// requests to the correct install, instead of last-wins which leaks
// requests across projects (the storage→media bug).
func (r *InstalledAppsRegistry) GetByNameAndProject(name, projectID string) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var globalMatch *InstalledApp
	for _, e := range r.entries {
		if e.AppName != name {
			continue
		}
		if e.ProjectID == projectID {
			return e
		}
		if e.ProjectID == "" {
			globalMatch = e
		}
	}
	return globalMatch
}

func (r *InstalledAppsRegistry) GetByNameAndProjectExact(name, projectID string) *InstalledApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		if e.AppName == name && e.ProjectID == projectID {
			return e
		}
	}
	return nil
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
		delete(r.entries, installID)
		// Keep the legacy name index coherent when another project/global
		// install with the same app name remains mounted.
		for _, candidate := range r.entries {
			if candidate.AppName == e.AppName {
				r.byName[e.AppName] = candidate
				if candidate.ProjectID == "" {
					break
				}
			}
		}
		return
	}
}

// LoadInstalledApps reads every running app_install from the DB and
// populates the in-memory registry. Called at server boot. Failures
// for one install are logged and skipped — they don't block boot.
func (s *Server) LoadInstalledApps() {
	rows, err := s.store.db.Query(
		`SELECT i.id, i.app_id, COALESCE(i.project_id, ''), i.service_name,
			COALESCE(i.sidecar_url_override, ''),
			COALESCE(i.config_encrypted, ''),
			i.permissions_json, i.version, a.name,
			COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
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
			id, appID                                                              int64
			projectID, serviceName, sidecarOverride, configEnc, permsJSON, version string
			appName, manifestJSON                                                  string
		)
		if err := rows.Scan(&id, &appID, &projectID, &serviceName, &sidecarOverride,
			&configEnc, &permsJSON, &version, &appName, &manifestJSON); err != nil {
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

		// Decrypt the install's config so static apps can inject branding
		// + the kiosk api key into their served index.html. Sidecar apps
		// don't read this here (the env-var pass-through happens at
		// spawn time), but loading it once is cheap and keeps the
		// runtime single-source-of-truth.
		var cfg map[string]string
		if configEnc != "" {
			if plain, derr := Decrypt(s.secret, configEnc); derr == nil {
				_ = json.Unmarshal([]byte(plain), &cfg)
			}
		}

		// Static-app detection. The install-time path stores
		// "static://<absolute-disk-path>" in the same column normally
		// used for a sidecar URL. Recognising this short-circuits the
		// orchestrator lookup and tells the mount loop to wire a
		// path-mounted file server instead of a reverse proxy.
		// Token must match what apps_source.installFromSource and
		// apps_local.installLocally set as APTEVA_APP_TOKEN at spawn.
		// Without this, handleAppProxy sends no
		// Authorization header to the sidecar and every dashboard
		// iframe request comes back 401 from withTokenAuth.
		token, tokenErr := s.appInstallToken(id)
		if tokenErr != nil {
			log.Printf("[APPS] install=%d credential unavailable: %v", id, tokenErr)
			continue
		}
		entry := &InstalledApp{
			InstallID:   id,
			AppName:     appName,
			ProjectID:   projectID,
			Manifest:    manifest,
			Config:      cfg,
			Permissions: perms,
			Token:       token,
		}
		if strings.HasPrefix(sidecarOverride, "static://") {
			entry.StaticDir = strings.TrimPrefix(sidecarOverride, "static://")
			entry.MountPath = resolveMountPath(&manifest, cfg)
			s.installedApps.Add(entry)
			count++
			log.Printf("[APPS] mounted %s (install=%d project=%q static_dir=%s mount=%s)",
				appName, id, projectID, entry.StaticDir, entry.MountPath)
			continue
		}

		// URL precedence: explicit override (local dev) > orchestrator
		// service lookup. Override is the cheap escape hatch — paste a
		// literal http://host:port at install time and you don't need
		// the orchestrator at all.
		sidecarURL := sidecarOverride
		if sidecarURL == "" {
			sidecarURL = s.resolveSidecarURL(serviceName, manifest.Runtime.Port)
		}
		entry.SidecarURL = sidecarURL
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
func (s *Server) resolveSidecarURL(serviceName string, primaryPort int) string {
	if serviceName == "" || s.orchestratorURL == "" {
		return ""
	}
	resp, err := http.Get(s.orchestratorURL + "/api/v1/services/" + url.PathEscape(serviceName))
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
				AgentID string             `json:"instance_id"`
				Ports   []orchestratorPort `json:"ports"`
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
	ip := s.workerIP(c.AgentID)
	if ip == "" {
		return ""
	}
	hostPort := selectPrimaryHostPort(c.Ports, primaryPort)
	if hostPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, hostPort)
}

type orchestratorPort struct {
	HostPort      int `json:"host_port"`
	ContainerPort int `json:"container_port"`
}

// selectPrimaryHostPort keeps the HTTP proxy pinned to runtime.port even when
// a sidecar also exposes raw TCP/UDP listeners. The legacy fallback preserves
// compatibility with orchestrators that did not return container_port.
func selectPrimaryHostPort(ports []orchestratorPort, primaryPort int) int {
	for _, p := range ports {
		if p.ContainerPort == primaryPort && p.HostPort > 0 {
			return p.HostPort
		}
	}
	if len(ports) == 1 && ports[0].ContainerPort == 0 {
		return ports[0].HostPort
	}
	return 0
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
	// External streaming providers do not consistently preserve query strings
	// on WebSocket URLs (Twilio explicitly forbids them on <Stream url>). Allow
	// an install to be selected in the path instead:
	//
	//   /api/apps/<name>/_install/<id>/<app route>
	//
	// The selector is removed before route authorization and proxying, so the
	// sidecar still sees its declared path such as /media/twilio/....
	var pathInstallID int64
	if strippedPath, installID, hasSelector, err := splitAppInstallSelector(tail); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if hasSelector {
		tail = strippedPath
		pathInstallID = installID
	}
	// Project-aware dispatch. /api/apps/<name>/...?project_id=<X>
	// must route to the install of <name> in project X (or the
	// global install if no project-X install exists). Without this,
	// the last-wins byName map silently routes every storage call
	// to one install, leaking writes across project boundaries when
	// multiple project-scoped installs of the same app coexist.
	//
	// Reject an EMPTY project_id query param explicitly. Caller
	// included the key but didn't fill it — typically a dashboard
	// panel rendering before its project context was hydrated. In
	// that state we'd silently fall through to byName.GetByName,
	// which returns whichever install registered last and serves
	// THAT project's data — the cross-project flashing operators
	// see on first paint. 400 with a clear error message surfaces
	// the bug to the panel author instead of papering over it.
	//
	// An absent project_id may only resolve a global install. Picking an
	// arbitrary project install here would cross tenant boundaries.
	rawQuery := r.URL.Query()
	_, hasProjectIDKey := rawQuery["project_id"]
	requestedProjectID := strings.TrimSpace(rawQuery.Get("project_id"))

	// X-Apteva-Project-ID is a server-owned identity header. The auth
	// middleware removes client-supplied identity headers before setting
	// these principal fields for delegated user keys. Capture that trusted
	// project context, then remove the header so it can only be re-added by
	// the proxy after routing and authorization have completed.
	trustedProjectID := ""
	if strings.TrimSpace(r.Header.Get("X-Apteva-Subject-Type")) != "" {
		trustedProjectID = strings.TrimSpace(r.Header.Get("X-Apteva-Project-ID"))
	}
	r.Header.Del("X-Apteva-Project-ID")
	if trustedProjectID != "" {
		if requestedProjectID != "" && requestedProjectID != trustedProjectID {
			http.Error(w, "project_id does not match delegated key project", http.StatusForbidden)
			return
		}
		if requestedProjectID == "" && !hasProjectIDKey {
			requestedProjectID = trustedProjectID
			rawQuery.Set("project_id", requestedProjectID)
			r.URL.RawQuery = rawQuery.Encode()
		}
	}
	if hasProjectIDKey && requestedProjectID == "" {
		http.Error(w, "project_id query param present but empty — caller must supply the project context", http.StatusBadRequest)
		return
	}
	var entry *InstalledApp
	if pathInstallID > 0 {
		if installIDRaw := rawQuery.Get("install_id"); installIDRaw != "" && installIDRaw != strconv.FormatInt(pathInstallID, 10) {
			http.Error(w, "install_id query does not match app path", http.StatusBadRequest)
			return
		}
		entry = s.installedApps.Get(pathInstallID)
		if entry != nil && entry.AppName != appName {
			http.Error(w, "install path does not match app: "+appName, http.StatusBadRequest)
			return
		}
	} else if installIDRaw := rawQuery.Get("install_id"); installIDRaw != "" {
		installID, err := strconv.ParseInt(installIDRaw, 10, 64)
		if err != nil || installID <= 0 {
			http.Error(w, "invalid install_id query param", http.StatusBadRequest)
			return
		}
		entry = s.installedApps.Get(installID)
		if entry != nil && entry.AppName != appName {
			http.Error(w, "install_id does not match app: "+appName, http.StatusBadRequest)
			return
		}
	} else if installID := installIDFromDevAPIKey(rawQuery.Get("api_key")); installID > 0 {
		entry = s.installedApps.Get(installID)
		if entry != nil && entry.AppName != appName {
			http.Error(w, "api_key install does not match app: "+appName, http.StatusBadRequest)
			return
		}
	} else if requestedProjectID != "" {
		entry = s.installedApps.GetByNameAndProject(appName, requestedProjectID)
	} else {
		entry = s.installedApps.GetByNameAndProject(appName, "")
	}
	if entry == nil {
		http.Error(w, "app not installed: "+appName, http.StatusNotFound)
		return
	}
	effectiveProjectID := entry.ProjectID
	if effectiveProjectID == "" {
		// Global installs execute inside the validated request project. This
		// lets project members use a shared sidecar without giving them
		// platform-admin access or exposing another project's data.
		effectiveProjectID = requestedProjectID
	} else if requestedProjectID != "" && requestedProjectID != effectiveProjectID {
		http.Error(w, "project_id does not match selected app install", http.StatusBadRequest)
		return
	}
	authenticatedAppID, _ := strconv.ParseInt(r.Header.Get("X-Apteva-App-Install-ID"), 10, 64)
	if authenticatedAppID > 0 {
		if authenticatedAppID != entry.InstallID {
			http.Error(w, "app credential does not match target install", http.StatusForbidden)
			return
		}
	} else if !appProxyRouteIsNoAuth(entry, tail, r.Method) {
		need := ProjectViewer
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			need = ProjectEditor
		}
		if !s.requireScopedProjectAccess(w, r, effectiveProjectID, need) {
			return
		}
	}
	if entry.SidecarURL == "" {
		http.Error(w, "app sidecar not reachable: "+appName, http.StatusServiceUnavailable)
		return
	}
	if effectiveProjectID != "" && tail == "/mcp" && r.Method == http.MethodPost {
		if err := injectProjectIntoMCPRequest(r, effectiveProjectID); err != nil {
			http.Error(w, "invalid MCP request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if tail == "/mcp" && r.Method == http.MethodPost {
		if err := extractCallerThreadFromMCPRequest(r); err != nil {
			http.Error(w, "invalid MCP caller context: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.applyChannelChatSubjectContext(r)
	}
	var asyncReq *appMCPAsyncRequest
	if tail == "/mcp" && r.Method == http.MethodPost {
		asyncReq = s.inspectAppMCPAsyncRequest(entry, r)
	}
	target, err := url.Parse(entry.SidecarURL)
	if err != nil {
		http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	publicRoute := appProxyRouteIsNoAuth(entry, tail, r.Method)
	// Rewrite path so the sidecar sees its own routes (without the
	// /apps/<name> prefix). The token swap happens in Director.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = tail
		if publicRoute && !requestIsProtocolUpgrade(r) {
			s.applyGeoCountryHeader(req.Header, r)
		} else {
			req.Header.Del(geoCountryHeader)
		}
		// Ordinary app routes are agent/user-facing. Only the authenticated
		// /apps/callback/apps/:name/call bridge may mint this identity.
		req.Header.Del(sdk.HeaderBoundCallerInstallID)
		req.Header.Del("X-Apteva-Project-ID")
		if effectiveProjectID != "" {
			req.Header.Set("X-Apteva-Project-ID", effectiveProjectID)
		}
		originalAuth := req.Header.Get("Authorization")
		if originalAuth != "" {
			req.Header.Set("X-Apteva-Original-Authorization", originalAuth)
		}
		if entry.Token != "" {
			if originalAuth != "" && appProxyRouteIsNoAuth(entry, tail, req.Method) {
				req.Header.Set("X-Apteva-App-Token", entry.Token)
			} else {
				req.Header.Set("Authorization", "Bearer "+entry.Token)
			}
		}
		req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprintf("%d", entry.InstallID))
	}
	if asyncReq != nil {
		proxy.ModifyResponse = func(resp *http.Response) error {
			return s.maybeAugmentAppMCPAsyncResponse(entry, asyncReq, resp)
		}
	}
	proxy.ServeHTTP(w, r)
}

func appProxyRouteIsNoAuth(entry *InstalledApp, path, method string) bool {
	if entry == nil {
		return false
	}
	for _, route := range entry.Manifest.Provides.HTTPRoutes {
		if !route.NoAuth {
			continue
		}
		if route.Method != "" && !strings.EqualFold(route.Method, method) {
			continue
		}
		if appRouteMatches(route.Prefix, path) {
			return true
		}
	}
	return false
}

func splitAppInstallSelector(appPath string) (strippedPath string, installID int64, hasSelector bool, err error) {
	if !strings.HasPrefix(appPath, "/_install/") {
		return appPath, 0, false, nil
	}
	selected := strings.TrimPrefix(appPath, "/_install/")
	parts := strings.SplitN(selected, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, true, fmt.Errorf("install path must include an install id and app route")
	}
	installID, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || installID <= 0 {
		return "", 0, true, fmt.Errorf("invalid install id in app path")
	}
	return "/" + parts[1], installID, true, nil
}

func installIDFromDevAPIKey(apiKey string) int64 {
	raw, ok := strings.CutPrefix(apiKey, "dev-")
	if !ok || raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func injectProjectIntoMCPRequest(r *http.Request, projectID string) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	nextBody := body
	defer func() {
		r.Body = io.NopCloser(bytes.NewReader(nextBody))
		r.ContentLength = int64(len(nextBody))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(nextBody)), nil
		}
	}()
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	rpc, _ := decoded.(map[string]any)
	if rpc == nil {
		return nil
	}
	method, _ := rpc["method"].(string)
	if method != "tools/call" {
		return nil
	}
	params, _ := rpc["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
		rpc["params"] = params
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
		params["arguments"] = args
	}
	injectProjectArgAny(args, projectID)
	rewritten, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	nextBody = rewritten
	return nil
}

// extractCallerThreadFromMCPRequest converts Core's hidden, post-telemetry
// caller value into a server-owned header for the app SDK and removes it from
// tool arguments. The value is accepted only alongside the authenticated
// caller-agent header; dashboard/API callers cannot forge thread identity.
func extractCallerThreadFromMCPRequest(r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	nextBody := body
	defer func() {
		r.Body = io.NopCloser(bytes.NewReader(nextBody))
		r.ContentLength = int64(len(nextBody))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(nextBody)), nil
		}
	}()
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var rpc map[string]any
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil
	}
	if method, _ := rpc["method"].(string); method != "tools/call" {
		return nil
	}
	params, _ := rpc["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		return nil
	}
	threadID, _ := args["_apteva_caller_thread"].(string)
	delete(args, "_apteva_caller_thread")
	if strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Agent")) != "" && strings.TrimSpace(threadID) != "" {
		r.Header.Set("X-Apteva-Caller-Thread", strings.TrimSpace(threadID))
	} else {
		r.Header.Del("X-Apteva-Caller-Thread")
	}
	rewritten, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	nextBody = rewritten
	return nil
}
