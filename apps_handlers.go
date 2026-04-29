package main

// HTTP handlers for /api/apps — list installed apps, install from a
// manifest URL, configure / uninstall / bind to instances. The actual
// sidecar deploy goes through the existing orchestrator (POST
// /api/v1/services).

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"

	"github.com/apteva/server/apps/framework"
)

// AppRow — what /api/apps returns for the dashboard's Installed view.
type AppRow struct {
	InstallID     int64            `json:"install_id"`
	AppID         int64            `json:"app_id"`
	Name          string           `json:"name"`
	DisplayName   string           `json:"display_name"`
	Version          string `json:"version"`
	AvailableVersion string `json:"available_version,omitempty"`
	Description      string `json:"description"`
	Icon          string           `json:"icon"`
	ProjectID     string           `json:"project_id"`
	Status        string           `json:"status"`
	StatusMessage string           `json:"status_message,omitempty"`
	ErrorMessage  string           `json:"error_message,omitempty"`
	Source        string           `json:"source"`
	UpgradePolicy string           `json:"upgrade_policy"`
	Permissions   []sdk.Permission `json:"permissions"`
	Surfaces      AppSurfaces      `json:"surfaces"`
	UIPanels      []sdk.UIPanel    `json:"ui_panels,omitempty"`
}

// AppSurfaces summarises a manifest's `provides` block for the
// dashboard. Counts where the count is meaningful (tools, routes,
// panels), the actual identifying strings where they fit cheaply
// (route prefixes, tool names, channel names), and a kind string
// pulled from runtime.kind so the UI can colour-code "static UI app"
// vs. "service sidecar" vs. "source build". Keep this in sync with
// the dashboard's AppDetailPanel — additions here flow through to
// the side panel automatically.
type AppSurfaces struct {
	Kind            string   `json:"kind"`              // service | source | static
	MCPToolCount    int      `json:"mcp_tool_count"`
	MCPToolNames    []string `json:"mcp_tool_names,omitempty"`
	HTTPRouteCount  int      `json:"http_route_count"`
	HTTPRoutes      []string `json:"http_routes,omitempty"`
	UIPanelCount    int      `json:"ui_panel_count"`
	UIApp           bool     `json:"ui_app"`
	UIAppMount      string   `json:"ui_app_mount,omitempty"`
	ChannelCount    int      `json:"channel_count"`
	ChannelNames    []string `json:"channel_names,omitempty"`
	WorkerCount     int      `json:"worker_count"`
	PromptFragments int      `json:"prompt_fragment_count"`
	Permissions     []string `json:"permissions,omitempty"`
	ConfigKeys      []string `json:"config_keys,omitempty"`
	// RequiredApps lists this app's `requires.apps` entries — other
	// Apteva apps that must be installed alongside this one. The
	// dashboard shows them in the side panel and the install handler
	// cascade-installs them automatically when the operator clicks
	// Install on the dependent app.
	RequiredApps    []AppDependency `json:"required_apps,omitempty"`
}

// AppDependency mirrors sdk.RequiredAppRef + a server-side resolution
// hint: the install handler walks the registry once at request time
// to fill ManifestURL so the cascade install knows where to fetch
// each dep's manifest. The dashboard uses Optional + Reason for the
// "Dependencies" section in the side panel.
type AppDependency struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
	ManifestURL string `json:"manifest_url,omitempty"`
	// Installed: filled in by the marketplace handler once it knows
	// what's currently in app_installs. The dashboard renders this
	// as a per-dep ✓/✗/~ next to the name.
	Installed bool `json:"installed,omitempty"`
}

// RegistryEntry — one row in the marketplace registry.json.
type RegistryEntry struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Author       string   `json:"author"`
	Repo         string   `json:"repo"`
	ManifestURL  string   `json:"manifest_url"`
	Icon         string   `json:"icon"`
	Tags         []string `json:"tags"`
	Official     bool     `json:"official"`
	Category     string   `json:"category"`
}

// Default registry URL used when the operator hasn't overridden it via
// the APTEVA_APP_REGISTRY_URL env var. Self-hosted deployments can
// point at their own curated list.
const defaultRegistryURL = "https://raw.githubusercontent.com/apteva/app-registry/main/registry.json"

