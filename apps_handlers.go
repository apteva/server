package main

// HTTP handlers for /api/apps — list installed apps, install from a
// manifest URL, configure / uninstall / bind to agents. The actual
// sidecar deploy goes through the existing orchestrator (POST
// /api/v1/services).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"

	"github.com/apteva/server/apps/framework"
)

// AppRow — what /api/apps returns for the dashboard's Installed view.
type AppRow struct {
	InstallID        int64            `json:"install_id"`
	AppID            int64            `json:"app_id"`
	Name             string           `json:"name"`
	DisplayName      string           `json:"display_name"`
	Version          string           `json:"version"`
	AvailableVersion string           `json:"available_version,omitempty"`
	Description      string           `json:"description"`
	Icon             string           `json:"icon"`
	IconStyle        string           `json:"icon_style,omitempty"`
	ProjectID        string           `json:"project_id"`
	Status           string           `json:"status"`
	StatusMessage    string           `json:"status_message,omitempty"`
	ErrorMessage     string           `json:"error_message,omitempty"`
	Source           string           `json:"source"`
	UpgradePolicy    string           `json:"upgrade_policy"`
	Permissions      []sdk.Permission `json:"permissions"`
	Surfaces         AppSurfaces      `json:"surfaces"`
	Deprecated       bool             `json:"deprecated,omitempty"`
	Deprecation      string           `json:"deprecation,omitempty"`
	Replacement      string           `json:"replacement,omitempty"`
	UIPanels         []sdk.UIPanel    `json:"ui_panels,omitempty"`
	// UISurfaces are code-free native UI descriptors. Mobile clients use
	// Entry through the authenticated /api/apps/<name>/... proxy and validate
	// the downloaded document against Schema before rendering it.
	UISurfaces []sdk.UISurface `json:"ui_surfaces,omitempty"`
	// UIComponents — chat-attachment + sidebar-widget components
	// declared in the install's manifest. The dashboard reads this
	// to know which {app, name} pairs the agent's respond(components)
	// call can target. Empty / omitted for apps that don't declare any.
	UIComponents []sdk.UIComponent `json:"ui_components,omitempty"`
	// Publishes — topics this app's manifest declares it emits on
	// the AppBus via ctx.Emit. Drives the dashboard subscription
	// form's event dropdown. Empty for apps that haven't documented
	// their emissions yet — the form falls back to free-text.
	Publishes []sdk.EventDecl `json:"publishes,omitempty"`
	// Bindings: role → connection_id | install_id | null. Empty when
	// the install's manifest declares no requires.integrations.
	Bindings map[string]any `json:"bindings,omitempty"`
	// HasPendingOptions: true when an optional integration role is
	// currently unbound but a compatible target now exists in the
	// project. Drives the "configure" banner in the install detail.
	HasPendingOptions bool `json:"has_pending_options,omitempty"`
	// Imports: app-owned declarative import sources, if the manifest
	// exposes any. The dashboard renders these as manual import actions.
	Imports map[string]any `json:"imports,omitempty"`
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
	Kind            string   `json:"kind"` // service | source | static
	MCPToolCount    int      `json:"mcp_tool_count"`
	MCPToolNames    []string `json:"mcp_tool_names,omitempty"`
	SkillCount      int      `json:"skill_count"`
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
	RequiredApps []AppDependency `json:"required_apps,omitempty"`
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
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Repo        string   `json:"repo"`
	ManifestURL string   `json:"manifest_url"`
	Icon        string   `json:"icon"`
	IconStyle   string   `json:"icon_style,omitempty"`
	Tags        []string `json:"tags"`
	Official    bool     `json:"official"`
	Category    string   `json:"category"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	Deprecation string   `json:"deprecation,omitempty"`
	Replacement string   `json:"replacement,omitempty"`
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
	search := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("q")))
	category := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("category")))
	if category == "all" {
		category = ""
	}
	page := 1
	if v := strings.TrimSpace(r.URL.Query().Get("page")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := 0
	if v := strings.TrimSpace(r.URL.Query().Get("page_size")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
			if pageSize > 100 {
				pageSize = 100
			}
		}
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
	// Scope the installed set to what's visible in the current
	// project: own installs + globals. Without the project_id filter
	// an install in Project A would mark the marketplace entry as
	// "Installed" when viewing Project B, blocking the user from
	// installing the app separately for B (which is the legitimate
	// use case for project-scoped installs in the first place).
	projectID := r.URL.Query().Get("project_id")
	installed := map[string]bool{}
	addInstalled := func(name string) {
		if name == "" {
			return
		}
		installed[normalizeAppName(name)] = true
	}
	var installRows *sql.Rows
	var qerr error
	if projectID != "" {
		installRows, qerr = s.store.db.Query(
			`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id
			 WHERE i.project_id = '' OR i.project_id = ?`, projectID)
	} else {
		// No project context (rare — pre-project view, admin tools).
		// Fall back to the unfiltered "any install anywhere" view.
		installRows, qerr = s.store.db.Query(
			`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id`)
	}
	if qerr == nil {
		for installRows.Next() {
			var n string
			if installRows.Scan(&n) == nil {
				addInstalled(n)
			}
		}
		installRows.Close()
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
	internal := map[string]bool{}
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
				if m.Internal {
					internal[key] = true
				}
			}
		}
	}
	categoryCounts := map[string]int{}
	filtered := make([]RegistryEntry, 0, len(reg.Apps))
	for _, e := range reg.Apps {
		// Internal framework components use the app runtime as an
		// implementation detail. They are neither installable products nor
		// operator-managed inventory, so omit them from Marketplace too.
		if internal[normalizeAppName(e.Name)] {
			continue
		}
		entryCategory := strings.ToLower(e.Category)
		if entryCategory == "" {
			entryCategory = "other"
		}
		if search != "" {
			hay := strings.ToLower(strings.Join(append([]string{
				e.Name, e.DisplayName, e.Description,
			}, e.Tags...), " "))
			if !strings.Contains(hay, search) {
				continue
			}
		}
		categoryCounts[entryCategory]++
		if category != "" && entryCategory != category {
			continue
		}
		filtered = append(filtered, e)
	}
	total := len(filtered)
	pageEntries := filtered
	if pageSize > 0 {
		start := (page - 1) * pageSize
		if start >= len(filtered) {
			pageEntries = []RegistryEntry{}
		} else {
			end := start + pageSize
			if end > len(filtered) {
				end = len(filtered)
			}
			pageEntries = filtered[start:end]
		}
	}

	// Resolve manifest URLs in parallel (with cache) only for the
	// filtered page. The old no-pagination path still enriches every
	// row for compatibility, but paged callers avoid N network/cache
	// lookups just to render the first marketplace screen.
	surfacesByName := map[string]AppSurfaces{}
	versionByName := map[string]string{}
	iconByName := map[string]string{}
	iconStyleByName := map[string]string{}
	for k, v := range builtinSurfaces {
		surfacesByName[k] = v
	}
	{
		type result struct {
			name      string
			surf      AppSurfaces
			version   string
			icon      string
			iconStyle string
		}
		ch := make(chan result, len(reg.Apps))
		dispatched := 0
		for _, e := range pageEntries {
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
				ch <- result{
					name:      name,
					surf:      surfacesFromManifest(m),
					version:   m.Version,
					icon:      resolveMarketplaceAppIcon(url, m.Icon),
					iconStyle: m.IconStyle,
				}
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
			if r.icon != "" {
				iconByName[key] = r.icon
			}
			if r.iconStyle != "" {
				iconStyleByName[key] = r.iconStyle
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

	out := make([]entryWithStatus, 0, len(pageEntries))
	for _, e := range pageEntries {
		key := normalizeAppName(e.Name)
		// Override the registry's hardcoded version with the live
		// manifest's version when we successfully fetched it. The
		// registry tends to drift behind real releases — showing the
		// stale value confuses operators ("I just bumped storage to
		// 0.1.1, why does the marketplace still say 0.1.0?").
		if v, ok := versionByName[key]; ok && v != "" {
			e.Version = v
		}
		if icon, ok := iconByName[key]; ok && icon != "" {
			e.Icon = icon
		}
		if style, ok := iconStyleByName[key]; ok && style != "" {
			e.IconStyle = style
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
		"total":        total,
		"page":         page,
		"page_size":    pageSize,
		"categories":   categoryCounts,
	})
}

func getRegistryURLFromEnv() string {
	return os.Getenv("APTEVA_APP_REGISTRY_URL")
}

// resolveInstalledAppIcon turns an app-relative identity asset into the same
// authenticated, install-scoped proxy URL used for panel/component modules.
// Absolute legacy URLs pass through unchanged.
func resolveInstalledAppIcon(appName, icon, version string, installID int64, projectID string) string {
	icon = strings.TrimSpace(icon)
	if icon == "" || !strings.HasPrefix(icon, "/ui/") {
		return icon
	}
	params := url.Values{}
	if version != "" {
		params.Set("v", version)
	}
	if installID > 0 {
		params.Set("install_id", strconv.FormatInt(installID, 10))
	}
	if projectID != "" {
		params.Set("project_id", projectID)
	}
	resolved := "/api/apps/" + url.PathEscape(appName) + icon
	if query := params.Encode(); query != "" {
		resolved += "?" + query
	}
	return resolved
}

// resolveMarketplaceAppIcon resolves the same app-relative icon against the
// public manifest URL. Marketplace cards render before an app has a sidecar to
// proxy, so they need the repository-hosted form of the one canonical asset.
func resolveMarketplaceAppIcon(manifestURL, icon string) string {
	icon = strings.TrimSpace(icon)
	if icon == "" {
		return ""
	}
	ref, err := url.Parse(icon)
	if err != nil || ref.IsAbs() || strings.HasPrefix(icon, "data:") {
		return icon
	}
	base, err := url.Parse(manifestURL)
	if err != nil || !base.IsAbs() {
		return icon
	}
	// A manifest icon is app-root relative even though it starts with "/ui/".
	// Strip the leading slash before ResolveReference so raw GitHub URLs keep
	// the repository/ref/entry prefix rather than resolving from host root.
	relative, err := url.Parse(strings.TrimPrefix(icon, "/"))
	if err != nil {
		return icon
	}
	return base.ResolveReference(relative).String()
}

// deriveManifestURL converts a manifest's runtime.source (github
// owner/repo + ref + entry path) into the raw URL of the upstream
// apteva.yaml. Returns "" when the source isn't github-shaped — the
// caller falls back to the stored snapshot.
func deriveManifestURL(m *sdk.Manifest) string {
	if m == nil {
		return ""
	}
	s := m.Runtime.Source
	if s == nil || s.Repo == "" {
		return ""
	}
	repo := strings.TrimPrefix(s.Repo, "https://")
	repo = strings.TrimPrefix(repo, "http://")
	repo = strings.TrimSuffix(repo, ".git")
	if !strings.HasPrefix(repo, "github.com/") {
		return ""
	}
	ownerAndRepo := strings.TrimPrefix(repo, "github.com/")
	ref := s.Ref
	if ref == "" {
		ref = "main"
	}
	entry := s.Entry
	if entry == "" || entry == "." {
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/apteva.yaml", ownerAndRepo, ref)
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/apteva.yaml", ownerAndRepo, ref, strings.Trim(entry, "/"))
}

// updateManifestURL returns the moving manifest URL used to discover and
// execute upgrades. Curated apps must follow the registry's manifest_url:
// the runtime ref in an installed manifest is an immutable release tag and
// therefore cannot advertise the next release. Unregistered source apps fall
// back to their declared runtime source.
func (s *Server) updateManifestURL(appName string, installed *sdk.Manifest) string {
	if url := s.lookupRegistryManifestURL(appName); url != "" {
		return url
	}
	return deriveManifestURL(installed)
}

// refreshManifestFromUpstream re-fetches the live apteva.yaml from
// the app's moving update channel and writes it back into apps.manifest_json
// so the dashboard's "update available" detector compares the
// installed version against what's actually upstream — not against
// the snapshot taken at install time, and not against the running
// sidecar (which always reports its own embedded version, so an
// install can never lag itself).
//
// Best-effort: cache-backed fetch, errors leave the row untouched.
func (s *Server) refreshManifestFromUpstream(appName, manifestJSON string) {
	if appName == "" || manifestJSON == "" {
		return
	}
	var current sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &current); err != nil {
		return
	}
	url := s.updateManifestURL(appName, &current)
	if url == "" {
		return
	}
	live, err := s.fetchAndCacheManifest(url)
	if err != nil || live == nil {
		return
	}
	if live.Version == "" || live.Version == current.Version {
		return
	}
	s.updateAppCatalogMetadataByName(appName, live)
}

// lookupRegistryManifestURL returns the manifest_url declared in the
// curated registry for appName, or "" if the registry can't be
// reached or doesn't list the app. Used as a fallback when the
// installed manifest doesn't declare runtime.source (e.g. apps that
// install from a runtime.bundle and never had a clone-from-source
// path). Registry payload is cached in fetchAndCacheRegistry to keep
// this cheap on every list call.
func (s *Server) lookupRegistryManifestURL(appName string) string {
	if appName == "" {
		return ""
	}
	reg, err := s.fetchAndCacheRegistry()
	if err != nil || reg == nil {
		return ""
	}
	target := normalizeAppName(appName)
	for _, e := range reg.Apps {
		if normalizeAppName(e.Name) == target {
			return e.ManifestURL
		}
	}
	return ""
}

// GET /api/apps[?project_id=X]
//
// Returns one row per install visible to the caller — project installs
// for the requested project plus all globals. Built-in apps appear with
// source='builtin'.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	q := `
		SELECT i.id, i.app_id, i.project_id, i.status, i.status_message, i.error_message,
			i.upgrade_policy, i.version, i.permissions_json, a.name,
			COALESCE(NULLIF(i.source, ''), a.source),
			COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json), a.manifest_json,
			COALESCE(i.integration_bindings, '{}'), COALESCE(i.has_pending_options, 0)
		FROM app_installs i JOIN apps a ON a.id = i.app_id`
	args := []any{}
	if projectID != "" {
		q += ` WHERE i.project_id = '' OR i.project_id = ?`
		args = append(args, projectID)
	} else if s.store.GetPlatformRole(getUserID(r)) != PlatformAdmin {
		q += ` WHERE i.project_id = ''`
	}
	q += ` ORDER BY a.name`
	// Refresh manifest_json from upstream IN THE BACKGROUND. The list
	// returns whatever's already stored; the next poll picks up any
	// upstream version bumps after the goroutine writes them back.
	//
	// Pre-fix: this loop ran synchronously inside the handler. With N
	// non-builtin installs, the cold-cache path did up to N × 8s of
	// sequential HTTP fetches to GitHub before the response was sent.
	// The dashboard polls /api/apps every few seconds, so polls piled
	// up faster than they could complete and the agent detail page
	// (which depends on this endpoint loading) hung indefinitely.
	type appPair struct{ name, manifestJSON string }
	var pairs []appPair
	if rs, err := s.store.db.Query(`SELECT name, manifest_json FROM apps WHERE source != 'builtin'`); err == nil {
		for rs.Next() {
			var p appPair
			if rs.Scan(&p.name, &p.manifestJSON) == nil {
				pairs = append(pairs, p)
			}
		}
		rs.Close()
	}
	// Coalesce concurrent dashboard polls onto a single in-flight
	// refresh. fetchAndCacheManifest is per-URL cached for a minute,
	// but without this guard 10 simultaneous polls on a cold cache
	// would each spawn a goroutine doing the same N fetches in
	// parallel — cheap relative to blocking the response, but still
	// 10× the upstream traffic.
	if s.manifestRefreshInFlight.CompareAndSwap(false, true) {
		go func(pairs []appPair) {
			defer s.manifestRefreshInFlight.Store(false)
			for _, p := range pairs {
				s.refreshManifestFromUpstream(p.name, p.manifestJSON)
			}
		}(pairs)
	}
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := []AppRow{}
	for rows.Next() {
		var (
			installID, appID                                                int64
			projID, status, statusMsg, errMsg                               string
			upgradePolicy, version, permsJSON                               string
			name, source, manifestJSON, availableManifestJSON, bindingsJSON string
			hasPendingOptions                                               int
		)
		if err := rows.Scan(&installID, &appID, &projID, &status, &statusMsg, &errMsg,
			&upgradePolicy, &version, &permsJSON, &name, &source, &manifestJSON, &availableManifestJSON,
			&bindingsJSON, &hasPendingOptions); err != nil {
			continue
		}
		var manifest sdk.Manifest
		_ = json.Unmarshal([]byte(manifestJSON), &manifest)
		var availableManifest sdk.Manifest
		_ = json.Unmarshal([]byte(availableManifestJSON), &availableManifest)
		availableVersion := availableManifest.Version
		if availableVersion == "" || semverLess(availableVersion, version) {
			availableVersion = version
		}
		var perms []sdk.Permission
		_ = json.Unmarshal([]byte(permsJSON), &perms)
		var bindings map[string]any
		_ = json.Unmarshal([]byte(bindingsJSON), &bindings)
		surfaces := surfacesFromManifest(&manifest)
		// For static UI apps, swap the manifest-default mount path for
		// the per-install resolved one (config.mount_path overrides the
		// manifest default). The live registry already computed this at
		// boot/install time, so we just read it back here. Without this
		// the dashboard's "Open" link would point at the manifest
		// default even after the operator changed URL prefix in the
		// install config.
		if surfaces.UIApp {
			if entry := s.installedApps.Get(installID); entry != nil && entry.MountPath != "" {
				surfaces.UIAppMount = entry.MountPath
			}
		}
		depInfo, isDeprecated := deprecatedApp(name)
		out = append(out, AppRow{
			InstallID: installID, AppID: appID, Name: name, DisplayName: manifest.DisplayName,
			Bindings:          bindings,
			HasPendingOptions: hasPendingOptions != 0,
			Version:           version,
			AvailableVersion:  availableVersion,
			Description:       manifest.Description,
			Icon: resolveInstalledAppIcon(
				name,
				manifest.Icon,
				version,
				installID,
				firstNonEmpty(projectID, projID),
			),
			IconStyle: manifest.IconStyle,
			ProjectID: projID, Status: status, StatusMessage: statusMsg, ErrorMessage: errMsg,
			Source: source, UpgradePolicy: upgradePolicy,
			Permissions: perms, Surfaces: surfaces,
			Deprecated:   isDeprecated,
			Deprecation:  depInfo.Message,
			Replacement:  depInfo.Replacement,
			UIPanels:     manifest.Provides.UIPanels,
			UISurfaces:   manifest.Provides.UISurfaces,
			UIComponents: manifest.Provides.UIComponents,
			Publishes:    manifest.Provides.Publishes,
			Imports:      manifest.Imports,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows.Close()

	// Backfill Surfaces.RequiredApps[].Installed from the live install
	// set so the install-detail panel shows the correct ✓/✗ badge.
	// surfacesFromManifest only fills name/version/reason/optional —
	// the "is this dep installed?" lookup needs the full visible
	// install set, which we have right here. Without this, every
	// requires.apps entry rendered as "missing" even when the dep
	// was clearly installed in the same project.
	installedNames := map[string]bool{}
	for _, r := range out {
		installedNames[normalizeAppName(r.Name)] = true
	}
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			for _, k := range []string{m.Slug, m.Name} {
				if k != "" {
					installedNames[normalizeAppName(k)] = true
				}
			}
		}
	}
	for i := range out {
		for j := range out[i].Surfaces.RequiredApps {
			depKey := normalizeAppName(out[i].Surfaces.RequiredApps[j].Name)
			out[i].Surfaces.RequiredApps[j].Installed = installedNames[depKey]
		}
	}

	// Append integration rows: every connection in this project whose
	// integration declares ui_components surfaces here as a synthetic
	// AppRow so the dashboard's chat-component lookup finds it via
	// the same `apps[]` array. Source flips to "integration" so the
	// UI can distinguish (badges, settings link) without a new endpoint.
	if projectID != "" && s.catalog != nil {
		seen := map[string]bool{}
		// Track app names already in `out` so app/integration slugs
		// don't shadow each other (highly unusual but defensive).
		for _, r := range out {
			seen[r.Name] = true
		}
		connRows, err := s.store.db.Query(
			`SELECT DISTINCT app_slug FROM connections WHERE project_id = ? AND status != 'disabled'`,
			projectID,
		)
		if err == nil {
			defer connRows.Close()
			for connRows.Next() {
				var slug string
				if connRows.Scan(&slug) != nil || seen[slug] {
					continue
				}
				seen[slug] = true
				tmpl := s.catalog.Get(slug)
				if tmpl == nil || len(tmpl.UIComponents) == 0 {
					continue
				}
				// Translate IntegrationUIComponent → sdk.UIComponent
				// shape so the dashboard can stay agnostic about
				// the source.
				uiComps := make([]sdk.UIComponent, 0, len(tmpl.UIComponents))
				for _, c := range tmpl.UIComponents {
					uiComps = append(uiComps, sdk.UIComponent{
						Name:         c.Name,
						Entry:        c.Entry,
						Slots:        c.Slots,
						PropsSchema:  c.PropsSchema,
						PreviewProps: c.PreviewProps,
					})
				}
				icon := ""
				if tmpl.Logo != nil {
					icon = *tmpl.Logo
				}
				out = append(out, AppRow{
					Name:         slug,
					DisplayName:  tmpl.Name,
					Description:  tmpl.Description,
					Icon:         icon,
					IconStyle:    "image",
					Status:       "running",
					Source:       "integration",
					Version:      "1.0.0",
					ProjectID:    projectID,
					UIComponents: uiComps,
				})
			}
		}
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
		SkillCount:      len(m.Provides.Skills),
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
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
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
//
//	upgrade_policy: "manual"|"auto-patch"|"auto-minor" }
//
// MVP: creates the apps + app_installs rows in 'pending' state and
// returns. Sidecar deployment via the orchestrator + status flip to
// 'running' is handled by a follow-up reconcile (not in this slice —
// for now the operator runs `./scripts/admin install-app <id>` or sets
// status='running' manually after deploying the image).
func (s *Server) handleInstallApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	userID := getUserID(r)
	var body struct {
		ManifestURL   string            `json:"manifest_url"`
		ManifestYAML  string            `json:"manifest_yaml"`
		Repo          string            `json:"repo"`
		Ref           string            `json:"ref"`
		ProjectID     string            `json:"project_id"`
		Config        map[string]string `json:"config"`
		UpgradePolicy string            `json:"upgrade_policy"`
		// Bindings: role → connection_id (kind=integration) or
		// install_id (kind=app) | null. Sent by the dashboard's
		// install modal after the operator picks targets for each
		// requires.integrations role. Required roles MUST have a
		// non-null binding; the install handler validates this
		// after parsing the manifest.
		Bindings map[string]any `json:"bindings"`
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
	if info, ok := deprecatedApp(manifest.Name); ok {
		displayName := manifest.DisplayName
		if displayName == "" {
			displayName = manifest.Name
		}
		writeJSONStatus(w, http.StatusGone, map[string]any{
			"error":       fmt.Sprintf("%s is deprecated and can no longer be installed", displayName),
			"app":         manifest.Name,
			"deprecated":  true,
			"deprecation": info.Message,
			"replacement": info.Replacement,
		})
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

	if body.Bindings == nil {
		body.Bindings = map[string]any{}
	}

	// Cascade-install dependencies declared in requires.apps. Walks
	// the dep graph in topo order (deps before the dependent),
	// detects cycles, skips already-installed apps. Optional deps are
	// opt-in: the cascade only resolves them when the operator supplied
	// a non-null binding/intent for that app name.
	//
	// Returns a map of dep_name → resolved install_id which we merge
	// into body.Bindings below — that's what makes installBoundApp
	// able to authorize backup→jobs (and every other requires.apps
	// caller) without manual binding by the operator.
	depBindings := map[string]int64{}
	if len(manifest.Requires.Apps) > 0 {
		out, err := s.installDependencies(userID, manifest, body.ProjectID, body.Bindings)
		if err != nil {
			http.Error(w, "dependency install: "+err.Error(), http.StatusBadGateway)
			return
		}
		depBindings = out
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
		s.updateAppCatalogMetadata(appID, manifest, source, body.Repo, body.Ref)
	}

	// Validate bindings against requires.integrations: required roles
	// must have a non-null target; unknown role names are rejected.
	// Merge the cascade's resolved app-dep install_ids into the
	// bindings map so installBoundApp can authorize app→app calls
	// for requires.apps deps. Operator-supplied keys win — if the
	// dashboard's preflight let the user pick a specific bound
	// install (e.g. when two installs of the dep coexist), don't
	// stomp it with the cascade's first match.
	for name, id := range depBindings {
		if _, present := body.Bindings[name]; !present {
			body.Bindings[name] = id
		}
	}
	if err := normalizeManifestIntegrationBindings(manifest, body.Bindings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, dep := range manifest.Requires.Apps {
		if dep.Optional {
			continue
		}
		raw, present := body.Bindings[dep.Name]
		if !present || raw == nil {
			http.Error(w,
				fmt.Sprintf("required app dep %q is unbound", dep.Name),
				http.StatusBadRequest,
			)
			return
		}
	}
	// Strip unknown keys to keep the bindings JSON tidy. Allow-set
	// is the union of integration roles AND requires.apps[].name —
	// without the latter, the cascade-written app-dep ids we just
	// merged in would be deleted right back out.
	allowed := make(map[string]bool, len(manifest.Requires.Integrations)+len(manifest.Requires.Apps))
	for _, dep := range manifest.Requires.Integrations {
		allowed[dep.Role] = true
	}
	for _, dep := range manifest.Requires.Apps {
		allowed[dep.Name] = true
	}
	for k := range body.Bindings {
		if !allowed[k] {
			delete(body.Bindings, k)
		}
	}
	bindingsJSON, _ := json.Marshal(body.Bindings)

	// Install row.
	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs
		 (app_id, project_id, config_encrypted, status, upgrade_policy, version, manifest_json, source, repo, ref, permissions_json, installed_by, integration_bindings)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, body.ProjectID, configEncrypted, upgradePolicy, manifest.Version, string(manifestJSON), source, body.Repo, body.Ref,
		string(permsJSON), userID, string(bindingsJSON))
	if err != nil {
		http.Error(w, "create install: "+err.Error(), http.StatusInternalServerError)
		return
	}
	installID, _ := res.LastInsertId()
	log.Printf("[APPS] install user=%d app=%s install=%d project=%q version=%s",
		userID, manifest.Name, installID, body.ProjectID, manifest.Version)

	// Skills shipped by this manifest. Best-effort — a body_file
	// fetch failure logs but doesn't fail the install (the rest of
	// the app still works without its skills, and the operator can
	// re-run from /apps to re-register).
	if len(manifest.Provides.Skills) > 0 {
		fetcher := s.makeSkillBodyFileFetcher(body.ManifestURL)
		if err := s.registerAppSkills(installID, manifest.Name, body.ProjectID, manifest.Provides.Skills, fetcher); err != nil {
			log.Printf("[APPS-SKILLS] register install=%d failed: %v", installID, err)
		} else {
			log.Printf("[APPS-SKILLS] registered %d skill(s) for install=%d", len(manifest.Provides.Skills), installID)
		}
	}

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
			// Outermost slot acquisition — gates concurrent top-level
			// installs across the host. Dep-recursion inside
			// installFromSource doesn't re-acquire (would deadlock).
			// "Queued" status surfaced before the slot blocks so the
			// dashboard pill reads coherently while the user waits.
			s.store.db.Exec(`UPDATE app_installs SET status_message='Queued — waiting for a build slot' WHERE id=?`, installID)
			go func() {
				release := s.localApps.acquireBuildSlot()
				defer release()
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
			s.store.db.Exec(`UPDATE app_installs SET status_message='Queued — waiting for a build slot' WHERE id=?`, installID)
			go func() {
				release := s.localApps.acquireBuildSlot()
				defer release()
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
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	installID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	receipt := struct {
		Status      string `json:"status"`
		AppName     string `json:"app_name"`
		DisplayName string `json:"display_name"`
		InstallID   int64  `json:"install_id"`
		AppID       int64  `json:"app_id"`
		ProjectID   string `json:"project_id"`
		Version     string `json:"version"`
	}{Status: "uninstalled"}
	err = s.store.db.QueryRow(`
		SELECT ai.id, ai.app_id, a.name,
		       COALESCE(NULLIF(json_extract(COALESCE(NULLIF(ai.manifest_json, ''), a.manifest_json), '$.display_name'), ''), a.name),
		       COALESCE(ai.project_id, ''), COALESCE(ai.version, '')
		FROM app_installs ai
		JOIN apps a ON a.id = ai.app_id
		WHERE ai.id = ?`, installID).Scan(
		&receipt.InstallID,
		&receipt.AppID,
		&receipt.AppName,
		&receipt.DisplayName,
		&receipt.ProjectID,
		&receipt.Version,
	)
	if err == sql.ErrNoRows {
		http.Error(w, "app install not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load app install: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if requestedProjectID := strings.TrimSpace(r.URL.Query().Get("project_id")); requestedProjectID != "" && requestedProjectID != receipt.ProjectID {
		http.Error(w, "app install does not belong to requested project", http.StatusNotFound)
		return
	}
	// Reverse-dependency check: refuse if uninstalling this app would
	// orphan another running install whose manifest hard-requires it.
	// Operators can override with ?force=1 (CLI / scripted uninstalls);
	// the dashboard never sets force, so the check is the user-facing
	// safety net.
	force := r.URL.Query().Get("force") == "1"
	if !force {
		blockers, err := s.dependentsBlockingUninstall(installID)
		if err != nil {
			http.Error(w, "dependency check failed", http.StatusInternalServerError)
			return
		}
		if len(blockers) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":      "uninstall blocked — other apps require this one",
				"dependents": blockers,
				"hint":       "uninstall the dependents first, or pass ?force=1 to override.",
			})
			return
		}
		if deps, derr := s.dependentsOfApp(installID); derr != nil {
			http.Error(w, "dependency check failed", http.StatusInternalServerError)
			return
		} else if len(deps) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":      "app has dependents",
				"message":    formatDependents(deps),
				"dependents": deps,
				"hint":       "Unbind dependent apps first, or pass ?force=1 to override (apps will degrade).",
			})
			return
		}
	}
	// Capture assigned skill identities before the transaction removes their
	// catalog rows. Memory cleanup runs only after commit.
	var sweepProjectID string
	var sweepUserID int64
	var pendingSkills []struct {
		id   int64
		slug string
	}
	_ = s.store.db.QueryRow(`SELECT COALESCE(project_id,''), installed_by FROM app_installs WHERE id = ?`, installID).
		Scan(&sweepProjectID, &sweepUserID)
	skillRows, _ := s.store.db.Query(`SELECT id, slug FROM skills WHERE install_id = ?`, installID)
	if skillRows != nil {
		for skillRows.Next() {
			var rowID int64
			var rowSlug string
			if err := skillRows.Scan(&rowID, &rowSlug); err == nil {
				pendingSkills = append(pendingSkills, struct {
					id   int64
					slug string
				}{rowID, rowSlug})
			}
		}
		skillRows.Close()
	}
	s.cleanupInactiveIntegrationWebhooks(installID, true)
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "begin uninstall: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	statements := []struct {
		query string
		arg   any
	}{
		{`DELETE FROM app_agent_bindings WHERE install_id=?`, installID},
		{`DELETE FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(installID)},
		{`DELETE FROM skills WHERE install_id=?`, installID},
		{`DELETE FROM app_installs WHERE id=?`, installID},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement.query, statement.arg); err != nil {
			http.Error(w, "uninstall failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "uninstall commit failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Runtime side effects only happen after every authorization/dependency
	// check and the database transaction has committed.
	s.installedApps.Remove(installID)
	s.RemountStaticApps()
	if s.localApps != nil {
		if err := s.localApps.Stop(installID); err != nil {
			log.Printf("[APPS] uninstall committed but process stop failed install=%d: %v", installID, err)
		}
	}
	for _, skill := range pendingSkills {
		s.sweepSkillFromProject(sweepUserID, sweepProjectID, skill.id, skill.slug, "app uninstalled")
	}
	// Removed install may both eliminate options AND newly satisfy
	// other installs' optional deps if it was bound somewhere.
	s.recomputePendingOptions()
	writeJSON(w, receipt)
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
	effectiveSidecarURL := strings.TrimSpace(body.SidecarURL)
	if body.Status == "running" {
		var localBinPath string
		var localPort int64
		if err := s.store.db.QueryRow(
			`SELECT COALESCE(local_bin_path, ''), COALESCE(local_port, 0)
				   FROM app_installs
				  WHERE id = ?`,
			installID,
		).Scan(&localBinPath, &localPort); err == nil && localBinPath != "" && localPort > 0 {
			effectiveSidecarURL = localSidecarURL(localPort)
		}
	}
	upd, err := s.store.db.Exec(
		`UPDATE app_installs SET
				status = ?,
				service_name = COALESCE(NULLIF(?, ''), service_name),
				sidecar_url_override = COALESCE(NULLIF(?, ''), sidecar_url_override)
			 WHERE id = ?`,
		body.Status, body.ServiceName, effectiveSidecarURL, installID)
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

// PUT /api/apps/installs/:id/bindings
//
// Body: {role: connection_id|install_id|null, ...}
//
// Updates the install's integration_bindings in place. Used by the
// "App dependencies" section in the install detail page when the
// operator wants to bind a previously-skipped optional dep, swap a
// connection, or null one out. Validates required roles stay bound;
// rejects unknown role names.
func (s *Server) handleSetInstallBindings2(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "bindings" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	// Validate against the manifest's known keys: integration roles
	// AND requires.apps[].name (since installBoundApp reads bindings
	// keyed by app name for app-deps).
	allowed := make(map[string]bool, len(manifest.Requires.Integrations)+len(manifest.Requires.Apps))
	for _, dep := range manifest.Requires.Integrations {
		allowed[dep.Role] = true
	}
	for _, dep := range manifest.Requires.Apps {
		allowed[dep.Name] = true
	}
	for k := range body {
		if !allowed[k] {
			http.Error(w, "unknown role: "+k, http.StatusBadRequest)
			return
		}
	}
	// MERGE semantics: read the existing bindings JSON and overlay
	// the supplied keys. Missing keys are preserved — this lets the
	// dashboard's per-role panels (BackupPanel sends {cloud_storage}
	// only, not the full bindings) update one role without wiping
	// out cascade-written app-dep ids. Pass an explicit `null` to
	// unbind a role.
	merged := bindingsForInstall(s, installID)
	for k, v := range body {
		merged[k] = v
	}
	// A partial PUT must preserve other valid roles, but it must not
	// preserve roles removed by an app upgrade. The manifest remains
	// the complete authority for app-to-app grants.
	pruneUnknownManifestBindings(manifest, merged)
	if err := normalizeManifestIntegrationBindings(manifest, merged); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, dep := range manifest.Requires.Apps {
		if dep.Optional {
			continue
		}
		raw, present := merged[dep.Name]
		if !present || raw == nil {
			http.Error(w, "required app dep unbound: "+dep.Name, http.StatusBadRequest)
			return
		}
	}
	bj, _ := json.Marshal(merged)
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET integration_bindings = ?, has_pending_options = 0 WHERE id = ?`,
		string(bj), installID,
	); err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.recomputePendingOptions()
	s.cleanupInactiveIntegrationWebhooks(installID, false)

	// Bounce the sidecar so OnMount re-runs with the new bindings.
	// Without this, the binding change is recorded but the running
	// process never re-reads it (storage's S3-vs-disk decision and
	// every other bind-time resolution lives in OnMount). We still
	// return 200 if the respawn fails — the binding write succeeded;
	// the operator gets a status_message they can check.
	respawnErr := s.RespawnLocalInstall(installID)
	if respawnErr != nil {
		log.Printf("[APPS] bindings updated but respawn failed install=%d: %v", installID, respawnErr)
	}
	respawnErrMsg := ""
	if respawnErr != nil {
		respawnErrMsg = respawnErr.Error()
	}
	writeJSON(w, map[string]any{
		"ok":          true,
		"bindings":    merged,
		"respawned":   respawnErr == nil,
		"respawn_err": respawnErrMsg,
	})
}

// POST /api/apps/install/preflight
//
// Body: same shape as /api/apps/install (manifest_url | manifest_yaml,
// project_id) but does NOT write anything. Returns:
//
//	{
//	  "manifest": {...},
//	  "roles": [
//	    {
//	      "role": "provider",
//	      "kind": "integration",
//	      "label": "Image-generation provider",
//	      "required": true,
//	      "hint": "...",
//	      "capabilities": ["image.generate"],
//	      "compatible": ["openai-api", "replicate"],
//	      "candidates": [{"connection_id": 42, "app_slug": "openai-api", "name": "My OpenAI"}],
//	      "can_create_new": true
//	    },
//	    {
//	      "role": "storage",
//	      "kind": "app",
//	      "required": false,
//	      "candidates": [{"install_id": 17, "app_name": "storage", "display_name": "Storage"}]
//	    }
//	  ]
//	}
//
// Dashboard renders a step in the install modal per role. When the
// user submits, the resulting bindings JSON is passed to /install.
func (s *Server) handlePreflightApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	var body struct {
		ManifestURL  string `json:"manifest_url"`
		ManifestYAML string `json:"manifest_yaml"`
		ProjectID    string `json:"project_id"`
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
	roles := s.buildPreflightRoles(manifest, body.ProjectID, userID)
	writeJSON(w, map[string]any{
		"manifest": manifest,
		"roles":    roles,
	})
}

// handlePreflightInstalled — GET /api/apps/installs/:id/preflight
//
// Returns the same role summary as POST /apps/install/preflight, but
// derived from an installed app's stored manifest + project. Used by
// the install detail panel's "Edit dependencies" section so the
// operator can rebind roles without uninstalling.
//
// Also returns the install's current integration_bindings so the
// dashboard can pre-select the existing choices.
func (s *Server) handlePreflightInstalled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "preflight" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	userID := getUserID(r)
	var (
		projectID, manifestJSON string
	)
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(i.project_id,''), COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&projectID, &manifestJSON); err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		http.Error(w, "manifest parse: "+err.Error(), http.StatusInternalServerError)
		return
	}
	roles := s.buildPreflightRoles(&manifest, projectID, userID)
	writeJSON(w, map[string]any{
		"manifest":         manifest,
		"roles":            roles,
		"current_bindings": bindingsForInstall(s, installID),
	})
}

type installMCPToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) handleInstallTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "tools" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || installID <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if s.installedApps == nil {
		http.Error(w, "apps registry not configured", http.StatusInternalServerError)
		return
	}
	inst := s.installedApps.Get(installID)
	if inst == nil || inst.SidecarURL == "" {
		writeJSON(w, []installMCPToolInfo{})
		return
	}
	appToken, err := s.appInstallToken(installID)
	if err != nil {
		http.Error(w, "app credential unavailable", http.StatusServiceUnavailable)
		return
	}
	tools, err := listAppMCPTools(inst.SidecarURL+"/mcp", appToken)
	if err != nil {
		http.Error(w, "list tools: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, tools)
}

// buildPreflightRoles is the role-resolution core shared by the
// marketplace preflight (POST /apps/install/preflight) and the
// installed preflight (GET /apps/installs/:id/preflight). For each
// requires.integrations entry it lists compatible connections in
// the project; for each requires.apps entry it lists running app
// installs that match. The dashboard renders one RolePicker per row.
func (s *Server) buildPreflightRoles(manifest *sdk.Manifest, projectID string, userID int64) []preflightRole {
	roles := make([]preflightRole, 0, len(manifest.Requires.Integrations)+len(manifest.Requires.Apps))
	for _, dep := range manifest.Requires.Integrations {
		kind := dep.Kind
		if kind == "" {
			kind = "integration"
		}
		row := preflightRole{
			Role:         dep.Role,
			Kind:         kind,
			Mode:         dep.Mode,
			Label:        dep.Label,
			Required:     dep.Required,
			Hint:         dep.Hint,
			Capabilities: dep.Capabilities,
		}
		if appBindingIsMultipleForManifest(manifest, dep) {
			row.Mode = appBindingModeMultiple
		}
		if kind == "integration" {
			compatibleSlugs := dep.CompatibleSlugs
			if len(compatibleSlugs) == 0 && len(dep.CompatibleAppNames) > 0 {
				// Be permissive for existing manifests that accidentally used
				// compatible_app_names on a kind=integration role. The binding
				// picker matches connection app_slug values, so these are slugs
				// in practice for integration roles.
				compatibleSlugs = dep.CompatibleAppNames
			}
			row.Compatible = compatibleSlugs
			row.CanCreateNew = true
			conns, _ := s.store.ListConnections(userID, projectID)
			for _, c := range conns {
				if !contains(compatibleSlugs, c.AppSlug) {
					continue
				}
				// Tag candidates so the dashboard's role-picker
				// can render a "global" badge — pre-v0.15.0 every
				// candidate was project-scoped so the field didn't
				// need to exist; now an operator picking between
				// a project Slack and a global one needs the cue.
				scope := "project"
				if c.ProjectID == "" {
					scope = "global"
				}
				row.IntegrationCands = append(row.IntegrationCands, preflightIntegrationCandidate{
					ConnectionID: c.ID, AppSlug: c.AppSlug, Name: c.Name, Status: c.Status, Scope: scope,
				})
			}
		} else if kind == "app" {
			row.Compatible = dep.CompatibleAppNames
			row.CanCreateNew = false
			rs, err := s.store.db.Query(
				`SELECT i.id, a.name,
				        COALESCE(json_extract(COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json),'$.display_name'), a.name)
				 FROM app_installs i JOIN apps a ON a.id=i.app_id
				 WHERE i.status='running' AND (i.project_id = ? OR i.project_id = '')`,
				projectID,
			)
			if err == nil {
				for rs.Next() {
					var (
						instID             int64
						aName, displayName string
					)
					if rs.Scan(&instID, &aName, &displayName) == nil && contains(dep.CompatibleAppNames, aName) {
						row.AppCands = append(row.AppCands, preflightAppCandidate{
							InstallID: instID, AppName: aName, DisplayName: displayName,
						})
					}
				}
				rs.Close()
			}
		}
		roles = append(roles, row)
	}
	for _, dep := range manifest.Requires.Apps {
		row := preflightRole{
			Role:         dep.Name,
			Kind:         "app",
			Label:        dep.Name,
			Required:     !dep.Optional,
			Hint:         dep.Reason,
			Compatible:   []string{dep.Name},
			CanCreateNew: false,
		}
		rs, err := s.store.db.Query(
			`SELECT i.id, a.name,
			        COALESCE(json_extract(COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json),'$.display_name'), a.name)
			 FROM app_installs i JOIN apps a ON a.id=i.app_id
			 WHERE i.status='running' AND (i.project_id = ? OR i.project_id = '')`,
			projectID,
		)
		if err == nil {
			for rs.Next() {
				var (
					instID             int64
					aName, displayName string
				)
				if rs.Scan(&instID, &aName, &displayName) == nil && normalizeAppName(aName) == normalizeAppName(dep.Name) {
					row.AppCands = append(row.AppCands, preflightAppCandidate{
						InstallID: instID, AppName: aName, DisplayName: displayName,
					})
				}
			}
			rs.Close()
		}
		roles = append(roles, row)
	}
	return roles
}

type preflightIntegrationCandidate struct {
	ConnectionID int64  `json:"connection_id"`
	AppSlug      string `json:"app_slug"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	// Scope is "project" or "global"; the dashboard's role-picker
	// renders a badge so an operator binding the storage backend
	// can tell at a glance whether they're picking the project-
	// scoped R2 or the global one.
	Scope string `json:"scope,omitempty"`
}

