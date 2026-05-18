package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
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
	// Integration kind. Empty / "rest" = legacy default (tools defined
	// in `Tools` and proxied via the per-connection generated stdio
	// MCP). "remote_mcp" = vendor hosts the MCP server; we proxy tool
	// calls through to the URL declared in `MCP`. Tools is allowed to
	// be empty for remote_mcp templates because the upstream's
	// tools/list response is the source of truth.
	Kind string           `json:"kind,omitempty"`
	MCP  *RemoteMcpConfig `json:"mcp,omitempty"`
	// UIComponents — optional React cards the agent can attach to chat
	// replies via respond(components=[…]). Same shape as apps'
	// provides.ui_components. Each entry's Entry path resolves under
	// /api/integrations/<slug>/<entry> at runtime.
	UIComponents []IntegrationUIComponent `json:"ui_components,omitempty"`
	// HealthCheck — cheap probe used by `apteva connection test` /
	// the dashboard's "Test" button. Optional. When set, the runner
	// builds a synthetic AppToolDef from these fields and reuses
	// executeIntegrationTool so credential templating, AWS-SigV4
	// signing, header building, and base_url resolution all behave
	// identically to a real tool call. The probe should be
	// read-only and zero-arg (e.g. /me, /whoami, S3 list_buckets,
	// Slack auth.test). Apps without a health_check fall back to
	// "no validation possible" — the test endpoint replies with
	// `skipped: true`. See integrations/src/types.ts AppHealthCheck.
	HealthCheck *AppHealthCheck `json:"health_check,omitempty"`
}

