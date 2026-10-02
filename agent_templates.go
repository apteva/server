package main

// agent_templates.go — pre-canned starter agent configs for the
// "build your first agent" wizard. Three sources share one table:
//
//   builtin  — derived from workspace presets and read-only through
//              the tenant API. New platform defaults ship with the server.
//   app      — contributed by an installed app via its manifest.
//              apps_loader upserts on install/upgrade.
//   user     — operator's own templates (save-from-agent or hand-rolled).
//
// The listing endpoint returns the union, filtered by user_template_hidden
// for the caller's hide list. Only caller-owned source=user rows are writable.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AgentTemplate is the wire shape returned by the templates endpoints.
// Highlights, recommended_apps, and requirements are stored as JSON in the
// DB and emitted as flat arrays so the dashboard does not have to parse them.
type AgentTemplate struct {
	ID         string `json:"id"`
	UserID     int64  `json:"user_id,omitempty"`
	Source     string `json:"source"` // "builtin" | "app" | "user"
	SourceRef  string `json:"source_ref,omitempty"`
	Category   string `json:"category,omitempty"`
	PresetName string `json:"preset_name,omitempty"`
	Name       string `json:"name"`
	// Icon is a short name (e.g. "user", "search", "code") that the
	// dashboard resolves to a stroked SVG component. Keeps the wire
	// payload tiny and the rendering consistent with the rest of
	// the platform's lucide-style icon set — no emojis, no per-app
	// PNG fetches at render time.
	Icon            string   `json:"icon,omitempty"`
	Description     string   `json:"description"`
	Highlights      []string `json:"highlights,omitempty"`
	Directive       string   `json:"directive"`
	Mode            string   `json:"mode"`
	Unconscious     bool     `json:"unconscious"`
	RecommendedApps []string `json:"recommended_apps"`
	// Requirements is the structured TODO list the wizard's Setup
	// step renders into install + bind + connect actions, and that
	// the future meta-agent reads as a checklist. Persisted as a
	// JSON array on the row.
	Requirements []Requirement `json:"requirements"`
	// ResolvedLogos is server-derived (not stored). The list endpoint
	// walks Requirements, looks up app marketplace icons + integration
	// catalog logos, and emits a flat array the dashboard renders
	// inside each template card.
	ResolvedLogos []TemplateLogo `json:"resolved_logos,omitempty"`
	SortOrder     int            `json:"sort_order"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// Requirement is one entry on a template's setup checklist. Four
// kinds:
//
//	kind=app          — sidecar app the agent uses. Wizard installs
//	                    via apps.install. Slug names the app
//	                    (storage, media, channel-email, ...).
//	kind=integration  — third-party SaaS credential (slack, github,
//	                    stripe). compatible_slugs lists any one of
//	                    which can satisfy the role; the wizard
//	                    picks the first that has a bound connection
//	                    or prompts to create one.
//	kind=channel      — agent-time delivery channel (email, slack,
//	                    telegram). Channel binding lives on the
//	                    agent, not on the project.
//	kind=skill        — markdown playbook the agent loads. Wizard
//	                    pushes via skills.push.
//
// Reason is operator-facing copy ("Used to send drafted replies").
// Required gates Setup advancement when true.
type Requirement struct {
	Kind            string         `json:"kind"`
	Slug            string         `json:"slug,omitempty"`
	Role            string         `json:"role,omitempty"`
	Type            string         `json:"type,omitempty"` // for kind=channel
	CompatibleSlugs []string       `json:"compatible_slugs,omitempty"`
	Capabilities    []string       `json:"capabilities,omitempty"`
	BindTo          *BindTo        `json:"bind_to,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	Required        bool           `json:"required"`
	Source          string         `json:"source,omitempty"` // builtin | app:<slug> | url for skills
	Config          map[string]any `json:"config,omitempty"` // pre-fill for app install
}

// BindTo names the app + role on the app that this integration
// requirement should bind to once both pieces (app install +
// connection) exist. Apps loader resolves this into an
// app_agent_bindings row.
type BindTo struct {
	App  string `json:"app"`
	Role string `json:"role"`
}