type preflightAppCandidate struct {
	InstallID   int64  `json:"install_id"`
	AppName     string `json:"app_name"`
	DisplayName string `json:"display_name"`
}

type preflightRole struct {
	Role             string                          `json:"role"`
	Kind             string                          `json:"kind"`
	Mode             string                          `json:"mode,omitempty"`
	Label            string                          `json:"label,omitempty"`
	Required         bool                            `json:"required"`
	Hint             string                          `json:"hint,omitempty"`
	Capabilities     []string                        `json:"capabilities,omitempty"`
	Compatible       []string                        `json:"compatible,omitempty"`
	IntegrationCands []preflightIntegrationCandidate `json:"integration_candidates,omitempty"`
	AppCands         []preflightAppCandidate         `json:"app_candidates,omitempty"`
	CanCreateNew     bool                            `json:"can_create_new"`
}

// POST /api/apps/installs/:id/upgrade — re-run the install at the
// upstream manifest's current version.
//
// Built-in apps: the new code already ships inside apteva-server, so
// "upgrade" just bumps app_installs.version to the bundled manifest's
// version — that clears the dashboard's "update available" badge.
//
// Source/git apps: re-fetch the upstream apteva.yaml, run the same
// BuildFromSource → spawn → swap sidecar pipeline as the original
// install. The cached binary lives at $cacheDir/<name>/<old-version>
// so the previous version stays on disk if the new build fails. The
// install row's bin path / port / version are flipped atomically by
// installFromSource on success.
//
// Manual installs (no source.repo / kind != source) can't be upgraded
// in-place; the handler returns 501 with a message asking the operator
// to uninstall + reinstall.
func (s *Server) handleUpgradeApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "upgrade" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body struct {
		ApproveNewPermissions bool `json:"approve_new_permissions"`
	}
	if r.Body != nil && r.Body != http.NoBody {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var (
		source, manifestJSON, availableManifestJSON, currentVersion, projectID, configEnc, permissionsJSON string
	)
	err = s.store.db.QueryRow(
		`SELECT COALESCE(NULLIF(i.source, ''), a.source),
		        COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json), a.manifest_json,
		        i.version, i.project_id, COALESCE(i.config_encrypted,''), COALESCE(i.permissions_json,'[]')
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&source, &manifestJSON, &availableManifestJSON, &currentVersion, &projectID, &configEnc, &permissionsJSON)
	if err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	var stored sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &stored); err != nil {
		http.Error(w, "manifest parse: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if info, ok := deprecatedApp(stored.Name); ok {
		// Existing Certs installs remain upgradeable during the native
		// certificate migration window. New installs stay blocked, and
		// once the legacy fallback is explicitly disabled this exception
		// disappears as well.
		if normalizeAppName(stored.Name) != "certs" || !legacyCertsFallbackEnabled(s) {
			writeJSONStatus(w, http.StatusGone, map[string]any{
				"error":       fmt.Sprintf("%s is deprecated and can no longer be upgraded", stored.Name),
				"app":         stored.Name,
				"deprecated":  true,
				"deprecation": info.Message,
				"replacement": info.Replacement,
			})
			return
		}
		log.Printf("[APPS] allowing Certs upgrade while legacy certificate fallback remains enabled")
	}
	approvedPermissions := parsePermissionListJSON(permissionsJSON)

	// Built-in: just bump the version — the running binary already
	// has whatever was bundled at server-build time.
	if source == "builtin" {
		var available sdk.Manifest
		if err := json.Unmarshal([]byte(availableManifestJSON), &available); err != nil {
			http.Error(w, "available manifest parse: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if available.Version == "" {
			http.Error(w, "no available version in manifest", http.StatusInternalServerError)
			return
		}
		if available.Version == currentVersion {
			writeJSON(w, map[string]string{"status": "up-to-date", "version": currentVersion})
			return
		}
		if missing := missingRequiredPermissions(approvedPermissions, available.Requires.Permissions); len(missing) > 0 {
			if !body.ApproveNewPermissions {
				writeMissingPermissionUpgradeConflict(w, available.DisplayName, available.Version, missing)
				return
			}
			approvedPermissions, err = s.approveAppInstallPermissions(installID, approvedPermissions, missing)
			if err != nil {
				http.Error(w, "approve permissions: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		requiredPermissionsJSON, _ := json.Marshal(available.Requires.Permissions)
		if _, err := s.store.db.Exec(
			`UPDATE app_installs SET version = ?, manifest_json = ?, permissions_json = ? WHERE id = ?`,
			available.Version, availableManifestJSON, string(requiredPermissionsJSON), installID,
		); err != nil {
			http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if changed, err := s.reconcileAppDepBindings(installID); err != nil {
			log.Printf("[APPS-BIND] built-in upgrade reconcile install=%d: %v", installID, err)
		} else if changed {
			s.recomputePendingOptions()
		}
		writeJSON(w, map[string]string{"status": "upgraded", "version": available.Version})
		return
	}

	// Source apps: re-fetch the upstream apteva.yaml so the install
	// gets the version the user actually wants, not the snapshot in
	// apps.manifest_json (which may itself be stale if the cache hasn't
	// rolled over).
	url := s.updateManifestURL(stored.Name, &stored)
	if url == "" {
		http.Error(w, "manifest has no github source — uninstall + reinstall at the desired ref", http.StatusNotImplemented)
		return
	}
	live, err := s.fetchAndCacheManifest(url)
	if err != nil || live == nil {
		http.Error(w, "fetch upstream manifest: "+errString(err), http.StatusBadGateway)
		return
	}
	if live.Version == "" {
		http.Error(w, "upstream manifest has no version", http.StatusBadGateway)
		return
	}
	if missing := missingRequiredPermissions(approvedPermissions, live.Requires.Permissions); len(missing) > 0 {
		if !body.ApproveNewPermissions {
			writeMissingPermissionUpgradeConflict(w, live.DisplayName, live.Version, missing)
			return
		}
		approvedPermissions, err = s.approveAppInstallPermissions(installID, approvedPermissions, missing)
		if err != nil {
			http.Error(w, "approve permissions: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Decrypt the config_encrypted blob so the rebuild gets the same
	// env that was passed at install time.
	var cfg map[string]string
	if configEnc != "" {
		if plain, derr := Decrypt(s.secret, configEnc); derr == nil {
			_ = json.Unmarshal([]byte(plain), &cfg)
		}
	}

	// Persist the new manifest immediately so the next list call
	// reflects the in-flight version even before the build completes.
	s.updateAppCatalogMetadataByName(live.Name, live)

	pendingManifestJSON, _ := json.Marshal(live)
	s.store.db.Exec(
		`UPDATE app_installs
		 SET status='pending', status_message='Upgrading…', error_message='', pending_manifest_json=?
		 WHERE id=?`,
		string(pendingManifestJSON), installID,
	)

	// installFromSource clones + builds + respawns + flips the install
	// row to running. Runs in a goroutine so the dashboard's POST
	// returns immediately — operators see the AppCard switch to the
	// pending state with live status_message ("Cloning…", "Building…")
	// driven by the existing pending-poll loop, instead of staring at
	// a frozen "Update → …" button for 10–60s while go build runs.
	//
	// Acquires the global build slot here (outermost goroutine) so
	// concurrent upgrade-all clicks queue cleanly instead of OOM-ing
	// the host with N parallel `go build` processes.
	s.store.db.Exec(`UPDATE app_installs SET status_message='Queued — waiting for a build slot' WHERE id=?`, installID)
	go func() {
		release := s.localApps.acquireBuildSlot()
		defer release()
		if err := s.installFromSource(installID, live, projectID, cfg); err != nil {
			// installFromSource already wrote status='error' + error_message.
			return
		}
		// The live manifest is the complete authority for app grants.
		// Dropped permissions must be revoked on upgrade; retaining them
		// leaves old capabilities available indefinitely even though the
		// app no longer declares them.
		requiredPermissionsJSON, _ := json.Marshal(live.Requires.Permissions)
		if _, err := s.store.db.Exec(
			`UPDATE app_installs SET permissions_json=? WHERE id=?`,
			string(requiredPermissionsJSON), installID,
		); err != nil {
			log.Printf("[APPS] prune obsolete permissions install=%d: %v", installID, err)
		}
		// The healthy sidecar and install snapshot now point at the new
		// version. Only now refresh app-owned skills; failed upgrades keep
		// both their old manifest and old skill surface.
		if len(live.Provides.Skills) > 0 {
			fetcher := s.makeSkillBodyFileFetcher(deriveManifestURL(live))
			if err := s.registerAppSkills(installID, live.Name, projectID, live.Provides.Skills, fetcher); err != nil {
				log.Printf("[APPS-SKILLS] upgrade refresh install=%d failed: %v", installID, err)
			} else {
				log.Printf("[APPS-SKILLS] refreshed %d skill(s) on upgrade install=%d", len(live.Provides.Skills), installID)
			}
		} else if err := s.deleteAppSkillsForInstall(installID, "app skill removed"); err != nil {
			log.Printf("[APPS-SKILLS] clear install=%d failed: %v", installID, err)
		}
		// Refresh the bridge row so a manifest that adds new tools
		// across versions surfaces them in mcp_servers.allowed_tools.
		// installFromSource already calls registerAppMCP on the success
		// path, but we call it again here to make the contract obvious
		// (upgrade => MCP refreshed) after the atomic running+version
		// install row update.
		_ = s.registerAppMCP(installID)
		// Reconcile the complete binding set against the successful
		// live manifest: prune removed roles/app deps and backfill
		// missing required app deps.
		if changed, err := s.reconcileAppDepBindings(installID); err != nil {
			log.Printf("[APPS-BIND] upgrade reconcile install=%d: %v", installID, err)
		} else if changed {
			s.recomputePendingOptions()
		}
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{
		"status":  "pending",
		"version": live.Version,
	})
}

func parsePermissionListJSON(raw string) []sdk.Permission {
	var out []sdk.Permission
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		return []sdk.Permission{}
	}
	return out
}

func missingRequiredPermissions(approved, required []sdk.Permission) []sdk.Permission {
	allowed := make(map[sdk.Permission]bool, len(approved))
	for _, p := range approved {
		allowed[p] = true
	}
	var missing []sdk.Permission
	for _, p := range required {
		if !allowed[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

func (s *Server) approveAppInstallPermissions(installID int64, approved, missing []sdk.Permission) ([]sdk.Permission, error) {
	merged := make([]sdk.Permission, 0, len(approved)+len(missing))
	seen := make(map[sdk.Permission]bool, len(approved)+len(missing))
	for _, p := range approved {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		merged = append(merged, p)
	}
	for _, p := range missing {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		merged = append(merged, p)
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json = ? WHERE id = ?`, string(body), installID); err != nil {
		return nil, err
	}
	return merged, nil
}

