package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// --- App catalog types (matches apteva JSON definitions) ---

type AppTemplate struct {
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Logo        *string            `json:"logo"`
	Categories  []string           `json:"categories"`
	BaseURL     string             `json:"base_url"`
	Auth        AppAuthConfig      `json:"auth"`
	Tools       []AppToolDef       `json:"tools"`
	Webhooks    *AppWebhookConfig  `json:"webhooks,omitempty"`
	// Suite membership + per-scope credential variants. Opt-in; legacy
	// apps leave both nil. See types.ts for the canonical description.
	CredentialGroup *CredentialGroup `json:"credential_group,omitempty"`
	Scopes          *AppScopes       `json:"scopes,omitempty"`
}

// --- Credential groups (suites) ---

type CredentialGroup struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Logo        *string               `json:"logo,omitempty"`
	Description string                `json:"description,omitempty"`
	Discovery   *GroupDiscoveryConfig `json:"discovery,omitempty"`
}

type GroupDiscoveryConfig struct {
	ListProjects DiscoveryCall `json:"list_projects"`
}

type DiscoveryCall struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	BaseURL      string `json:"base_url,omitempty"`
	ResponsePath string `json:"response_path,omitempty"`
	IDField      string `json:"id_field"`
	LabelField   string `json:"label_field"`
}

type AppScopes struct {
	Account *AppScope `json:"account,omitempty"`
	Project *AppScope `json:"project,omitempty"`
}

type AppScope struct {
	CredentialFields []CredentialField `json:"credential_fields"`
	AuthHeaders      map[string]string `json:"auth_headers,omitempty"`
	AuthQuery        map[string]string `json:"auth_query,omitempty"`
	ProjectBinding   *ProjectBinding   `json:"project_binding,omitempty"`
}

type ProjectBinding struct {
	Type  string `json:"type"`  // "header" | "path_prefix" | "path_param"
	Name  string `json:"name"`
	Value string `json:"value"`
}

type AppWebhookConfig struct {
	SignatureHeader string            `json:"signature_header"`
	Events          []AppWebhookEvent `json:"events"`
	Registration    *WebhookRegConfig `json:"registration,omitempty"`
}

