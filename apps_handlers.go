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
)

// AppRow — what /api/apps returns for the dashboard's Installed view.
type AppRow struct {
	InstallID     int64            `json:"install_id"`
	AppID         int64            `json:"app_id"`
	Name          string           `json:"name"`
	DisplayName   string           `json:"display_name"`
	Version       string           `json:"version"`
	Description   string           `json:"description"`
	Icon          string           `json:"icon"`
	ProjectID     string           `json:"project_id"`
	Status        string           `json:"status"`
	Source        string           `json:"source"`
	UpgradePolicy string           `json:"upgrade_policy"`
	Permissions   []sdk.Permission `json:"permissions"`
	Surfaces      AppSurfaces      `json:"surfaces"`
	UIPanels      []sdk.UIPanel    `json:"ui_panels,omitempty"`
}

// AppSurfaces — flattened booleans the dashboard uses to render the
// row's icons (has tools? has UI? etc.) without re-parsing the manifest.
type AppSurfaces struct {
	MCPTools        bool `json:"mcp_tools"`
	HTTPRoutes      bool `json:"http_routes"`
	UIPanel         bool `json:"ui_panel"`
	UIPage          bool `json:"ui_page"`
	UIApp           bool `json:"ui_app"`
	Channels        bool `json:"channels"`
	Workers         bool `json:"workers"`
	PromptFragments bool `json:"prompt_fragments"`
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
	installed := map[string]bool{}
	addInstalled := func(name string) {
		if name == "" {
			return
		}
		installed[normalizeAppName(name)] = true
	}
	if rows, err := s.store.db.Query(`SELECT name FROM apps`); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				addInstalled(n)
			}
		}
		rows.Close()
	}
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			addInstalled(m.Slug)
			addInstalled(m.Name)
		}
	}
	type entryWithStatus struct {
		RegistryEntry
		Installed bool `json:"installed"`
		Builtin   bool `json:"builtin"`
	}
	builtin := map[string]bool{}
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			builtin[normalizeAppName(m.Slug)] = true
			builtin[normalizeAppName(m.Name)] = true
		}
	}
	out := make([]entryWithStatus, 0, len(reg.Apps))
	for _, e := range reg.Apps {
		key := normalizeAppName(e.Name)
		out = append(out, entryWithStatus{
			RegistryEntry: e,
			Installed:     installed[key],
			Builtin:       builtin[key],
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

// GET /api/apps[?project_id=X]
//
// Returns one row per install visible to the caller — project installs
// for the requested project plus all globals. Built-in apps appear with
// source='builtin'.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	q := `
		SELECT i.id, i.app_id, i.project_id, i.status, i.upgrade_policy,
			i.version, i.permissions_json, a.name, a.source, a.manifest_json
		FROM app_installs i JOIN apps a ON a.id = i.app_id`
	args := []any{}
	if projectID != "" {
		q += ` WHERE i.project_id = '' OR i.project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY a.name`
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []AppRow{}
	for rows.Next() {
		var (
			installID, appID                                                int64
			projID, status, upgradePolicy, version, permsJSON               string
			name, source, manifestJSON                                      string
		)
		if err := rows.Scan(&installID, &appID, &projID, &status, &upgradePolicy,
			&version, &permsJSON, &name, &source, &manifestJSON); err != nil {
			continue
		}
		var manifest sdk.Manifest
		_ = json.Unmarshal([]byte(manifestJSON), &manifest)
		var perms []sdk.Permission
		_ = json.Unmarshal([]byte(permsJSON), &perms)
		out = append(out, AppRow{
			InstallID: installID, AppID: appID, Name: name, DisplayName: manifest.DisplayName,
			Version: version, Description: manifest.Description, Icon: manifest.Icon,
			ProjectID: projID, Status: status, Source: source, UpgradePolicy: upgradePolicy,
			Permissions: perms, Surfaces: surfacesFromManifest(&manifest),
			UIPanels: manifest.Provides.UIPanels,
		})
	}
	writeJSON(w, out)
}

func surfacesFromManifest(m *sdk.Manifest) AppSurfaces {
	return AppSurfaces{
		MCPTools:        len(m.Provides.MCPTools) > 0,
		HTTPRoutes:      len(m.Provides.HTTPRoutes) > 0,
		UIPanel:         len(m.Provides.UIPanels) > 0,
		UIPage:          len(m.Provides.UIPages) > 0,
		UIApp:           m.Provides.UIApp != nil,
		Channels:        len(m.Provides.Channels) > 0,
		Workers:         len(m.Provides.Workers) > 0,
		PromptFragments: len(m.Provides.PromptFragments) > 0,
	}
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
	// declares — source (clone+build, works on any host with Go),
	// then per-platform binaries, then fall back. Failures flip the
	// install row to 'error' with the message stored.
	preferLocal := os.Getenv("APTEVA_APPS_REMOTE") == "" // default: local mode
	if preferLocal {
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
	// Detach in-memory mount first so further proxy calls 404 immediately.
	s.installedApps.Remove(installID)
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