// GET /api/apps/marketplace
//
// Fetches the configured registry URL and returns its apps[] alongside
// flags telling the dashboard which ones the user already has installed.
// The registry payload is small (~1 KB per entry) and changes rarely;
// we proxy it server-side so the dashboard sees a single CORS-clean
// origin and the server can short-circuit when offline.
func (s *Server) handleMarketplace(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("registry_url")
	if url == "" {
		if v := getRegistryURLFromEnv(); v != "" {
			url = v
		} else {
			url = defaultRegistryURL
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		http.Error(w, "fetch registry: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, fmt.Sprintf("registry %s: http %d", url, resp.StatusCode), http.StatusBadGateway)
		return
	}
	const maxRegistry = 512 * 1024
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRegistry))
	var reg struct {
		Schema string          `json:"schema"`
		Apps   []RegistryEntry `json:"apps"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		http.Error(w, "parse registry: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Tag entries with installed:true if there's a row in apps for the
	// same name — lets the dashboard render an "Installed" pill.
	// Match keys are normalized (lowercase, hyphens/underscores stripped)
	// so the registry's "channelchat" matches the bundled slug
	// "channel-chat", and built-ins are pre-seeded so they always show
	// as installed even though they have no apps row.
	// "installed" means there's an actual app_installs row — i.e.
	// the operator clicked Install. A row in `apps` alone is just a
	// cached manifest (preview / built-in scan / leftover from an
	// uninstall) and must NOT mark the marketplace entry as installed.
	// Same goes for the framework's loaded built-in apps: those are
	// the in-process apps (channel-chat etc.) — they're "always on"
	// platform components, distinct from the user-installable kind
	// shown in the marketplace.
	installed := map[string]bool{}
	addInstalled := func(name string) {
		if name == "" {
			return
		}
		installed[normalizeAppName(name)] = true
	}
	if rows, err := s.store.db.Query(
		`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id`,
	); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				addInstalled(n)
			}
		}
		rows.Close()
	}
	type entryWithStatus struct {
		RegistryEntry
		Installed bool        `json:"installed"`
		Builtin   bool        `json:"builtin"`
		Surfaces  AppSurfaces `json:"surfaces"`
	}
	// Built-in detection — registry entries whose normalized name
	// matches an in-process framework app (channel-chat etc.) are
	// flagged as built-ins. We also remember the framework app handle
	// so we can derive surfaces directly from it (some built-ins
	// don't have a fetchable manifest_url because they ship inside
	// apteva-server itself).
	builtin := map[string]bool{}
	builtinSurfaces := map[string]AppSurfaces{}
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			surf := surfacesFromFrameworkApp(a)
			for _, k := range []string{m.Slug, m.Name} {
				key := normalizeAppName(k)
				if key == "" {
					continue
				}
				builtin[key] = true
				builtinSurfaces[key] = surf
			}
		}
	}
	// Resolve manifest URLs in parallel (with cache) so the surfaces
	// block on each entry reflects the actual provides/requires/runtime
	// the manifest declares. Built-ins skip the network — their
	// surfaces come from the framework app handle. Failures are
	// non-fatal — the entry just goes out with a zero-value Surfaces
	// struct, and the dashboard degrades gracefully (no badges).
	surfacesByName := map[string]AppSurfaces{}
	versionByName := map[string]string{}
	for k, v := range builtinSurfaces {
		surfacesByName[k] = v
	}
	{
		type result struct {
			name    string
			surf    AppSurfaces
			version string
		}
		ch := make(chan result, len(reg.Apps))
		dispatched := 0
		for _, e := range reg.Apps {
			key := normalizeAppName(e.Name)
			if _, isBuiltin := builtinSurfaces[key]; isBuiltin {
				continue
			}
			if e.ManifestURL == "" {
				continue
			}
			dispatched++
			go func(name, url string) {
				m, _ := s.fetchAndCacheManifest(url)
				if m == nil {
					ch <- result{name: name}
					return
				}
				ch <- result{name: name, surf: surfacesFromManifest(m), version: m.Version}
			}(e.Name, e.ManifestURL)
		}
		for i := 0; i < dispatched; i++ {
			r := <-ch
			key := normalizeAppName(r.name)
			if _, hasBuiltin := surfacesByName[key]; !hasBuiltin {
				surfacesByName[key] = r.surf
			}
			if r.version != "" {
				versionByName[key] = r.version
			}
		}
	}
	// Resolve each dep's ManifestURL from the registry + Installed
	// flag from the live install set, so the dashboard can render a
	// "Tasks ✓ installed / Status ✗ missing" Dependencies section
	// without doing any extra round-trips.
	manifestByAppName := map[string]string{}
	for _, e := range reg.Apps {
		manifestByAppName[normalizeAppName(e.Name)] = e.ManifestURL
	}
	for k, surf := range surfacesByName {
		if len(surf.RequiredApps) == 0 {
			continue
		}
		for i := range surf.RequiredApps {
			depKey := normalizeAppName(surf.RequiredApps[i].Name)
			surf.RequiredApps[i].Installed = installed[depKey] || builtin[depKey]
			if u, ok := manifestByAppName[depKey]; ok {
				surf.RequiredApps[i].ManifestURL = u
			}
		}
		surfacesByName[k] = surf
	}

	out := make([]entryWithStatus, 0, len(reg.Apps))
	for _, e := range reg.Apps {
		key := normalizeAppName(e.Name)
		// Override the registry's hardcoded version with the live
		// manifest's version when we successfully fetched it. The
		// registry tends to drift behind real releases — showing the
		// stale value confuses operators ("I just bumped storage to
		// 0.1.1, why does the marketplace still say 0.1.0?").
		if v, ok := versionByName[key]; ok && v != "" {
			e.Version = v
		}
		out = append(out, entryWithStatus{
			RegistryEntry: e,
			Installed:     installed[key],
			Builtin:       builtin[key],
			Surfaces:      surfacesByName[key],
		})
	}
	writeJSON(w, map[string]any{
		"registry_url": url,
		"apps":         out,
	})
}

func getRegistryURLFromEnv() string {
	return os.Getenv("APTEVA_APP_REGISTRY_URL")
}

// refreshManifestFromSidecar pulls the live manifest from a running
// sidecar's /manifest endpoint and overwrites apps.manifest_json so the
// dashboard's "update available" detector sees the version the sidecar
// itself reports rather than the snapshot taken at install time.
//
// Best-effort: any error (sidecar down, no /manifest, parse failure)
// leaves the DB row untouched so the listing falls back to the stored
// snapshot. Logs are quiet because mass-failures (e.g. orchestrator
// unreachable) would flood with no signal.
func (s *Server) refreshManifestFromSidecar(appName, sidecarURL string) {
	if sidecarURL == "" || appName == "" {
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(sidecarURL + "/manifest")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if !json.Valid(body) {
		return
	}
	s.store.db.Exec(
		`UPDATE apps SET manifest_json = ? WHERE name = ? AND source != 'builtin'`,
		string(body), appName,
	)
}

// GET /api/apps[?project_id=X]
//
// Returns one row per install visible to the caller — project installs
// for the requested project plus all globals. Built-in apps appear with
// source='builtin'.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	q := `
		SELECT i.id, i.app_id, i.project_id, i.status, i.status_message, i.error_message,
			i.upgrade_policy, i.version, i.permissions_json, a.name, a.source, a.manifest_json
		FROM app_installs i JOIN apps a ON a.id = i.app_id`
	args := []any{}
	if projectID != "" {
		q += ` WHERE i.project_id = '' OR i.project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY a.name`
	// Refresh manifest_json from running sidecars before reading.
	// Best-effort and bounded: only non-builtin running installs we
	// already have a SidecarURL for (in-memory registry). Built-ins
	// are kept fresh by RegisterBuiltinApps on every boot.
	for _, e := range s.installedApps.List() {
		s.refreshManifestFromSidecar(e.AppName, e.SidecarURL)
	}
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []AppRow{}
	for rows.Next() {
		var (
			installID, appID                                  int64
			projID, status, statusMsg, errMsg                 string
			upgradePolicy, version, permsJSON                 string
			name, source, manifestJSON                        string
		)
		if err := rows.Scan(&installID, &appID, &projID, &status, &statusMsg, &errMsg,
			&upgradePolicy, &version, &permsJSON, &name, &source, &manifestJSON); err != nil {
			continue
		}
		var manifest sdk.Manifest
		_ = json.Unmarshal([]byte(manifestJSON), &manifest)
		var perms []sdk.Permission
		_ = json.Unmarshal([]byte(permsJSON), &perms)
		out = append(out, AppRow{
			InstallID: installID, AppID: appID, Name: name, DisplayName: manifest.DisplayName,
			Version:          version,
			AvailableVersion: manifest.Version,
			Description:      manifest.Description, Icon: manifest.Icon,
			ProjectID: projID, Status: status, StatusMessage: statusMsg, ErrorMessage: errMsg,
			Source: source, UpgradePolicy: upgradePolicy,
			Permissions: perms, Surfaces: surfacesFromManifest(&manifest),
			UIPanels: manifest.Provides.UIPanels,
		})
	}
	writeJSON(w, out)
}

// surfacesFromFrameworkApp computes a surfaces summary for an app
// that lives in-process via the apps/framework package (rather than
// being declared in an external apteva.yaml). Used so built-in apps
// like channel-chat can show real counts in the marketplace side
// panel even though they have no fetchable manifest URL.
func surfacesFromFrameworkApp(a framework.App) AppSurfaces {
	s := AppSurfaces{
		Kind:           "service",
		MCPToolCount:   len(a.MCPTools()),
		HTTPRouteCount: len(a.HTTPRoutes()),
		ChannelCount:   len(a.Channels()),
		WorkerCount:    len(a.Workers()),
	}
	for _, t := range a.MCPTools() {
		s.MCPToolNames = append(s.MCPToolNames, t.Name)
	}
	for _, rt := range a.HTTPRoutes() {
		s.HTTPRoutes = append(s.HTTPRoutes, rt.Method+" "+rt.Path)
	}
	for _, c := range a.Channels() {
		// ChannelFactory has no plain "name" — use its Go type's
		// short name as a stable, human-readable hint. Empty fallback
		// avoids an empty entry.
		t := fmt.Sprintf("%T", c)
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:]
		}
		if t != "" {
			s.ChannelNames = append(s.ChannelNames, t)
		}
	}
	if len(a.Manifest().UISlots) > 0 {
		s.UIPanelCount = len(a.Manifest().UISlots)
	}
	return s
}

func surfacesFromManifest(m *sdk.Manifest) AppSurfaces {
	s := AppSurfaces{
		Kind:            m.Runtime.Kind,
		MCPToolCount:    len(m.Provides.MCPTools),
		HTTPRouteCount:  len(m.Provides.HTTPRoutes),
		UIPanelCount:    len(m.Provides.UIPanels),
		UIApp:           m.Provides.UIApp != nil,
		ChannelCount:    len(m.Provides.Channels),
		WorkerCount:     len(m.Provides.Workers),
		PromptFragments: len(m.Provides.PromptFragments),
	}
	for _, t := range m.Provides.MCPTools {
		s.MCPToolNames = append(s.MCPToolNames, t.Name)
	}
	for _, rt := range m.Provides.HTTPRoutes {
		s.HTTPRoutes = append(s.HTTPRoutes, rt.Prefix)
	}
	for _, c := range m.Provides.Channels {
		s.ChannelNames = append(s.ChannelNames, c.Name)
	}
	if m.Provides.UIApp != nil {
		s.UIAppMount = m.Provides.UIApp.MountPath
	}
	for _, p := range m.Requires.Permissions {
		s.Permissions = append(s.Permissions, string(p))
	}
	for _, c := range m.ConfigSchema {
		s.ConfigKeys = append(s.ConfigKeys, c.Name)
	}
	for _, dep := range m.Requires.Apps {
		s.RequiredApps = append(s.RequiredApps, AppDependency{
			Name:     dep.Name,
			Version:  dep.Version,
			Reason:   dep.Reason,
			Optional: dep.Optional,
		})
	}
	return s
}

// POST /api/apps/preview
//
// Body: { "manifest_url": "<URL to apteva.yaml>" } OR { "manifest_yaml": "..." }
//
// Returns the parsed manifest + a permission summary so the dashboard
// can render the install consent screen before the user commits.
func (s *Server) handlePreviewApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ManifestURL  string `json:"manifest_url"`
		ManifestYAML string `json:"manifest_yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	yamlBytes, err := s.fetchManifestBytes(body.ManifestURL, body.ManifestYAML)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	manifest, err := sdk.ParseManifest(yamlBytes)
	if err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"manifest": manifest,
		"surfaces": surfacesFromManifest(manifest),
	})
}