type AppWebhookEvent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type WebhookRegConfig struct {
	Method      string                 `json:"method"`
	Path        string                 `json:"path"`
	URLField    string                 `json:"url_field"`
	EventsField string                 `json:"events_field,omitempty"`
	SecretField string                 `json:"secret_field,omitempty"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
	IDField     string                 `json:"id_field,omitempty"`
	DeletePath   string                 `json:"delete_path,omitempty"`
	DeleteMethod string                 `json:"delete_method,omitempty"`
	ManualSetup  string                 `json:"manual_setup,omitempty"`
}

type AppAuthConfig struct {
	Types           []string            `json:"types"`
	Headers         map[string]string   `json:"headers,omitempty"`
	QueryParams     map[string]string   `json:"query_params,omitempty"`
	CredentialFields []CredentialField  `json:"credential_fields,omitempty"`
	OAuth2          *OAuthConfig        `json:"oauth2,omitempty"`
}

type CredentialField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Required    *bool  `json:"required,omitempty"`
	Type        string `json:"type,omitempty"` // "password" or "text"
}

type OAuthConfig struct {
	AuthorizeURL     string   `json:"authorize_url"`
	TokenURL         string   `json:"token_url"`
	Scopes           []string `json:"scopes"`
	ClientIDRequired bool     `json:"client_id_required"`
	PKCE             bool     `json:"pkce"`
	// Extra static query parameters merged into the authorize URL after
	// the standard ones (response_type, client_id, redirect_uri, scope,
	// state, code_challenge). Required by some providers to actually
	// hand out a refresh_token. Examples:
	//   Google: { "access_type": "offline", "prompt": "consent",
	//             "include_granted_scopes": "true" }
	//   Microsoft: { "prompt": "consent" }
	// Without access_type=offline + prompt=consent on Google, the FIRST
	// authorization yields both access + refresh tokens but every
	// SUBSEQUENT one (after revocation/re-link) yields only an access
	// token — you only ever get the refresh_token on the very first
	// consent, and Google skips the consent screen by default for
	// already-authorized apps.
	ExtraAuthorizeParams map[string]string `json:"extra_authorize_params,omitempty"`
}

type AppToolDef struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	InputSchema  map[string]any `json:"input_schema"`
	// Names of input fields that should be sent as URL query string
	// parameters instead of being folded into the request body. Required
	// for APIs that mix query+body on POST/PUT/PATCH (e.g. Google Sheets'
	// values:append puts valueInputOption in the URL but the ValueRange
	// object in the body). Without this, executeIntegrationTool sends
	// every non-path field as body content and the API rejects the
	// request — google-sheets.write_range / append_rows were broken
	// before this field existed on the Go side. Mirrors the same field
	// in @apteva/integrations/src/types.ts AppToolTemplate.
	QueryParams []string `json:"query_params,omitempty"`
	ResponsePath *string `json:"response_path,omitempty"`

	// ResponseOmit declares JSON paths in the tool's response that
	// should be stripped before the agent sees them. Use `.` to
	// descend into objects and `[]` to step into every element of an
	// array. Intended for upstream APIs that return huge redundant
	// metadata (per-word timestamps, full re-serialisations of the
	// same text, model info, etc) that would otherwise blow the
	// agent's context window. Unmatched paths are silent no-ops.
	//
	// Examples:
	//   "metadata.model_info"
	//   "results.channels[].alternatives[].words"
	//   "results.utterances"
	ResponseOmit []string `json:"response_omit,omitempty"`
}

// AppSummary is a lightweight version for catalog listing
type AppSummary struct {
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Logo           *string  `json:"logo"`
	Categories     []string `json:"categories"`
	AuthTypes      []string `json:"auth_types"`
	ToolCount      int      `json:"tool_count"`
	HasWebhooks    bool              `json:"has_webhooks"`
	WebhookEvents  []AppWebhookEvent `json:"webhook_events,omitempty"`
}

// --- App Catalog ---

type AppCatalog struct {
	mu     sync.RWMutex
	apps   map[string]*AppTemplate
	// Aggregated by credential_group.id → metadata + member app slugs.
	// Rebuilt on every LoadFromDir/Register call. Empty when no app in
	// the catalog declares `credential_group`.
	groups map[string]*catalogGroup
}

type catalogGroup struct {
	Meta    CredentialGroup
	Members []string // app slugs participating in the group
}

// GroupSummary is the catalog-surface view of a suite (for the UI).
type GroupSummary struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Logo        *string             `json:"logo,omitempty"`
	Description string              `json:"description,omitempty"`
	Members     []GroupMemberSummary `json:"members"`
	HasAccountScope bool            `json:"has_account_scope"`
	HasProjectScope bool            `json:"has_project_scope"`
}

type GroupMemberSummary struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	ToolCount int    `json:"tool_count"`
	Logo      *string `json:"logo,omitempty"`
}

func NewAppCatalog() *AppCatalog {
	return &AppCatalog{apps: make(map[string]*AppTemplate), groups: make(map[string]*catalogGroup)}
}

func (c *AppCatalog) LoadFromDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Full reload: wipe before rebuilding so stale entries from a
	// previously-loaded dir don't survive a swap.
	c.apps = make(map[string]*AppTemplate)
	c.groups = make(map[string]*catalogGroup)

	for _, f := range files {
		// Skip index.ts etc
		if !strings.HasSuffix(f, ".json") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var app AppTemplate
		if err := json.Unmarshal(data, &app); err != nil {
			continue
		}
		if app.Slug == "" {
			continue
		}
		c.apps[app.Slug] = &app

		// Aggregate into the group map when this app opts into a suite.
		// First app declaring the group seeds its metadata (name, logo,
		// description, discovery). Subsequent members just append.
		if app.CredentialGroup != nil && app.CredentialGroup.ID != "" {
			gid := app.CredentialGroup.ID
			g, ok := c.groups[gid]
			if !ok {
				g = &catalogGroup{Meta: *app.CredentialGroup}
				// Inherit discovery from the first member that
				// declares it (templates usually duplicate it — we
				// only need it once).
				c.groups[gid] = g
			}
			if g.Meta.Discovery == nil && app.CredentialGroup.Discovery != nil {
				g.Meta.Discovery = app.CredentialGroup.Discovery
			}
			g.Members = append(g.Members, app.Slug)
		}
	}

	return nil
}

func (c *AppCatalog) Register(app *AppTemplate) {
	c.mu.Lock()
	c.apps[app.Slug] = app
	if app.CredentialGroup != nil && app.CredentialGroup.ID != "" {
		gid := app.CredentialGroup.ID
		g, ok := c.groups[gid]
		if !ok {
			g = &catalogGroup{Meta: *app.CredentialGroup}
			c.groups[gid] = g
		}
		if g.Meta.Discovery == nil && app.CredentialGroup.Discovery != nil {
			g.Meta.Discovery = app.CredentialGroup.Discovery
		}
		g.Members = append(g.Members, app.Slug)
	}
	c.mu.Unlock()
}

// GetGroup returns the metadata + member list for a credential group,
// or nil if no app in the catalog participates in it.
func (c *AppCatalog) GetGroup(id string) *catalogGroup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.groups[id]
}

// ListGroups returns a stable-sorted summary of every credential group
// represented in the catalog.
func (c *AppCatalog) ListGroups() []GroupSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]GroupSummary, 0, len(c.groups))
	for _, g := range c.groups {
		s := GroupSummary{
			ID:          g.Meta.ID,
			Name:        g.Meta.Name,
			Logo:        g.Meta.Logo,
			Description: g.Meta.Description,
		}
		for _, slug := range g.Members {
			app := c.apps[slug]
			if app == nil {
				continue
			}
			s.Members = append(s.Members, GroupMemberSummary{
				Slug:      app.Slug,
				Name:      app.Name,
				ToolCount: len(app.Tools),
				Logo:      app.Logo,
			})
			if app.Scopes != nil {
				if app.Scopes.Account != nil {
					s.HasAccountScope = true
				}
				if app.Scopes.Project != nil {
					s.HasProjectScope = true
				}
			}
		}
		sort.Slice(s.Members, func(i, j int) bool { return s.Members[i].Name < s.Members[j].Name })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// appInGroup returns true when `slug` is a member of any credential
// group — used to hide members from the flat catalog (the group card
// is shown instead). Read lock must not be held by the caller.
func (c *AppCatalog) appInGroup(slug string) bool {
	for _, g := range c.groups {
		for _, m := range g.Members {
			if m == slug {
				return true
			}
		}
	}
	return false
}

func (c *AppCatalog) Get(slug string) *AppTemplate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apps[slug]
}

func (c *AppCatalog) List() []AppSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var summaries []AppSummary
	for _, app := range c.apps {
		s := AppSummary{
			Slug:        app.Slug,
			Name:        app.Name,
			Description: app.Description,
			Logo:        app.Logo,
			Categories:  app.Categories,
			AuthTypes:   app.Auth.Types,
			ToolCount:   len(app.Tools),
		}
		// Webhook capability comes from the app's webhooks config
		if app.Webhooks != nil && len(app.Webhooks.Events) > 0 {
			s.HasWebhooks = true
			s.WebhookEvents = app.Webhooks.Events
		}
		summaries = append(summaries, s)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

// ListUngrouped returns catalog entries that are NOT members of any
// credential group. For the dashboard: grouped apps surface as their
// suite card instead.
func (c *AppCatalog) ListUngrouped() []AppSummary {
	all := c.List()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]AppSummary, 0, len(all))
	for _, s := range all {
		inGroup := false
		for _, g := range c.groups {
			for _, m := range g.Members {
				if m == s.Slug {
					inGroup = true
					break
				}
			}
			if inGroup {
				break
			}
		}
		if !inGroup {
			out = append(out, s)
		}
	}
	return out
}

func (c *AppCatalog) Search(query string) []AppSummary {
	q := strings.ToLower(query)
	all := c.List()
	if q == "" {
		return all
	}
	var results []AppSummary
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.Slug), q) {
			results = append(results, s)
		}
		if len(results) == 0 {
			for _, cat := range s.Categories {
				if strings.Contains(strings.ToLower(cat), q) {
					results = append(results, s)
					break
				}
			}
		}
	}
	return results
}

func (c *AppCatalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.apps)
}

// --- HTTP Handlers ---

// GET /integrations/catalog?q=search
//
// When `group=1` is passed, omits apps that are members of a
// credential group so the dashboard can render one card per suite
// (the group details come from /integrations/groups). When omitted
// the legacy flat list is returned, for backwards compatibility with
// clients that don't know about groups yet.
func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	group := r.URL.Query().Get("group")
	var apps []AppSummary
	if q != "" {
		apps = s.catalog.Search(q)
	} else if group == "1" || group == "true" {
		apps = s.catalog.ListUngrouped()
	} else {
		apps = s.catalog.List()
	}
	if apps == nil {
		apps = []AppSummary{}
	}
	writeJSON(w, apps)
}

// GET /integrations/groups
// Returns every credential group represented in the catalog plus
// their member apps. The dashboard uses this to render suite cards.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.catalog.ListGroups())
}

// GET /integrations/groups/:id
func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/integrations/groups/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.Error(w, "group id required", http.StatusBadRequest)
		return
	}
	g := s.catalog.GetGroup(id)
	if g == nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	// Return the summary shape the dashboard expects + the discovery
	// config + the two account/project scope credential_fields lists
	// (pick from any member — they're validated identical at load).
	sum := GroupSummary{
		ID: g.Meta.ID, Name: g.Meta.Name, Logo: g.Meta.Logo,
		Description: g.Meta.Description,
	}
	var accountScope, projectScope *AppScope
	for _, slug := range g.Members {
		app := s.catalog.Get(slug)
		if app == nil {
			continue
		}
		sum.Members = append(sum.Members, GroupMemberSummary{
			Slug: app.Slug, Name: app.Name, ToolCount: len(app.Tools), Logo: app.Logo,
		})
		if app.Scopes != nil {
			if accountScope == nil && app.Scopes.Account != nil {
				accountScope = app.Scopes.Account
				sum.HasAccountScope = true
			}
			if projectScope == nil && app.Scopes.Project != nil {
				projectScope = app.Scopes.Project
				sum.HasProjectScope = true
			}
		}
	}
	sort.Slice(sum.Members, func(i, j int) bool { return sum.Members[i].Name < sum.Members[j].Name })

	writeJSON(w, map[string]any{
		"summary":       sum,
		"discovery":     g.Meta.Discovery,
		"account_scope": accountScope,
		"project_scope": projectScope,
	})
}

// GET /integrations/catalog/:slug
func (s *Server) handleGetCatalogApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	slug := strings.TrimPrefix(r.URL.Path, "/integrations/catalog/")
	app := s.catalog.Get(slug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	writeJSON(w, app)
}

// --- Template resolution for HTTP execution ---

// credAliases groups credential field names that should all resolve to
// the same value. Within a group, the first non-empty value found in the
// credentials map is mirrored under every other name in the group, so a
// template using {{token}} works whether the credential blob has
// `token`, `access_token`, `accessToken`, `bearer_token`, etc.
//
// This is what fixes 48 templates that ship with cred field names like
// `accessToken`, `apiToken`, `authToken`, `apikey`, etc. and headers
// using `{{token}}` or `{{api_key}}`. Without normalization the literal
// substitution would leave the placeholder unresolved → 401 on every
// outbound call.
var credAliases = [][]string{
	// Bearer access tokens — the canonical "API access" credential.
	// Anything in this group becomes available under all other names.
	{"access_token", "accessToken", "token", "bearer_token", "auth_token", "authToken"},
	// API keys — same idea, plus the camelCase variants we keep seeing.
	{"api_key", "apiKey", "apikey", "api_token", "apiToken", "x_api_key"},
	// Refresh tokens
	{"refresh_token", "refreshToken"},
	// Token metadata
	{"token_type", "tokenType"},
	{"expires_in", "expiresIn"},
	// OAuth client identity (mostly internal but template authors sometimes
	// reference these in custom auth flows)
	{"client_id", "clientId"},
	{"client_secret", "clientSecret"},
}

// normalizeCredentials returns a copy of the credentials map with every
// alias group filled in from the first non-empty member found. Order
// inside each group is significant — earlier names are preferred as the
// canonical value source.
func normalizeCredentials(c map[string]string) map[string]string {
	out := make(map[string]string, len(c)*2)
	for k, v := range c {
		out[k] = v
	}
	for _, group := range credAliases {
		var val string
		for _, name := range group {
			if v, ok := out[name]; ok && v != "" {
				val = v
				break
			}
		}
		if val == "" {
			continue
		}
		for _, name := range group {
			if existing, ok := out[name]; !ok || existing == "" {
				out[name] = val
			}
		}
	}
	return out
}

func resolveTemplate(template string, credentials map[string]string) string {
	norm := normalizeCredentials(credentials)
	result := template
	for key, val := range norm {
		result = strings.ReplaceAll(result, "{{"+key+"}}", val)
	}
	return result
}

func buildURL(baseURL, path string, input map[string]any) string {
	resolved := path
	for key, val := range input {
		placeholder := "{" + key + "}"
		if strings.Contains(resolved, placeholder) {
			resolved = strings.ReplaceAll(resolved, placeholder, fmt.Sprintf("%v", val))
		}
	}
	return baseURL + resolved
}

func buildAuthQuery(queryParams map[string]string, credentials map[string]string) string {
	if len(queryParams) == 0 {
		return ""
	}
	var parts []string
	for key, tmpl := range queryParams {
		val := resolveTemplate(tmpl, credentials)
		if val != "" {
			parts = append(parts, key+"="+val)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

func buildHeaders(authHeaders map[string]string, credentials map[string]string) map[string]string {
	headers := map[string]string{}
	for key, tmpl := range authHeaders {
		headers[key] = resolveTemplate(tmpl, credentials)
	}
	return headers
}