// AppHealthCheck is the catalog-side shape of a per-app probe.
// Pre-v0.13 connections were never validated; an operator could
// type a wrong API key, click save, and only learn it was bogus
// when an agent failed three days later. The probe runs the same
// pipeline as a real tool call so a failing health check predicts
// a failing tool call (modulo upstream rate limits).
//
// Two forms — pick whichever fits the app:
//
//   1. Reference an existing tool by name. The runner looks the
//      tool up in app.Tools[] and reuses its method, path,
//      query_params, etc. DRY: a fix to the tool propagates to
//      the probe automatically. Use this when there's a natural
//      read-only zero-arg tool (S3 list_buckets, GitHub /user,
//      Slack auth.test).
//
//        "health_check": { "tool": "list_buckets" }
//
//      Pass `input` if the tool needs parameters:
//        "health_check": { "tool": "me", "input": { "scope": "read" } }
//
//   2. Synthetic HTTP request. Use when the probe URL doesn't
//      correspond to any exposed tool (a dedicated /healthz, a
//      different host, etc.):
//
//        "health_check": { "method": "GET", "path": "/healthz" }
//
// ExpectStatus defaults to [200] when empty. BaseURL overrides
// app.BaseURL for this single probe — useful when the auth-
// introspection endpoint lives on a different host than the
// data-plane (e.g. AWS STS sts.amazonaws.com vs. the per-service
// S3 host).
type AppHealthCheck struct {
	// Form 1 fields — reference an existing tool.
	Tool  string         `json:"tool,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// Form 2 fields — synthetic HTTP request.
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	BaseURL string `json:"base_url,omitempty"`

	// Expected upstream status. Empty = [200]. Applies to both forms.
	ExpectStatus []int `json:"expect_status,omitempty"`

	// AuthOKWhenBodyContains lets a probe treat a 4xx response as
	// "auth valid, permission denied" rather than "auth invalid",
	// so long as the response body contains one of the listed
	// substrings. Designed for S3-compatible APIs where
	// bucket-scoped tokens (the production-best-practice setup)
	// cannot ListBuckets at account level — R2 returns
	// `403 <Code>AccessDenied</Code>` for valid bucket-only tokens
	// while still returning `403 <Code>InvalidAccessKeyId</Code>`
	// for typo'd ones. Listing "AccessDenied" here lets us pass
	// the former and fail the latter from the same probe.
	//
	// Semantics: if status ∈ expect_status → OK. Else if status is
	// 401 or 403 AND body contains any listed substring → OK.
	// Otherwise fail. Empty list = strict (only expect_status).
	AuthOKWhenBodyContains []string `json:"auth_ok_when_body_contains,omitempty"`
}

// IntegrationUIComponent mirrors @apteva/integrations/src/types.ts
// UIComponent. We keep the Go side as map[string]any for the schema
// because we never inspect the schema server-side beyond rendering it
// in the agent-facing description — the dashboard's runtime is the
// authority for prop validation.
type IntegrationUIComponent struct {
	Name        string         `json:"name"`
	Entry       string         `json:"entry"`
	Slots       []string       `json:"slots,omitempty"`
	PropsSchema map[string]any `json:"props_schema,omitempty"`
	PreviewProps map[string]any `json:"preview_props,omitempty"`
}

// RemoteMcpConfig describes how to reach a vendor-hosted MCP server.
// Mirrors @apteva/integrations/src/types.ts RemoteMcpConfig.
type RemoteMcpConfig struct {
	Transport  string             `json:"transport"` // "http" | "sse"
	URL        string             `json:"url"`
	AuthHeader *McpAuthHeaderTmpl `json:"auth_header,omitempty"`
}

type McpAuthHeaderTmpl struct {
	Name  string `json:"name"`
	Value string `json:"value"`
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
	// ApiLookup — optional, run once before ProvisionProjectKey to
	// resolve a fact about the upstream account that the provision
	// body needs (e.g. the OmniKit API UUID matching base_path=omnikit).
	// Result is cached on the master under credKeyAPILookupID.
	ApiLookup *LookupCall `json:"api_lookup,omitempty"`
	// ProvisionProjectKey — when set, the suite-enable flow uses the
	// master credentials to MINT a project-scoped key and stores it on
	// the child connection. Without this, children fall back to the
	// classic master+project_binding share. See server/suites.go:
	// provisionProjectKey for the runtime.
	ProvisionProjectKey *ProvisionKeyCall `json:"provision_project_key,omitempty"`
}

type DiscoveryCall struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	BaseURL      string `json:"base_url,omitempty"`
	ResponsePath string `json:"response_path,omitempty"`
	IDField      string `json:"id_field"`
	LabelField   string `json:"label_field"`
}

// LookupCall — like DiscoveryCall but resolves to a single id rather
// than a list. Walks ResponsePath to an array, finds the first entry
// where MatchField == MatchValue, returns IDField. Used for upstream
// "find me the api/workspace/account record matching X" queries that
// have to run before ProvisionProjectKey can build its body.
type LookupCall struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	BaseURL      string `json:"base_url,omitempty"`
	ResponsePath string `json:"response_path,omitempty"`
	MatchField   string `json:"match_field,omitempty"`
	MatchValue   string `json:"match_value,omitempty"`
	IDField      string `json:"id_field"`
}

// ProvisionKeyCall — declarative spec for "mint a project-scoped key
// using the master's credentials". Body values may include {{label}},
// {{external_project_id}}, and {{api_lookup_id}} placeholders; the
// server templates them at request time. ResponseKeyPath points at
// the minted plaintext key (returned once); IDResponsePath points at
// the upstream id of the new key so we can revoke it on disconnect.
type ProvisionKeyCall struct {
	Method          string         `json:"method"`
	Path            string         `json:"path"`
	BaseURL         string         `json:"base_url,omitempty"`
	Body            map[string]any `json:"body,omitempty"`
	ResponseKeyPath string         `json:"response_key_path"`
	IDResponsePath  string         `json:"id_response_path,omitempty"`
	// CredentialField — the connection-credential key under which the
	// minted plaintext key is stored on the child (e.g. "api_key" so
	// the existing AuthHeaders template `X-API-Key: {{api_key}}` keeps
	// working without further wiring).
	CredentialField string `json:"credential_field"`
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
	// AwsSigV4 configures AWS Signature V4 request signing (SES, S3,
	// anything else that lives behind aws_sigv4). Service is required
	// when types includes "aws_sigv4" — region comes from the
	// connection's credentials. Credentials must include
	// access_key_id + secret_access_key + region (and optionally
	// session_token for STS-issued temporary creds).
	//
	// Legacy field: kept for backwards compatibility with existing
	// catalog entries. New entries should declare auth.signers[] with
	// {name: "aws_sigv4", params: {service: "ses"}} instead. The
	// integration runner translates the legacy form via
	// effectiveSigners().
	AwsSigV4 *AwsSigV4Config `json:"aws_sigv4,omitempty"`
	// Signers — request-signer chain applied to every outbound call
	// for this integration unless overridden per-tool. Each entry
	// references a name in the server's signer registry (signer.go).
	// Order matters: body-mutating signers (EIP-712) must run before
	// header-only signers (HMAC/AWS) that sign over the final body.
	Signers []SignerSpec `json:"signers,omitempty"`
}

type AwsSigV4Config struct {
	Service string `json:"service"`
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
	// ClientIDParamName overrides the parameter name carrying the client
	// id on both the authorize-URL build and the token-exchange POST
	// body. Defaults to "client_id" (the OAuth 2.0 standard). TikTok is
	// the notable outlier here — it demands "client_key" instead, and
	// rejects the standard name with errCode=10003 / error_type=client_key.
	// Set per-integration in the catalog when an upstream diverges from
	// the spec; leave empty for the 99% case.
	ClientIDParamName string `json:"client_id_param_name,omitempty"`
	// ScopeSeparator overrides the character used to join the scopes
	// list in the authorize URL's `scope` query param. Defaults to a
	// single space (the OAuth 2.0 standard, RFC 6749 §3.3). TikTok
	// diverges and demands a comma; their docs are explicit ("A comma
	// (,) separated string of authorization scope(s)"). Set this to
	// "," in the catalog when an upstream rejects space-joined scopes
	// with invalid_scope / error_type=scope.
	ScopeSeparator string `json:"scope_separator,omitempty"`
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

	// TimeoutMS overrides the default 30s HTTP client timeout for this
	// tool's upstream call. Set on tools that legitimately take longer
	// than 30s on the wire — image generation (gpt-image-2 medium ~30–60s,
	// hd ~90s+), video generation, transcription of long audio, etc.
	// Capped at 600s server-side to keep a single tool call from holding
	// a connection open indefinitely. Zero / unset → 30s default.
	TimeoutMS int `json:"timeout_ms,omitempty"`

	// BodyInput names a single input field whose value carries the raw
	// request body. Required for endpoints that take binary content
	// (S3 PutObject, Cloudinary upload, R2 PutObject, etc.) — without
	// this the runner JSON-marshals every input into a single body map
	// and the upstream rejects the request.
	//
	// When set, the named field's value is sent as the request body
	// verbatim (string → bytes; []byte passed through; numbers/bool
	// stringified). Other inputs follow the normal rules: path-template
	// substitution for path fields, query string for tool-declared
	// query_params, request headers via header_input (future). Any
	// remaining inputs are silently dropped — they have nowhere to go
	// once the body slot is taken by binary content.
	//
	// The integration JSON should also declare a Content-Type header
	// (often application/octet-stream) since the runner's "default
	// json" content-type assumption is bypassed.
	BodyInput string `json:"body_input,omitempty"`

	// Signing overrides the app-level auth.signers[] chain for this
	// specific tool. Use when most endpoints share one auth flavor but
	// a handful need extra work — e.g. Polymarket's CLOB reads with
	// just L2 HMAC vs create_order which additionally needs EIP-712
	// typed-data signing of the order body.
	//
	// Set Signing.Signers to an empty slice (not nil) to suppress all
	// signing for this tool — useful for public endpoints inside an
	// otherwise-authenticated app catalog entry.
	Signing *ToolSigningConfig `json:"signing,omitempty"`
}

// ToolSigningConfig — per-tool override of the app-level signer chain.
// When present, replaces auth.signers[] for this tool (does not merge).
type ToolSigningConfig struct {
	Signers []SignerSpec `json:"signers"`
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
	// Kind = "rest" (default) or "remote_mcp". Surfaced so the catalog
	// UI can render a "hosted MCP" badge alongside a REST entry with
	// the same brand (e.g. `hubspot` and `hubspot-mcp`). Empty for
	// legacy entries — UI should treat empty as "rest".
	Kind string `json:"kind,omitempty"`
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

// LoadFromFS is the fs.FS-shaped sibling of LoadFromDir, used by
// the boot path to read the embedded integrations-catalog/ tree
// out of the apteva-server binary. Same semantics as LoadFromDir
// (full reload, populate apps + groups maps), just sourced from
// an io/fs.FS so we can swap in embed.FS without rewriting the
// loader. Returns the count of apps loaded so the caller can
// decide whether to fall through to the disk / GitHub paths.
func (c *AppCatalog) LoadFromFS(fsys fs.FS) (int, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.apps = make(map[string]*AppTemplate)
	c.groups = make(map[string]*catalogGroup)

	count := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
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
		count++
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
	}
	return count, nil
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
			Kind:        app.Kind,
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
	// Derived credentials. basic_auth is base64(username:password) for
	// HTTP Basic auth — Twilio uses (account_sid, auth_token); generic
	// Basic users put (username, password) in their connection.
	// Computed once here so auth.headers can declare a clean
	// "Basic {{basic_auth}}" template without runner-internal hooks.
	if out["basic_auth"] == "" {
		user, pass := basicAuthPair(out)
		if user != "" && pass != "" {
			out["basic_auth"] = base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		}
	}
	return out
}

// basicAuthPair picks the (username, password) pair for HTTP Basic
// auth from a credential map. Recognises the conventional pairings
// used by the catalog today; integrations that need a different
// pairing can declare them in their credential field names.
func basicAuthPair(c map[string]string) (user, pass string) {
	pairs := [][2]string{
		{"username", "password"},
		{"account_sid", "auth_token"},
		{"api_key", "api_secret"},
	}
	for _, p := range pairs {
		u := c[p[0]]
		v := c[p[1]]
		if u != "" && v != "" {
			return u, v
		}
	}
	return "", ""
}

func resolveTemplate(template string, credentials map[string]string) string {
	norm := normalizeCredentials(credentials)
	result := template
	for key, val := range norm {
		result = strings.ReplaceAll(result, "{{"+key+"}}", val)
	}
	return result
}

// formEncode marshals body fields as application/x-www-form-urlencoded.
// Used when an integration declares Content-Type:
// application/x-www-form-urlencoded in auth.headers (Twilio, Stripe,
// etc.). Arrays of primitives become repeated key=value pairs, which
// is what Twilio expects for things like Tags / MediaUrl.
func formEncode(body map[string]any) string {
	values := url.Values{}
	for k, v := range body {
		switch x := v.(type) {
		case nil:
			// skip
		case []any:
			for _, item := range x {
				values.Add(k, fmt.Sprintf("%v", item))
			}
		default:
			values.Set(k, fmt.Sprintf("%v", v))
		}
	}
	return values.Encode()
}

func buildURL(baseURL, path string, input map[string]any) string {
	resolved := path
	for key, val := range input {
		placeholder := "{" + key + "}"
		if strings.Contains(resolved, placeholder) {
			resolved = strings.ReplaceAll(resolved, placeholder, fmt.Sprintf("%v", val))
		}
	}
	// If the resolved path is itself absolute, treat it as the full URL
	// and skip baseURL concatenation. Used by tools whose endpoint lives
	// on a different host than the integration's primary base_url —
	// YouTube's resumable-upload init at googleapis.com/upload/youtube/v3
	// (vs the read API at googleapis.com/youtube/v3), Pinecone's per-
	// index data plane at {index_host}, etc. Detected post-substitution
	// so {{credential.host}}-style hostname injection still works.
	if strings.HasPrefix(resolved, "http://") || strings.HasPrefix(resolved, "https://") {
		return resolved
	}
	return baseURL + resolved
}

// buildAuthQuery returns the auth credentials encoded as "k=v&k=v" with no
// leading separator. The caller is responsible for joining with "?" or "&"
// depending on whether the URL already has a query string — Namecheap and
// any other integration whose tool.path embeds "?Command=..." would otherwise
// produce a URL with two "?" separators, which standard parsers split on "&"
// and so swallow the first auth field into the preceding param's value.
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
	return strings.Join(parts, "&")
}

func buildHeaders(authHeaders map[string]string, credentials map[string]string) map[string]string {
	headers := map[string]string{}
	for key, tmpl := range authHeaders {
		headers[key] = resolveTemplate(tmpl, credentials)
	}
	return headers
}