// POST /api/apps/install
//
// Body: { manifest_url|manifest_yaml, project_id, config: {...},
//          upgrade_policy: "manual"|"auto-patch"|"auto-minor" }
//
// MVP: creates the apps + app_installs rows in 'pending' state and
// returns. Sidecar deployment via the orchestrator + status flip to
// 'running' is handled by a follow-up reconcile (not in this slice —
// for now the operator runs `./scripts/admin install-app <id>` or sets
// status='running' manually after deploying the image).
func (s *Server) handleInstallApp(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	var body struct {
		ManifestURL   string            `json:"manifest_url"`
		ManifestYAML  string            `json:"manifest_yaml"`
		Repo          string            `json:"repo"`
		Ref           string            `json:"ref"`
		ProjectID     string            `json:"project_id"`
		Config        map[string]string `json:"config"`
		UpgradePolicy string            `json:"upgrade_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	yamlBytes, err := s.fetchManifestBytes(body.ManifestURL, body.ManifestYAML)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	manifest, err := sdk.ParseManifest(yamlBytes)
	if err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Scope check: project install must be allowed; global only if scopes include global.
	scope := sdk.ScopeProject
	if body.ProjectID == "" {
		scope = sdk.ScopeGlobal
	}
	if !manifestAllowsScope(manifest, scope) {
		http.Error(w, fmt.Sprintf("app does not support scope %q", scope), http.StatusBadRequest)
		return
	}

	// Cascade-install dependencies declared in requires.apps. Walks
	// the dep graph in topo order (deps before the dependent),
	// detects cycles, skips already-installed apps. Optional deps
	// install too — operator can uninstall any of them later.
	// Failures of optional deps are logged but don't block the
	// requesting app; failures of required deps abort the install.
	if len(manifest.Requires.Apps) > 0 {
		if err := s.installDependencies(userID, manifest, body.ProjectID); err != nil {
			http.Error(w, "dependency install: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	upgradePolicy := body.UpgradePolicy
	if upgradePolicy == "" {
		upgradePolicy = string(manifest.UpgradePolicy)
	}
	if upgradePolicy == "" {
		upgradePolicy = "manual"
	}

	// Encrypt user config + persist.
	configEncrypted := ""
	if len(body.Config) > 0 {
		raw, _ := json.Marshal(body.Config)
		enc, err := Encrypt(s.secret, string(raw))
		if err != nil {
			http.Error(w, "encrypt config", http.StatusInternalServerError)
			return
		}
		configEncrypted = enc
	}

	manifestJSON, _ := json.Marshal(manifest)
	source := "git"
	if body.Repo == "" && body.ManifestYAML != "" {
		source = "manual"
	}

	// Upsert the app row.
	var appID int64
	err = s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, manifest.Name).Scan(&appID)
	if err != nil {
		res, e := s.store.db.Exec(
			`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, ?, ?, ?, ?)`,
			manifest.Name, source, body.Repo, body.Ref, string(manifestJSON))
		if e != nil {
			http.Error(w, "create app row: "+e.Error(), http.StatusInternalServerError)
			return
		}
		appID, _ = res.LastInsertId()
	} else {
		s.store.db.Exec(
			`UPDATE apps SET manifest_json = ?, ref = ? WHERE id = ?`,
			string(manifestJSON), body.Ref, appID)
	}

	// Install row.
	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, config_encrypted, status, upgrade_policy, version, permissions_json, installed_by)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?, ?)`,
		appID, body.ProjectID, configEncrypted, upgradePolicy, manifest.Version, string(permsJSON), userID)
	if err != nil {
		http.Error(w, "create install: "+err.Error(), http.StatusInternalServerError)
		return
	}
	installID, _ := res.LastInsertId()
	log.Printf("[APPS] install user=%d app=%s install=%d project=%q version=%s",
		userID, manifest.Name, installID, body.ProjectID, manifest.Version)

	// Local-spawn path: pick the best delivery mode the manifest
	// declares — static (no sidecar, just assets), source (clone+build,
	// works on any host with Go), then per-platform binaries, then fall
	// back. Failures flip the install row to 'error' with the message
	// stored.
	preferLocal := os.Getenv("APTEVA_APPS_REMOTE") == "" // default: local mode
	if preferLocal {
		if manifest.Runtime.Kind == "static" {
			// Static apps don't fork a process — installLocally
			// handles them inline (validates static_dir, persists the
			// `static://` marker, remounts the HTTP table). Returning
			// synchronously is fine because there's nothing to wait
			// for. Errors bubble back as the JSON status field.
			if err := s.installLocally(installID, manifest, body.ProjectID, body.Config); err != nil {
				log.Printf("[APPS-STATIC] install %d failed: %v", installID, err)
				writeJSON(w, map[string]any{
					"install_id": installID,
					"app_id":     appID,
					"status":     "error",
					"error":      err.Error(),
				})
				return
			}
			writeJSON(w, map[string]any{
				"install_id": installID,
				"app_id":     appID,
				"status":     "running",
				"mount_path": resolveMountPath(manifest, body.Config),
				"next_step":  "Static UI app mounted. Open the URL prefix shown in `mount_path` to view it.",
			})
			return
		}
		if manifest.Runtime.Kind == "source" || manifest.Runtime.Source != nil {
			go func() {
				if err := s.installFromSource(installID, manifest, body.ProjectID, body.Config); err != nil {
					log.Printf("[APPS-SOURCE] install %d failed: %v", installID, err)
				}
			}()
			writeJSON(w, map[string]any{
				"install_id": installID,
				"app_id":     appID,
				"status":     "building",
				"next_step":  "Apteva is cloning the repo and running `go build`. First builds take 30-60s while dependencies download; subsequent installs of the same version are cached. Refresh the Apps tab — status will be 'running' once health checks pass, or 'error' with details if the build fails.",
			})
			return
		}
		if _, ok := manifest.Runtime.Binaries[localPlatform()]; ok {
			go func() {
				if err := s.installLocally(installID, manifest, body.ProjectID, body.Config); err != nil {
					log.Printf("[APPS-LOCAL] install %d failed: %v", installID, err)
				}
			}()
			writeJSON(w, map[string]any{
				"install_id": installID,
				"app_id":     appID,
				"status":     "spawning",
				"next_step":  fmt.Sprintf("Apteva is downloading the binary for %s and starting it as a subprocess. Refresh the Apps tab in a few seconds — status will be 'running' once health checks pass.", localPlatform()),
			})
			return
		}
		log.Printf("[APPS-LOCAL] no source or binary for %s in manifest; falling back to manual mount", localPlatform())
	}

	writeJSON(w, map[string]any{
		"install_id": installID,
		"app_id":     appID,
		"status":     "pending",
		"next_step":  "Manifest has no source or binary for this platform. Add a source: block, add a binaries[" + localPlatform() + "] entry, or run the sidecar yourself and Mount it by URL.",
	})
}