// TemplateLogo is one icon resolved from a Requirement. Emitted in
// AgentTemplate.ResolvedLogos for the wizard's card render.
//
//	kind = "app" | "integration" | "channel"
//	source = "direct" (declared on the template) | "derived" (pulled
//	         from a required app's requires.integrations)
//	via = app slug that pulled in a derived entry, empty otherwise
type TemplateLogo struct {
	Kind      string `json:"kind"`
	Slug      string `json:"slug"`
	IconURL   string `json:"icon_url,omitempty"`
	IconStyle string `json:"icon_style,omitempty"`
	Label     string `json:"label"`
	Source    string `json:"source"`
	Via       string `json:"via,omitempty"`
}

// Builtin agent templates are projections of the shipped workspace presets.
// Keep roles, instructions, behavior and app requirements in one catalog.
func presetAgentTemplates() ([]AgentTemplate, error) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		return nil, err
	}
	out := []AgentTemplate{{
		ID: "empty", Source: "builtin", Name: "Start from scratch", Icon: "robot",
		Description: "Define your own role, instructions, and tools.", Mode: "learn",
		RecommendedApps: []string{}, Requirements: []Requirement{},
	}}
	for _, preset := range catalog.Presets {
		for _, agent := range preset.Agents {
			// The workspace description is supplied during preset setup, but a single
			// agent can be created globally. Do not leak template placeholders into it.
			role := strings.TrimPrefix(agent.Directive, "Use this project description as your operating context: {{description}}. ")
			description, _, _ := strings.Cut(role, ". ")
			description = strings.TrimSuffix(description, ".") + "."
			directive := expandPresetTemplate(role, nil)
			directive += "\n\nUse the user's instructions and available project context to guide your work. Other roles mentioned above may not exist in this workspace. Collaborate only with agents that are actually available; otherwise handle work within your capabilities or explain what is missing. Never claim to have delegated work to an unavailable agent."
			requirements := make([]Requirement, 0, len(agent.Apps))
			for _, slug := range agent.Apps {
				requirements = append(requirements, Requirement{Kind: "app", Slug: slug, Required: true, Reason: "Used by the " + agent.Name + " role."})
			}
			out = append(out, AgentTemplate{
				ID: "preset:" + preset.ID + ":" + agent.Key, Source: "builtin", SourceRef: preset.ID,
				Category: preset.Category, PresetName: preset.Name,
				Name: agent.Name, Icon: agent.Icon, Description: description, Directive: directive,
				Mode: agent.Mode, Unconscious: agent.Unconscious, RecommendedApps: agent.Apps,
				Requirements: requirements, SortOrder: len(out),
			})
		}
	}
	return out, nil
}

// Refresh only platform-owned templates. Legacy rows remain addressable for
// compatibility, but are no longer offered by the picker. Existing agents and
// user/app templates are unaffected.
func seedBuiltinTemplates(db *sql.DB) error {
	templates, err := presetAgentTemplates()
	if err != nil {
		return err
	}
	for _, t := range templates {
		appsJSON, _ := json.Marshal(t.RecommendedApps)
		reqJSON, _ := json.Marshal(t.Requirements)
		_, err := db.Exec(`
   INSERT INTO agent_templates
    (id, user_id, source, source_ref, name, icon, description, directive, mode,
     unconscious, recommended_apps, requirements, sort_order)
   VALUES (?, NULL, 'builtin', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
   ON CONFLICT(id) DO UPDATE SET source_ref=excluded.source_ref,
    name=excluded.name, icon=excluded.icon, description=excluded.description,
    directive=excluded.directive, mode=excluded.mode, unconscious=excluded.unconscious,
    recommended_apps=excluded.recommended_apps, requirements=excluded.requirements,
    sort_order=excluded.sort_order
   WHERE agent_templates.source='builtin'`,
			t.ID, t.SourceRef, t.Name, t.Icon, t.Description, t.Directive, t.Mode,
			boolToInt(t.Unconscious), string(appsJSON), string(reqJSON), t.SortOrder)
		if err != nil {
			return fmt.Errorf("seed agent template %s: %w", t.ID, err)
		}
	}
	return nil
}