func writeMissingPermissionUpgradeConflict(w http.ResponseWriter, appName, version string, missing []sdk.Permission) {
	names := make([]string, 0, len(missing))
	for _, p := range missing {
		names = append(names, string(p))
	}
	if appName == "" {
		appName = "This app"
	}
	msg := fmt.Sprintf("%s requires new platform permission(s): %s. Approve the new permissions before upgrading.", appName, strings.Join(names, ", "))
	writeJSONStatus(w, http.StatusConflict, map[string]any{
		"error":               "new permissions required",
		"message":             msg,
		"version":             version,
		"missing_permissions": names,
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	// MCP-capable app attachment is now owned by the per-agent additive API.
	// Keeping this metadata-only replace-all path for those apps recreates the
	// exact split-brain failure where app_agent_bindings and core config
	// disagree. UI/worker-only apps still use this endpoint because they have
	// no MCP runtime configuration to synchronize.
	var mcpServerID int64
	_ = s.store.db.QueryRow(`SELECT id FROM mcp_servers WHERE upstream_id=?`,
		appMCPUpstreamID(installID)).Scan(&mcpServerID)
	if mcpServerID > 0 {
		writeJSONStatus(w, http.StatusGone, map[string]any{
			"error":         "replace-all MCP app bindings are no longer supported",
			"replacement":   "POST /api/agents/:id/mcp-servers",
			"mcp_server_id": mcpServerID,
			"actions":       []string{"add", "remove"},
		})
		return
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "begin: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM app_agent_bindings WHERE install_id = ?`, installID); err != nil {
		http.Error(w, "clear bindings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, iid := range body.InstanceIDs {
		if _, err := tx.Exec(
			`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`,
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

// makeSkillBodyFileFetcher returns a closure that resolves a Skill's
// body_file path against the install's manifest URL. body_file is
// resolved as a path RELATIVE to the manifest's directory — e.g. a
// manifest at https://raw.githubusercontent.com/apteva/apps/main/mcp/storage/apteva.yaml
// with body_file: skills/how-to-use-storage.md fetches
// https://raw.githubusercontent.com/apteva/apps/main/mcp/storage/skills/how-to-use-storage.md.
//
// When manifestURL is empty (inline / manual install), the closure
// errors out — apps using inline manifests must inline their skill
// bodies too. We could add a "local checkout" lookup later for the
// dev workflow but it's deliberately omitted from v1 to keep the
// fetch surface narrow + auditable.
func (s *Server) makeSkillBodyFileFetcher(manifestURL string) func(string) (string, error) {
	return func(bodyFile string) (string, error) {
		if manifestURL == "" {
			return "", fmt.Errorf("body_file requires a manifest_url install — inline manifests must use inline body")
		}
		// Strip the manifest filename, keep the directory.
		base := manifestURL
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[:i+1]
		}
		fullURL := base + strings.TrimPrefix(bodyFile, "/")
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(fullURL)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", fullURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("fetch %s: http %d", fullURL, resp.StatusCode)
		}
		const maxSkillBody = 256 * 1024 // 256 KiB ceiling — Anthropic recommends <500 lines
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillBody))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fullURL, err)
		}
		return string(raw), nil
	}
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