// DELETE /api/apps/installs/:id
func (s *Server) handleUninstallApp(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	installID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	// Reverse-dependency check: refuse if uninstalling this app would
	// orphan another running install whose manifest hard-requires it.
	// Operators can override with ?force=1 (CLI / scripted uninstalls);
	// the dashboard never sets force, so the check is the user-facing
	// safety net.
	force := r.URL.Query().Get("force") == "1"
	if !force {
		if blockers, err := s.dependentsBlockingUninstall(installID); err == nil && len(blockers) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":      "uninstall blocked — other apps require this one",
				"dependents": blockers,
				"hint":       "uninstall the dependents first, or pass ?force=1 to override.",
			})
			return
		}
	}
	// Detach in-memory mount first so further proxy calls 404 immediately.
	s.installedApps.Remove(installID)
	// Refresh the static-app prefix table so a kind=static install
	// stops being served immediately. No-op when this install was a
	// sidecar app (the rebuilt table is identical).
	s.RemountStaticApps()
	// Stop the local subprocess if any.
	if s.localApps != nil {
		_ = s.localApps.Stop(installID)
	}
	if _, err := s.store.db.Exec(`DELETE FROM app_instance_bindings WHERE install_id = ?`, installID); err != nil {
		log.Printf("[APPS] delete bindings: %v", err)
	}
	if _, err := s.store.db.Exec(`DELETE FROM app_installs WHERE id = ?`, installID); err != nil {
		http.Error(w, "delete install: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "uninstalled"})
}

// PUT /api/apps/installs/:id/status — operator-side status flip.
// Used today as the manual "I deployed the sidecar; mount it" trigger.
// In the orchestrator-driven flow this becomes automatic.
func (s *Server) handleSetInstallStatus(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "status" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var body struct {
		Status      string `json:"status"`
		ServiceName string `json:"service_name"`
		SidecarURL  string `json:"sidecar_url"` // local-dev override; bypasses orchestrator lookup
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Status != "running" && body.Status != "disabled" && body.Status != "error" {
		http.Error(w, "status must be running|disabled|error", http.StatusBadRequest)
		return
	}
	upd, err := s.store.db.Exec(
		`UPDATE app_installs SET
			status = ?,
			service_name = COALESCE(NULLIF(?, ''), service_name),
			sidecar_url_override = COALESCE(NULLIF(?, ''), sidecar_url_override)
		 WHERE id = ?`,
		body.Status, body.ServiceName, body.SidecarURL, installID)
	if err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := upd.RowsAffected(); n == 0 {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	// Refresh the in-memory registry so the change takes effect now.
	s.installedApps.Remove(installID)
	if body.Status == "running" {
		s.LoadInstalledApps()
	}
	writeJSON(w, map[string]string{"status": body.Status})
}

// POST /api/apps/installs/:id/upgrade — apply the available version.
//
// For built-in apps the new code is already running (it's part of the
// server binary); this just bumps app_installs.version to the bundled
// manifest's version, which clears the dashboard's "update available"
// badge.
//
// For source/registry installs (not yet wired) this would re-run the
// install pipeline at the configured ref. The handler returns 501 in
// that case so the caller knows to fall back to uninstall + reinstall.
func (s *Server) handleUpgradeApp(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "upgrade" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var (
		source, manifestJSON, currentVersion string
	)
	err = s.store.db.QueryRow(
		`SELECT a.source, a.manifest_json, i.version
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&source, &manifestJSON, &currentVersion)
	if err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		http.Error(w, "manifest parse: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if source != "builtin" {
		http.Error(w, "upgrade for non-builtin apps not implemented — uninstall + reinstall at the new ref", http.StatusNotImplemented)
		return
	}
	if manifest.Version == "" {
		http.Error(w, "no available version in manifest", http.StatusInternalServerError)
		return
	}
	if manifest.Version == currentVersion {
		writeJSON(w, map[string]string{
			"status":  "up-to-date",
			"version": currentVersion,
		})
		return
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET version = ? WHERE id = ?`,
		manifest.Version, installID,
	); err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"status":  "upgraded",
		"version": manifest.Version,
	})
}

// PUT /api/apps/installs/:id/instances — set the binding list.
//
// Body: { "instance_ids": [1, 2, 3] } — exactly these instances are
// bound; everything else is removed.
func (s *Server) handleSetInstallBindings(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "instances" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var body struct {
		InstanceIDs []int64 `json:"instance_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "begin: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM app_instance_bindings WHERE install_id = ?`, installID); err != nil {
		http.Error(w, "clear bindings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, iid := range body.InstanceIDs {
		if _, err := tx.Exec(
			`INSERT INTO app_instance_bindings (install_id, instance_id, enabled) VALUES (?, ?, 1)`,
			installID, iid); err != nil {
			http.Error(w, "insert binding: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "bound": body.InstanceIDs})
}

// fetchManifestBytes — pulls the YAML from a URL OR returns the inline
// payload. Trusted only as far as the URL the caller provided; the
// parsed manifest is then validated.
func (s *Server) fetchManifestBytes(manifestURL, inline string) ([]byte, error) {
	if inline != "" {
		return []byte(inline), nil
	}
	if manifestURL == "" {
		return nil, fmt.Errorf("manifest_url or manifest_yaml required")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(manifestURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", manifestURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch %s: http %d", manifestURL, resp.StatusCode)
	}
	const maxManifest = 256 * 1024 // 256 KiB is plenty for any manifest
	return io.ReadAll(io.LimitReader(resp.Body, maxManifest))
}

func manifestAllowsScope(m *sdk.Manifest, scope sdk.Scope) bool {
	for _, s := range m.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// normalizeAppName collapses an app identifier to a single canonical
// form so registry entries match installed rows + bundled slugs even
// when names diverge. "channel-chat", "channelchat", and "Channel Chat"
// all collapse to "channelchat".
func normalizeAppName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}
	return string(out)
}