// ListAgentTemplates returns every template visible to the user:
// builtin + app + the user's own. Per-user-hidden entries are filtered
// out. Sorted by (sort_order, name) so the wizard renders in a
// deterministic order.
func (s *Store) ListAgentTemplates(userID int64) ([]AgentTemplate, error) {
	builtins, err := presetAgentTemplates()
	if err != nil {
		return nil, err
	}
	current := make(map[string]bool, len(builtins))
	for _, t := range builtins {
		current[t.ID] = true
	}
	rows, err := s.db.Query(`
		SELECT id, COALESCE(user_id, 0), source, source_ref, name, icon,
		       description, highlights, directive, mode, unconscious, recommended_apps,
		       requirements, sort_order, created_at, updated_at
		  FROM agent_templates t
		 WHERE (t.user_id IS NULL OR t.user_id = ?)
		   AND t.id NOT IN (
		       SELECT template_id FROM user_template_hidden WHERE user_id = ?
		   )
		 ORDER BY sort_order ASC, name ASC`,
		userID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentTemplate
	for rows.Next() {
		t, err := scanAgentTemplate(rows)
		if err != nil {
			return nil, err
		}
		if t.Source == "builtin" && !current[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAgentTemplate returns one template by id, scoped to what the
// caller is allowed to see (builtin/app are global, user-owned must
// match userID).
func (s *Store) GetAgentTemplate(userID int64, id string) (*AgentTemplate, error) {
	row := s.db.QueryRow(`
		SELECT id, COALESCE(user_id, 0), source, source_ref, name, icon,
		       description, highlights, directive, mode, unconscious, recommended_apps,
		       requirements, sort_order, created_at, updated_at
		  FROM agent_templates
		 WHERE id = ?
		   AND (user_id IS NULL OR user_id = ?)`,
		id, userID,
	)
	t, err := scanAgentTemplate(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, err
	}
	return &t, nil
}

// CreateAgentTemplate inserts a new user-owned template. id is
// generated from the name (lowercase, hyphenated) prefixed with the
// user id so cross-user collisions can't happen.
func (s *Store) CreateAgentTemplate(userID int64, t AgentTemplate) (*AgentTemplate, error) {
	if t.ID == "" {
		t.ID = userTemplateID(userID, t.Name)
	}
	appsJSON, _ := json.Marshal(t.RecommendedApps)
	highlightsJSON, _ := json.Marshal(t.Highlights)
	reqJSON, _ := json.Marshal(t.Requirements)
	if t.Requirements == nil {
		reqJSON = []byte("[]")
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_templates
			(id, user_id, source, source_ref, name, icon, description,
			 highlights, directive, mode, unconscious, recommended_apps, requirements,
			 sort_order)
		VALUES (?, ?, 'user', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, userID, t.Name, t.Icon, t.Description, string(highlightsJSON), t.Directive,
		t.Mode, boolToInt(t.Unconscious), string(appsJSON), string(reqJSON),
		t.SortOrder,
	)
	if err != nil {
		return nil, err
	}
	return s.GetAgentTemplate(userID, t.ID)
}

// UpdateAgentTemplate edits only a caller-owned template. Builtin and app
// templates are shared catalog entries and must never be mutated by a tenant.
func (s *Store) UpdateAgentTemplate(userID int64, id string, t AgentTemplate) error {
	appsJSON, _ := json.Marshal(t.RecommendedApps)
	highlightsJSON, _ := json.Marshal(t.Highlights)
	reqJSON, _ := json.Marshal(t.Requirements)
	if t.Requirements == nil {
		reqJSON = []byte("[]")
	}
	result, err := s.db.Exec(`
		UPDATE agent_templates
		   SET name = ?, icon = ?, description = ?, highlights = ?, directive = ?,
		       mode = ?, unconscious = ?, recommended_apps = ?,
		       requirements = ?, sort_order = ?,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?
		   AND user_id = ?`,
		t.Name, t.Icon, t.Description, string(highlightsJSON), t.Directive, t.Mode,
		boolToInt(t.Unconscious), string(appsJSON), string(reqJSON),
		t.SortOrder, id, userID,
	)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteAgentTemplate removes a user-owned row. For builtin/app rows,
// the caller should use HideAgentTemplate instead — those rows stay
// in the table so other users / fresh logins still see them.
func (s *Store) DeleteAgentTemplate(userID int64, id string) error {
	result, err := s.db.Exec(
		`DELETE FROM agent_templates WHERE id = ? AND user_id = ?`,
		id, userID,
	)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// HideAgentTemplate adds the template to the user's hide list. No-op
// if it's already there.
func (s *Store) HideAgentTemplate(userID int64, id string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO user_template_hidden (user_id, template_id) VALUES (?, ?)`,
		userID, id,
	)
	return err
}

// UnhideAgentTemplate removes the template from the user's hide list.
func (s *Store) UnhideAgentTemplate(userID int64, id string) error {
	_, err := s.db.Exec(
		`DELETE FROM user_template_hidden WHERE user_id = ? AND template_id = ?`,
		userID, id,
	)
	return err
}

// userTemplateID builds a stable id for user-owned templates. Format:
// usr-<userID>:<slug>. Conflict probability stays at zero for the same
// user (the dashboard nudges them to pick a different name on dupe)
// and is namespaced away from builtin/app id collisions by design.
func userTemplateID(userID int64, name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == ' ' || r == '_' || r == '-':
			return '-'
		}
		return -1
	}, slug)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "template"
	}
	return "usr-" + i64s(userID) + ":" + slug
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func i64s(v int64) string {
	// avoid pulling in strconv here just for one call
	return itoa64(v)
}

// scanAgentTemplate handles both *sql.Row and *sql.Rows via the
// shared rowScanner interface from skills_handlers.go (same package).
func scanAgentTemplate(r rowScanner) (AgentTemplate, error) {
	var (
		t              AgentTemplate
		uid            int64
		unc            int
		appsJSON       string
		highlightsJSON string
		reqJSON        string
		createdAt      string
		updatedAt      string
	)
	if err := r.Scan(
		&t.ID, &uid, &t.Source, &t.SourceRef, &t.Name, &t.Icon,
		&t.Description, &highlightsJSON, &t.Directive, &t.Mode, &unc, &appsJSON,
		&reqJSON, &t.SortOrder, &createdAt, &updatedAt,
	); err != nil {
		return t, err
	}
	if uid != 0 {
		t.UserID = uid
	}
	t.Unconscious = unc == 1
	if appsJSON != "" {
		_ = json.Unmarshal([]byte(appsJSON), &t.RecommendedApps)
	}
	if t.RecommendedApps == nil {
		t.RecommendedApps = []string{}
	}
	if highlightsJSON != "" {
		_ = json.Unmarshal([]byte(highlightsJSON), &t.Highlights)
	}
	if t.Highlights == nil {
		t.Highlights = []string{}
	}
	if reqJSON != "" {
		_ = json.Unmarshal([]byte(reqJSON), &t.Requirements)
	}
	if t.Requirements == nil {
		t.Requirements = []Requirement{}
	}
	if t.Source == "builtin" && strings.HasPrefix(t.ID, "preset:") {
		if catalog, err := loadProjectPresetCatalog(); err == nil {
			if preset, ok := catalog.ByID[t.SourceRef]; ok {
				t.Category = preset.Category
				t.PresetName = preset.Name
			}
		}
	}
	t.CreatedAt, _ = parseTime(createdAt)
	t.UpdatedAt, _ = parseTime(updatedAt)
	return t, nil
}

// resolveTemplateLogos walks a template's Requirements and resolves
// each one to a TemplateLogo the wizard's card can render. The
// resolver is server-side so the dashboard never has to fetch the
// catalog/marketplace itself to render a card.
//
// Direct logos come from the Requirement itself: kind=integration uses
// the integrations catalog (s.catalog.Get(slug).Logo + .Name);
// kind=app uses the curated marketplace registry's Icon for the slug.
//
// Derived logos: for each kind=app requirement, peek at its manifest
// (via the cache) and pull its requires.integrations through the same
// catalog lookup. The card can then show "this template uses storage,
// which itself needs SMTP" without the template author having to
// enumerate transitive deps. Marked source="derived" + via=<app slug>
// so the dashboard can render them distinctly.
//
// Channel kind maps to the integration that backs it (email→smtp,
// slack→slack, telegram→telegram) so the wizard renders a familiar
// brand logo rather than a generic "channel" pictogram.
func (s *Server) resolveTemplateLogos(t *AgentTemplate) {
	seen := map[string]bool{} // dedupe by kind+slug
	out := []TemplateLogo{}
	add := func(l TemplateLogo) {
		key := l.Kind + ":" + l.Slug
		if seen[key] || l.Slug == "" {
			return
		}
		seen[key] = true
		out = append(out, l)
	}
	channelToIntegration := map[string]string{
		"email":    "smtp",
		"slack":    "slack",
		"telegram": "telegram",
	}
	var registry *CuratedRegistry
	registryFor := func() *CuratedRegistry {
		if registry != nil {
			return registry
		}
		r, err := s.fetchAndCacheRegistry()
		if err != nil {
			return nil
		}
		registry = r
		return r
	}
	resolveIntegration := func(slug, source, via string) {
		if slug == "" {
			return
		}
		entry := s.catalog.Get(slug)
		if entry == nil {
			return
		}
		var icon string
		if entry.Logo != nil {
			icon = *entry.Logo
		}
		add(TemplateLogo{
			Kind:    "integration",
			Slug:    slug,
			IconURL: icon,
			Label:   entry.Name,
			Source:  source,
			Via:     via,
		})
	}
	resolveApp := func(slug, source string) {
		if slug == "" {
			return
		}
		reg := registryFor()
		if reg == nil {
			add(TemplateLogo{Kind: "app", Slug: slug, Label: slug, Source: source})
			return
		}
		want := normalizeAppName(slug)
		for _, e := range reg.Apps {
			if normalizeAppName(e.Name) == want {
				label := e.DisplayName
				if label == "" {
					label = e.Name
				}
				// Match the Apps marketplace: the manifest owns the current icon
				// and its rendering style; the registry is only a fallback.
				icon, iconStyle := e.Icon, e.IconStyle
				if e.ManifestURL != "" {
					if manifest, err := s.fetchAndCacheManifest(e.ManifestURL); err == nil && manifest != nil {
						if resolved := resolveMarketplaceAppIcon(e.ManifestURL, manifest.Icon); resolved != "" {
							icon = resolved
						}
						if manifest.IconStyle != "" {
							iconStyle = manifest.IconStyle
						}
					}
				}
				add(TemplateLogo{Kind: "app", Slug: slug, IconURL: icon, IconStyle: iconStyle, Label: label, Source: source})
				return
			}
		}
		add(TemplateLogo{Kind: "app", Slug: slug, Label: slug, Source: source})
	}
	// manifestURLFor looks up the registry entry for an app slug and
	// returns its manifest_url, or "" if the registry doesn't know
	// the app (offline / typo'd slug).
	manifestURLFor := func(slug string) string {
		reg := registryFor()
		if reg == nil {
			return ""
		}
		want := normalizeAppName(slug)
		for _, e := range reg.Apps {
			if normalizeAppName(e.Name) == want {
				return e.ManifestURL
			}
		}
		return ""
	}
	for _, req := range t.Requirements {
		switch req.Kind {
		case "integration":
			slug := req.Slug
			if slug == "" && len(req.CompatibleSlugs) > 0 {
				slug = req.CompatibleSlugs[0]
			}
			resolveIntegration(slug, "direct", "")
		case "app":
			resolveApp(req.Slug, "direct")
		case "channel":
			if mapped, ok := channelToIntegration[req.Type]; ok {
				resolveIntegration(mapped, "direct", "")
			}
		}
	}

	// Derived pass: for each required app, peek at its manifest and
	// surface the integration slugs the app itself needs (e.g. the
	// messaging app needs aws-ses, twilio). Marked source="derived"
	// + via=<app slug> so the dashboard renders them at lower opacity
	// after a divider. Failures (network, parse, unknown slug) are
	// silent — the card still has its direct logos.
	for _, req := range t.Requirements {
		if req.Kind != "app" || !req.Required || req.Slug == "" {
			continue
		}
		url := manifestURLFor(req.Slug)
		if url == "" {
			continue
		}
		manifest, err := s.fetchAndCacheManifest(url)
		if err != nil || manifest == nil {
			continue
		}
		for _, dep := range manifest.Requires.Integrations {
			// kind on IntegrationDep defaults to "integration".
			kind := dep.Kind
			if kind == "" {
				kind = "integration"
			}
			if kind != "integration" || !dep.Required {
				continue
			}
			depSlug := ""
			if len(dep.CompatibleSlugs) > 0 {
				depSlug = dep.CompatibleSlugs[0]
			}
			resolveIntegration(depSlug, "derived", req.Slug)
		}
	}
	t.ResolvedLogos = out
}

// ─── HTTP handlers ─────────────────────────────────────────────────

func validateWritableAgentTemplate(t AgentTemplate) error {
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 120 {
		return errors.New("name is required and must be at most 120 characters")
	}
	if strings.TrimSpace(t.Directive) == "" || len(t.Directive) > 32000 {
		return errors.New("directive is required and must be at most 32000 characters")
	}
	if !validAgentMode(t.Mode) {
		return errors.New("mode must be autonomous, cautious, or learn")
	}
	if len(t.Highlights) > 6 {
		return errors.New("template may contain at most 6 highlights")
	}
	for _, highlight := range t.Highlights {
		if strings.TrimSpace(highlight) == "" || len(highlight) > 200 {
			return errors.New("template contains an invalid highlight")
		}
	}
	return nil
}

// GET /agent-templates
func (s *Server) handleListAgentTemplates(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	list, err := s.store.ListAgentTemplates(userID)
	if err != nil {
		http.Error(w, "list templates: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []AgentTemplate{}
	}
	for i := range list {
		s.resolveTemplateLogos(&list[i])
	}
	writeJSON(w, list)
}

// POST /agent-templates — body shape mirrors AgentTemplate (no id
// expected; the store assigns one from user_id + name).
func (s *Server) handleCreateAgentTemplate(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	var body AgentTemplate
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Mode == "" {
		body.Mode = "learn"
	}
	if err := validateWritableAgentTemplate(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := s.store.CreateAgentTemplate(userID, body)
	if err != nil {
		http.Error(w, "create template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if t != nil {
		s.resolveTemplateLogos(t)
	}
	writeJSON(w, t)
}

// GET /agent-templates/:id   — handleAgentTemplateByID dispatches by method.
func (s *Server) handleAgentTemplateByID(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/agent-templates/")
	// Allow /agent-templates/:id/hide for the per-user hide endpoint.
	if strings.HasSuffix(path, "/hide") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(path, "/hide")
		if err := s.store.HideAgentTemplate(userID, id); err != nil {
			http.Error(w, "hide: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "hidden"})
		return
	}
	if strings.HasSuffix(path, "/unhide") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(path, "/unhide")
		if err := s.store.UnhideAgentTemplate(userID, id); err != nil {
			http.Error(w, "unhide: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "unhidden"})
		return
	}
	id := path
	switch r.Method {
	case http.MethodGet:
		t, err := s.store.GetAgentTemplate(userID, id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.resolveTemplateLogos(t)
		writeJSON(w, t)
	case http.MethodPut:
		current, err := s.store.GetAgentTemplate(userID, id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if current.Source != "user" || current.UserID != userID {
			http.Error(w, "builtin and app templates are read-only", http.StatusForbidden)
			return
		}
		var body AgentTemplate
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.Mode == "" {
			body.Mode = "learn"
		}
		if err := validateWritableAgentTemplate(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateAgentTemplate(userID, id, body); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
			return
		}
		t, _ := s.store.GetAgentTemplate(userID, id)
		if t != nil {
			s.resolveTemplateLogos(t)
		}
		writeJSON(w, t)
	case http.MethodDelete:
		if err := s.store.DeleteAgentTemplate(userID, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}
