package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ComposioClient is a thin REST wrapper for the Composio v3 API. It owns one
// HTTPS session authenticated with a user's COMPOSIO_API_KEY (sent in the
// `x-api-key` header) and exposes just the endpoints we need for the unified
// connections + mcp_servers flow.
//
// Endpoint paths confirmed against https://backend.composio.dev/api/v3/openapi.json.
// Drift in the upstream API only requires edits in this file — nothing else in
// apteva-server knows about Composio's URL structure.
type ComposioClient struct {
	BaseURL string
	APIKey  string
	http    *http.Client
}

func NewComposioClient(apiKey string) *ComposioClient {
	return &ComposioClient{
		BaseURL: "https://backend.composio.dev",
		APIKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// --- Typed shapes (minimal — only fields we consume) ---

// ComposioApp is an adapter shape the dashboard consumes. The upstream field
// in the v3 toolkits listing is actually `items[].slug / .name`, with no
// description/logo/categories. We fill the ones we have and leave the rest
// empty.
type ComposioApp struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Logo        string   `json:"logo,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	// NoAuth tells the UI it can connect without any OAuth popup.
	NoAuth bool `json:"no_auth,omitempty"`
	// ComposioManaged is true when Composio can run its own OAuth app for this
	// toolkit (no custom credentials required on the apteva side).
	ComposioManaged bool `json:"composio_managed,omitempty"`
}

// Raw shape of a toolkit item in GET /api/v3/toolkits
type composioToolkitItem struct {
	Slug                       string   `json:"slug"`
	Name                       string   `json:"name"`
	AuthSchemes                []string `json:"auth_schemes"`
	ComposioManagedAuthSchemes []string `json:"composio_managed_auth_schemes"`
	NoAuth                     bool     `json:"no_auth"`
	AuthGuideURL               string   `json:"auth_guide_url"`
	// note: the v3 spec marks is_local_toolkit deprecated; we ignore it
}

type composioAuthConfig struct {
	ID                string `json:"id"`
	AuthScheme        string `json:"auth_scheme"`
	IsComposioManaged bool   `json:"is_composio_managed"`
}

// ComposioCredField is one field a user must (or may) enter to create a
// connection to a toolkit. Surfaces the upstream shape with stable JSON tags
// that the dashboard consumes.
type ComposioCredField struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"` // "string", "password", "number", ...
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// ComposioToolkitDetails is the subset of GET /toolkits/{slug} we actually
// need for the dashboard's connect form. Composio supports several auth
// modes per toolkit; we surface the first mode that's either composio-managed
// or matches a small allow-list (api_key, basic, oauth2) so we can render a
// useful form.
type ComposioToolkitDetails struct {
	Slug                       string              `json:"slug"`
	Name                       string              `json:"name"`
	ComposioManagedAuthSchemes []string            `json:"composio_managed_auth_schemes"`
	AuthMode                   string              `json:"auth_mode"`              // lowercased
	AuthModeDisplay            string              `json:"auth_mode_display"`      // human readable
	AuthGuideURL               string              `json:"auth_guide_url,omitempty"`
	ConfigFields               []ComposioCredField `json:"config_fields"`          // for auth_configs create
	InitFields                 []ComposioCredField `json:"init_fields"`            // for per-connection init
	IsComposioManaged          bool                `json:"is_composio_managed"`
}

// Raw shape of auth_config_details[i] in GET /toolkits/{slug}
type composioToolkitDetailRaw struct {
	Slug                       string   `json:"slug"`
	Name                       string   `json:"name"`
	ComposioManagedAuthSchemes []string `json:"composio_managed_auth_schemes"`
	AuthGuideURL               string   `json:"auth_guide_url"`
	AuthConfigDetails          []struct {
		Mode   string `json:"mode"`
		Name   string `json:"name"`
		Fields struct {
			AuthConfigCreation         composioFieldGroup `json:"auth_config_creation"`
			ConnectedAccountInitiation composioFieldGroup `json:"connected_account_initiation"`
		} `json:"fields"`
	} `json:"auth_config_details"`
}

type composioFieldGroup struct {
	Required []composioRawField `json:"required"`
	Optional []composioRawField `json:"optional"`
}

type composioRawField struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Default     string `json:"default"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// ComposioConnectedAccount mirrors the server-side fields we read from the
// GET /api/v3/connected_accounts/{nanoid} response. The POST /link response
// does not return a status — it just returns redirect_url + connected_account_id.
type ComposioConnectedAccount struct {
	ID           string `json:"id"`
	Status       string `json:"status"` // ACTIVE | INITIATED | FAILED | EXPIRED
	AuthConfigID string `json:"-"`      // set by our code, not read from API
}

// ComposioMCPServer is what CreateMCPServer returns us. The upstream endpoint
// is POST /api/v3/mcp/servers/custom and returns {id, name, mcp_url, ...}.
type ComposioMCPServer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"mcp_url"`
}

// --- Core helper ---

func (c *ComposioClient) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	// v3 auth: project API key goes in x-api-key (lowercase header name is
	// case-insensitive but we match the spec literal).
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4_000_000))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Composio responds with JSON error objects; include raw for debugging
		// but strip obvious HTML 404 pages.
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("composio %s %s: http %d: %s", method, path, resp.StatusCode, snippet)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w (body=%s)", path, err, string(raw))
	}
	return nil
}

// --- API methods ---

// ListApps returns Composio's toolkit catalog (GET /api/v3/toolkits). It
// paginates with cursors until the upstream returns no next_cursor, so the
// dashboard gets the complete catalog in one call. Pass a non-empty search
// string to use Composio's server-side name/slug search instead of paging
// through everything.
func (c *ComposioClient) ListApps(search string) ([]ComposioApp, error) {
	const pageSize = 1000 // upstream hard cap
	var out []ComposioApp
	cursor := ""
	for page := 0; page < 20; page++ { // safety cap — 20k toolkits max
		q := fmt.Sprintf("limit=%d&sort_by=alphabetically", pageSize)
		if cursor != "" {
			q += "&cursor=" + urlQueryEscape(cursor)
		}
		if search != "" {
			q += "&search=" + urlQueryEscape(search)
		}
		var wrapper struct {
			Items      []composioToolkitItem `json:"items"`
			NextCursor string                `json:"next_cursor"`
		}
		if err := c.do("GET", "/api/v3/toolkits?"+q, nil, &wrapper); err != nil {
			return nil, err
		}
		for _, t := range wrapper.Items {
			out = append(out, ComposioApp{
				Slug:            t.Slug,
				Name:            t.Name,
				NoAuth:          t.NoAuth,
				ComposioManaged: len(t.ComposioManagedAuthSchemes) > 0,
			})
		}
		if wrapper.NextCursor == "" || len(wrapper.Items) == 0 {
			break
		}
		cursor = wrapper.NextCursor
		// For search queries the first page is enough; most callers want
		// quick disambiguation, not a deep scan.
		if search != "" {
			break
		}
	}
	return out, nil
}

// GetToolkitDetails fetches full toolkit metadata including the credential
// field schema the dashboard needs to render a connect form for non-managed
// auth modes (API key, basic, custom OAuth).
//
// We pick the first auth mode from the following priority order:
//  1. A composio-managed scheme (oauth2 with Composio's own app) — no fields
//     needed, one-click flow.
//  2. Any api_key / basic_auth mode.
//  3. The first entry in auth_config_details.
//
// Upstream shape uses camelCase inside `fields`; we normalise to snake_case
// for the dashboard.
func (c *ComposioClient) GetToolkitDetails(slug string) (*ComposioToolkitDetails, error) {
	var raw composioToolkitDetailRaw
	if err := c.do("GET", "/api/v3/toolkits/"+slug, nil, &raw); err != nil {
		return nil, err
	}

	out := &ComposioToolkitDetails{
		Slug:                       raw.Slug,
		Name:                       raw.Name,
		ComposioManagedAuthSchemes: raw.ComposioManagedAuthSchemes,
		AuthGuideURL:               raw.AuthGuideURL,
		IsComposioManaged:          len(raw.ComposioManagedAuthSchemes) > 0,
	}

	// Pick best available auth mode
	managed := map[string]bool{}
	for _, m := range raw.ComposioManagedAuthSchemes {
		managed[strings.ToUpper(m)] = true
	}
	var chosen *struct {
		Mode   string `json:"mode"`
		Name   string `json:"name"`
		Fields struct {
			AuthConfigCreation         composioFieldGroup `json:"auth_config_creation"`
			ConnectedAccountInitiation composioFieldGroup `json:"connected_account_initiation"`
		} `json:"fields"`
	}
	// First pass: look for a managed mode
	for i := range raw.AuthConfigDetails {
		d := &raw.AuthConfigDetails[i]
		if managed[strings.ToUpper(d.Mode)] {
			chosen = d
			break
		}
	}
	// Second pass: api_key / basic / anything
	if chosen == nil {
		for i := range raw.AuthConfigDetails {
			d := &raw.AuthConfigDetails[i]
			m := strings.ToUpper(d.Mode)
			if m == "API_KEY" || m == "BASIC" || m == "BEARER_TOKEN" {
				chosen = d
				break
			}
		}
	}
	if chosen == nil && len(raw.AuthConfigDetails) > 0 {
		chosen = &raw.AuthConfigDetails[0]
	}
	if chosen != nil {
		out.AuthMode = strings.ToLower(chosen.Mode)
		out.AuthModeDisplay = chosen.Name
		out.ConfigFields = normalizeFields(chosen.Fields.AuthConfigCreation)
		out.InitFields = normalizeFields(chosen.Fields.ConnectedAccountInitiation)
	}
	return out, nil
}

func normalizeFields(g composioFieldGroup) []ComposioCredField {
	// Always return a non-nil slice so JSON encodes to [] not null — the
	// dashboard calls .length on these fields.
	all := []ComposioCredField{}
	for _, f := range g.Required {
		all = append(all, ComposioCredField{
			Name: f.Name, DisplayName: f.DisplayName, Description: f.Description,
			Type: f.Type, Required: true, Default: f.Default,
		})
	}
	for _, f := range g.Optional {
		all = append(all, ComposioCredField{
			Name: f.Name, DisplayName: f.DisplayName, Description: f.Description,
			Type: f.Type, Required: false, Default: f.Default,
		})
	}
	return all
}

// ensureAuthConfig returns an auth_config id for the given toolkit slug.
//
// Behaviour:
//   - If a matching auth config already exists for the toolkit, return it.
//   - Otherwise, if the toolkit supports composio-managed auth AND no custom
//     credentials are provided, create one with type=use_composio_managed_auth.
//   - Otherwise, create one with type=use_custom_auth carrying the supplied
//     credentials map (API key, basic auth creds, OAuth client id/secret, ...).
//
// authMode is uppercase per Composio's enum (OAUTH2, API_KEY, BASIC, ...).
// Pass empty creds for the managed path.
func (c *ComposioClient) ensureAuthConfig(toolkitSlug, authMode string, creds map[string]string) (*composioAuthConfig, error) {
	// The discriminator is the caller's explicit authMode: when empty we try
	// the composio-managed path; when set we go custom (even if creds is
	// empty — for toolkits like Pushover where credentials live on the
	// connection init step, not the auth config).
	useCustom := authMode != ""

	// Look up existing configs for this toolkit.
	var listResp struct {
		Items []struct {
			ID                string `json:"id"`
			AuthScheme        string `json:"auth_scheme"`
			IsComposioManaged bool   `json:"is_composio_managed"`
			Toolkit           struct {
				Slug string `json:"slug"`
			} `json:"toolkit"`
		} `json:"items"`
	}
	path := "/api/v3/auth_configs?toolkit_slug=" + urlQueryEscape(toolkitSlug) + "&limit=50"
	if err := c.do("GET", path, nil, &listResp); err == nil {
		if !useCustom {
			// Managed path: reuse an existing managed config if any.
			for _, it := range listResp.Items {
				if it.IsComposioManaged {
					return &composioAuthConfig{ID: it.ID, AuthScheme: it.AuthScheme, IsComposioManaged: true}, nil
				}
			}
		} else {
			// Custom path: reuse any existing non-managed config for the same
			// auth scheme. Composio will accept the same credentials on the
			// per-connection init step even if the config was created earlier.
			for _, it := range listResp.Items {
				if !it.IsComposioManaged && strings.EqualFold(it.AuthScheme, authMode) {
					return &composioAuthConfig{ID: it.ID, AuthScheme: it.AuthScheme}, nil
				}
			}
		}
	}

	// Build the create body.
	var authConfig map[string]any
	if useCustom {
		body := map[string]any{
			"type":       "use_custom_auth",
			"name":       "apteva/" + toolkitSlug,
			"authScheme": strings.ToUpper(authMode),
		}
		if len(creds) > 0 {
			body["credentials"] = creds
		} else {
			// Composio requires the credentials key to be present even when
			// no config-stage fields apply; empty map is accepted.
			body["credentials"] = map[string]string{}
		}
		authConfig = body
	} else {
		authConfig = map[string]any{
			"type": "use_composio_managed_auth",
			"name": "apteva/" + toolkitSlug,
		}
	}

	body := map[string]any{
		"toolkit":     map[string]string{"slug": toolkitSlug},
		"auth_config": authConfig,
	}
	var resp struct {
		AuthConfig composioAuthConfig `json:"auth_config"`
	}
	if err := c.do("POST", "/api/v3/auth_configs", body, &resp); err != nil {
		return nil, fmt.Errorf("create auth config for %s: %w", toolkitSlug, err)
	}
	return &resp.AuthConfig, nil
}

// InitiateConnection creates a new auth-link session for the given toolkit
// under the project's isolated end-user identity and returns the URL the user
// must open in a browser.
//
// Branches:
//   - configCreds empty + toolkit is composio-managed → one-click managed OAuth.
//   - configCreds non-empty → use_custom_auth with those credentials embedded
//     in the auth_config (API key, basic auth, custom OAuth app).
//   - initCreds non-empty → forwarded as connection_data on the link request
//     (e.g. per-user subdomain, account id, or — for API_KEY auth — the key
//     fields themselves when Composio wants them on the connection rather
//     than the auth config).
//
// For pure API-key toolkits, the returned RedirectURL is typically Composio's
// "authorization complete" landing page; the connected account usually flips
// to ACTIVE immediately without the user needing to interact, so polling
// picks it up on the first tick.
func (c *ComposioClient) InitiateConnection(toolkitSlug, authMode, endUserID string, configCreds, initCreds map[string]string) (acct *ComposioConnectedAccount, redirectURL string, err error) {
	// Per Composio's documented flow:
	//   1. Create an auth config. For managed OAuth this is
	//      use_composio_managed_auth; for anything else (API_KEY, BASIC,
	//      custom OAuth) it's use_custom_auth. Custom-OAuth is the only case
	//      where the auth config itself needs credentials (client_id /
	//      client_secret) — those come in via configCreds. API_KEY toolkits
	//      create an auth config with empty credentials and collect the
	//      actual secret via the Connect Link hosted form at step 2.
	//
	//   2. Create a Connect Link session (POST /connected_accounts/link)
	//      bound to the user id. Returns a redirect URL the user must open
	//      in a browser. For managed OAuth this lands on the provider's
	//      OAuth page; for API_KEY it lands on Composio's hosted credential
	//      form; for custom OAuth it lands on the provider's OAuth page.
	//
	//   3. Composio stores the credentials in its vault, associated with
	//      the auth config + user id. Tools invoked via MCP for this user
	//      get the credentials auto-injected by Composio at call time.
	//
	// There is no direct-create path here — earlier attempts to skip the
	// popup for API_KEY by writing credentials to connection.state.val
	// stored them in the wrong place and caused tool calls to fail with
	// missing auth fields. initCreds is ignored; config-stage fields go
	// into the auth config.
	_ = initCreds
	cfg, err := c.ensureAuthConfig(toolkitSlug, authMode, configCreds)
	if err != nil {
		return nil, "", err
	}

	body := map[string]any{
		"auth_config_id": cfg.ID,
		"user_id":        endUserID,
	}
	var resp struct {
		LinkToken          string `json:"link_token"`
		RedirectURL        string `json:"redirect_url"`
		ExpiresAt          string `json:"expires_at"`
		ConnectedAccountID string `json:"connected_account_id"`
	}
	if err := c.do("POST", "/api/v3/connected_accounts/link", body, &resp); err != nil {
		return nil, "", err
	}
	return &ComposioConnectedAccount{
		ID:           resp.ConnectedAccountID,
		Status:       "INITIATED",
		AuthConfigID: cfg.ID,
	}, resp.RedirectURL, nil
}

// GetConnectedAccount fetches the current status of a connected account by id.
// Used by the polling loop that flips connections.status from pending → active.
func (c *ComposioClient) GetConnectedAccount(id string) (*ComposioConnectedAccount, error) {
	var out ComposioConnectedAccount
	if err := c.do("GET", "/api/v3/connected_accounts/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeConnection deletes a connected account on the Composio side. Called
// when the user deletes a Composio connection in apteva so we do not leak
// authorized accounts.
func (c *ComposioClient) RevokeConnection(id string) error {
	return c.do("DELETE", "/api/v3/connected_accounts/"+id, nil, nil)
}

// FindMCPServerByName looks up an existing Composio MCP server by its exact
// name (case-insensitive match as per Composio's filter behavior). Returns
// nil, nil if no matching server exists.
//
// This is the "upsert" companion for CreateMCPServer: Composio does not
// expose an update endpoint for custom MCP servers, and the create endpoint
// fails with error 1151 (MCP_DuplicateServerName) on collisions. Reconcile
// uses this to find-or-create idempotently.
func (c *ComposioClient) FindMCPServerByName(name string) (*ComposioMCPServer, error) {
	var resp struct {
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			MCPURL string `json:"mcp_url"`
		} `json:"items"`
	}
	path := "/api/v3/mcp/servers?name=" + urlQueryEscape(name) + "&limit=50"
	if err := c.do("GET", path, nil, &resp); err != nil {
		return nil, err
	}
	// Composio's name filter is "partial match" — pick an exact match only.
	for _, it := range resp.Items {
		if strings.EqualFold(it.Name, name) {
			return &ComposioMCPServer{ID: it.ID, Name: it.Name, URL: it.MCPURL}, nil
		}
	}
	return nil, nil
}

// CreateMCPServer creates a Composio "custom" MCP server that exposes the
// given set of toolkits behind a single URL. Composio's create endpoint is
// NOT idempotent — it returns HTTP 400 / code 1151 when a server with the
// same name already exists. This method makes it idempotent from the
// caller's perspective:
//   1. POST /api/v3/mcp/servers/custom
//   2. If the error is a duplicate-name error, look up the existing server
//      by name via FindMCPServerByName and return it.
//   3. Any other error propagates.
//
// The optional `actions` list scopes the hosted MCP endpoint to specific
// action ids; nil/empty exposes every tool from every toolkit in the
// toolkit list. To modify `actions` on an existing server use UpdateMCPServer
// — PATCH /api/v3/mcp/{id} — which the reconciler calls after a filter
// change so a single upstream server is updated in place instead of a new
// versioned server being created.
func (c *ComposioClient) CreateMCPServer(name string, toolkitSlugs []string, authConfigIDs []string, actions []string) (*ComposioMCPServer, error) {
	body := map[string]any{
		"name":                      name,
		"toolkits":                  toolkitSlugs,
		"managed_auth_via_composio": true,
	}
	if len(authConfigIDs) > 0 {
		body["auth_config_ids"] = authConfigIDs
	}
	if len(actions) > 0 {
		body["actions"] = actions
	}
	var out ComposioMCPServer
	err := c.do("POST", "/api/v3/mcp/servers/custom", body, &out)
	if err == nil {
		return &out, nil
	}
	// Fall back on duplicate-name — fetch the existing server and return it.
	// We match by the error body because Composio's error envelope is stable
	// even though HTTP status codes may shift.
	msg := err.Error()
	if strings.Contains(msg, "MCP_DuplicateServerName") || strings.Contains(msg, "already exists") {
		existing, lerr := c.FindMCPServerByName(name)
		if lerr != nil {
			return nil, fmt.Errorf("%w (and lookup failed: %v)", err, lerr)
		}
		if existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("%w (duplicate name but lookup returned no match)", err)
	}
	return nil, err
}

// DeleteMCPServer is a no-op today because the v3 API does not expose a
// top-level server delete (only instance delete). Reconciliation handles
// scope-empty cleanup by updating the upstream server to an empty toolkit
// list instead.
func (c *ComposioClient) DeleteMCPServer(_ string) error {
	return nil
}

// UpdateMCPServer patches an existing Composio MCP server's allowed_tools
// filter in place. Passing a nil or empty list clears the filter, which
// Composio interprets as "expose every tool in the toolkit". This lets the
// reconciler sync filter changes without creating a second upstream server.
func (c *ComposioClient) UpdateMCPServer(serverID string, allowedTools []string) (*ComposioMCPServer, error) {
	body := map[string]any{
		"allowed_tools": allowedTools,
	}
	if allowedTools == nil {
		body["allowed_tools"] = []string{}
	}
	var out ComposioMCPServer
	if err := c.do("PATCH", "/api/v3/mcp/"+serverID, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMCPInstances returns the per-user instances registered against a given
// MCP server config. Used by the reconciler to check whether the current
// end-user already has an instance before creating one.
func (c *ComposioClient) ListMCPInstances(serverID string) ([]string, error) {
	var resp struct {
		Instances []struct {
			InstanceID string `json:"instance_id"`
		} `json:"instances"`
	}
	if err := c.do("GET", "/api/v3/mcp/servers/"+serverID+"/instances?limit=100", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Instances))
	for _, it := range resp.Instances {
		out = append(out, it.InstanceID)
	}
	return out, nil
}

// EnsureMCPInstance creates an instance for the given user_id under the given
// MCP server config. Composio's POST /instances endpoint is idempotent per
// user_id (calling twice is fine), but we still skip the call when we can
// see the instance already exists to save a round trip. Any 4xx error from
// Composio that doesn't look like "already exists" propagates.
func (c *ComposioClient) EnsureMCPInstance(serverID, userID string) error {
	body := map[string]any{"user_id": userID}
	err := c.do("POST", "/api/v3/mcp/servers/"+serverID+"/instances", body, nil)
	if err == nil {
		return nil
	}
	// Composio returns a conflict-ish error if the instance already exists —
	// we treat that as success. Error envelopes use "already" in the message.
	if strings.Contains(err.Error(), "already") {
		return nil
	}
	return err
}

// GenerateMCPURL asks Composio for the per-user connectable URL for a given
// server + user_id. The /generate endpoint returns a list of URLs — one per
// user_id we passed — so we take the first entry.
//
// managed controls the `managed_auth_by_composio` flag; we currently always
// pass true because our reconcile uses composio-managed servers only.
func (c *ComposioClient) GenerateMCPURL(serverID, userID string) (string, error) {
	body := map[string]any{
		"mcp_server_id":            serverID,
		"managed_auth_by_composio": true,
		"user_ids":                 []string{userID},
	}
	var resp struct {
		MCPURL      string   `json:"mcp_url"`
		UserIDsURL  []string `json:"user_ids_url"`
	}
	if err := c.do("POST", "/api/v3/mcp/servers/generate", body, &resp); err != nil {
		return "", err
	}
	if len(resp.UserIDsURL) > 0 && resp.UserIDsURL[0] != "" {
		return resp.UserIDsURL[0], nil
	}
	return resp.MCPURL, nil
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// stripComposioHelperActions forces `include_composio_helper_actions=false` on
// a Composio MCP URL. The helpers (COMPOSIO_CHECK_ACTIVE_CONNECTION,
// INITIATE_CONNECTION, etc.) are pure connection-management tools and our
// server already owns that flow via the reconciler — exposing them to agents
// just bloats the tool schema (~5k extra input tokens per spawn) and tempts
// workers into retry dances when a connection blips. Called on every URL we
// persist so new and reconciled rows are clean by default.
func stripComposioHelperActions(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("include_composio_helper_actions", "false")
	u.RawQuery = q.Encode()
	return u.String()
}

// mcpServerNameFor builds a deterministic Composio MCP server name within
// Composio's 4-30 character window. The hash input is the end-user id PLUS
// the sorted toolkit slugs, so any change to the toolkit set produces a new
// name — which lets us sidestep Composio's lack of an MCP-server update
// endpoint. Same toolkit set → same name → reuse existing server; different
// set → different name → fresh server.
//
// Format: "apteva-" + first 16 hex chars of sha256(endUserID + "|" + slugs) = 23 chars.
//
// Side effect: every unique toolkit combination a user ever authorizes
// leaves an orphan MCP server on Composio's side (they don't expose a
// server-level DELETE). This is an acceptable cost — orphans don't break
// anything and the total count is bounded by 2^N where N is the number of
// toolkits ever authorized in a project.
func mcpServerNameFor(endUserID string, toolkitSlugs []string) string {
	// Canonicalize the slug list so ordering doesn't affect the hash.
	sorted := append([]string(nil), toolkitSlugs...)
	sort.Strings(sorted)
	input := endUserID + "|" + strings.Join(sorted, ",")
	sum := sha256.Sum256([]byte(input))
	suffix := hex.EncodeToString(sum[:])[:16]
	return "apteva-" + suffix
}

// stripLegacyVariantSuffix removes the old "-v<hex>" variant suffix that a
// prior version of the reconciler appended to upstream_id to encode the
// allowed_tools hash. Real Composio server ids are plain UUIDs (five
// hyphen-separated hex groups: 8-4-4-4-12), so anything past the 36-char
// UUID boundary is the legacy suffix. No-op for clean ids.
func stripLegacyVariantSuffix(upstreamID string) string {
	if len(upstreamID) <= 36 {
		return upstreamID
	}
	head := upstreamID[:36]
	tail := upstreamID[36:]
	if strings.HasPrefix(tail, "-v") {
		return head
	}
	return upstreamID
}

// ListToolkitActions returns the set of actions Composio exposes for a
// toolkit slug. This powers the dashboard's tool-picker: the user sees every
// available action and ticks the ones they want enabled on their MCP server
// row. The server then persists those names as allowed_tools and passes them
// to CreateMCPServer on the next reconcile.
//
// Uses /api/v3.1/tools with toolkit_versions=latest — the v3 endpoint and
// default version selector both under-report the action count (googledrive
// shows 51 vs. the 76 the Composio dashboard lists). Pagination is cursor
// based (max limit 1000); we use 500 and walk next_cursor to be safe.
func (c *ComposioClient) ListToolkitActions(toolkitSlug string) ([]ComposioAction, error) {
	var out []ComposioAction
	cursor := ""
	for {
		path := fmt.Sprintf("/api/v3.1/tools?toolkit_slug=%s&toolkit_versions=latest&limit=500", toolkitSlug)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var resp struct {
			Items []struct {
				Slug            string         `json:"slug"`
				Name            string         `json:"name"`
				Description     string         `json:"description"`
				InputParameters map[string]any `json:"input_parameters"`
				Toolkit         struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
				IsDeprecated bool `json:"is_deprecated"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			if it.IsDeprecated {
				continue
			}
			out = append(out, ComposioAction{
				Slug:            it.Slug,
				Name:            it.Name,
				Description:     it.Description,
				Toolkit:         it.Toolkit.Slug,
				InputParameters: it.InputParameters,
			})
		}
		if resp.NextCursor == "" || len(resp.Items) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return out, nil
}

// ComposioAction is a single action (tool) exposed by a Composio toolkit.
// Slug is what gets passed in the `actions` array when creating an MCP
// server — e.g. "GOOGLESHEETS_BATCH_GET_VALUES". InputParameters is the
// JSON Schema for the action's arguments (`input_parameters` on the
// /api/v3.1/tools response); it mirrors the MCP `inputSchema` shape and is
// forwarded to the dashboard Test modal so users can invoke the tool.
type ComposioAction struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	Toolkit         string         `json:"toolkit"`
	InputParameters map[string]any `json:"input_parameters,omitempty"`
}

// --- Triggers ---

// ComposioTriggerType describes one trigger template in Composio's catalog.
// Slug is the unique key ("GMAIL_NEW_GMAIL_MESSAGE",
// "GOOGLESHEETS_CELL_RANGE_VALUES_CHANGED", etc). Kind is "webhook" (real
// push from upstream) or "poll" (Composio polls upstream on a schedule).
// Config is the raw JSON schema describing trigger_config fields the caller
// must supply when upserting an instance (e.g. spreadsheet_id, range).
type ComposioTriggerType struct {
	Slug         string                 `json:"slug"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Instructions string                 `json:"instructions"`
	Kind         string                 `json:"type"` // "webhook" | "poll"
	Toolkit      string                 `json:"toolkit"`
	Config       map[string]interface{} `json:"config"`
}

// ListTriggerTypes fetches trigger templates for one or more toolkits from
// Composio. Used by the dashboard Subscriptions tab to populate the
// trigger picker when the connection's source is composio. Walks the
// cursor in case a toolkit exposes more than `limit` triggers.
func (c *ComposioClient) ListTriggerTypes(toolkitSlug string) ([]ComposioTriggerType, error) {
	var out []ComposioTriggerType
	cursor := ""
	for {
		path := fmt.Sprintf("/api/v3/triggers_types?toolkit_slugs=%s&limit=100", toolkitSlug)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var resp struct {
			Items []struct {
				Slug         string                 `json:"slug"`
				Name         string                 `json:"name"`
				Description  string                 `json:"description"`
				Instructions string                 `json:"instructions"`
				Type         string                 `json:"type"`
				Toolkit      struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
				Config map[string]interface{} `json:"config"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			out = append(out, ComposioTriggerType{
				Slug:         it.Slug,
				Name:         it.Name,
				Description:  it.Description,
				Instructions: it.Instructions,
				Kind:         it.Type,
				Toolkit:      it.Toolkit.Slug,
				Config:       it.Config,
			})
		}
		if resp.NextCursor == "" || len(resp.Items) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return out, nil
}

// UpsertTriggerInstance creates (or updates in place) a trigger instance
// scoped to one connected account. Returns the Composio-assigned instance
// id which we store as external_webhook_id on the apteva subscription row.
func (c *ComposioClient) UpsertTriggerInstance(slug, connectedAccountID string, config map[string]any) (string, error) {
	body := map[string]any{
		"connected_account_id": connectedAccountID,
		"trigger_config":       config,
	}
	var resp struct {
		TriggerID string `json:"trigger_id"`
		ID        string `json:"id"`
	}
	if err := c.do("POST", fmt.Sprintf("/api/v3/trigger_instances/%s/upsert", slug), body, &resp); err != nil {
		return "", err
	}
	if resp.TriggerID != "" {
		return resp.TriggerID, nil
	}
	return resp.ID, nil
}

// PatchTriggerInstance enables or disables an existing trigger instance
// on Composio's side. Called from handleToggleSubscription when the sub's
// connection is composio-sourced.
func (c *ComposioClient) PatchTriggerInstance(triggerID string, enable bool) error {
	status := "enable"
	if !enable {
		status = "disable"
	}
	return c.do("PATCH", fmt.Sprintf("/api/v3/trigger_instances/manage/%s", triggerID), map[string]any{"status": status}, nil)
}

// DeleteTriggerInstance permanently removes a trigger instance.
func (c *ComposioClient) DeleteTriggerInstance(triggerID string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v3/trigger_instances/manage/%s", triggerID), nil, nil)
}

// ComposioWebhookSubscription represents the per-project webhook
// destination Composio POSTs trigger events to. Exactly one allowed per
// Composio project; the signing secret is returned on create only.
type ComposioWebhookSubscription struct {
	ID            string   `json:"id"`
	WebhookURL    string   `json:"webhook_url"`
	Version       string   `json:"version"`
	EnabledEvents []string `json:"enabled_events"`
	Secret        string   `json:"secret"`
}

// EnsureWebhookSubscription idempotently sets up the project-level
// webhook subscription that receives all trigger events. Tries to find
// an existing one first; if none exists, creates one and returns the
// signing secret. If one exists but points at a different URL, updates
// it via PATCH. Composio caps projects at one webhook_subscription so we
// always converge to the same row.
//
// Callers persist the returned secret in the provider's encrypted blob
// because Composio never returns it again after the initial create.
func (c *ComposioClient) EnsureWebhookSubscription(webhookURL string, events []string) (*ComposioWebhookSubscription, error) {
	if len(events) == 0 {
		events = []string{"trigger.event"}
	}
	// Look for an existing subscription first.
	var listResp struct {
		Items []ComposioWebhookSubscription `json:"items"`
	}
	if err := c.do("GET", "/api/v3/webhook_subscriptions", nil, &listResp); err == nil && len(listResp.Items) > 0 {
		existing := listResp.Items[0]
		// If URL matches and events superset the desired ones, reuse.
		if existing.WebhookURL == webhookURL && containsAll(existing.EnabledEvents, events) {
			return &existing, nil
		}
		// Otherwise, patch it in place. PATCH response does NOT include
		// the secret — caller is expected to have stashed it the first
		// time around. We return the existing row sans secret; the
		// caller code treats an empty secret on reuse as "already known".
		patchBody := map[string]any{
			"webhook_url":    webhookURL,
			"enabled_events": events,
		}
		var patched ComposioWebhookSubscription
		if err := c.do("PATCH", "/api/v3/webhook_subscriptions/"+existing.ID, patchBody, &patched); err != nil {
			return nil, err
		}
		if patched.ID == "" {
			patched.ID = existing.ID
		}
		return &patched, nil
	}
	// None exists — create it. This is the only path that returns the
	// signing secret, so callers must persist it from this response.
	body := map[string]any{
		"webhook_url":    webhookURL,
		"enabled_events": events,
		"version":        "V3",
	}
	var created ComposioWebhookSubscription
	if err := c.do("POST", "/api/v3/webhook_subscriptions", body, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// containsAll reports whether every element of `want` is present in `got`.
func containsAll(got, want []string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// --- Server integration ---

// handleComposioReconcile is a manual trigger for the Composio MCP
// aggregation reconciler. The dashboard calls this when the user clicks a
// "Sync Composio MCP" button, or when it notices a composio connection row
// without a matching remote mcp_servers row.
//
// Route: POST /composio/reconcile?provider_id=<id>&project_id=<id>
func (s *Server) handleComposioReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	providerID, err := atoi64(r.URL.Query().Get("provider_id"))
	if err != nil {
		http.Error(w, "provider_id required", http.StatusBadRequest)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	log.Printf("[COMPOSIO-RECONCILE] manual trigger user=%d provider=%d project=%s remote=%s",
		userID, providerID, projectID, r.RemoteAddr)
	if err := s.reconcileComposioMCPServer(userID, providerID, projectID); err != nil {
		log.Printf("[COMPOSIO-RECONCILE] manual trigger FAILED: %v", err)
		http.Error(w, "reconcile: "+err.Error(), http.StatusBadGateway)
		return
	}
	// With the per-toolkit design, there can be N rows for a given
	// (user, provider, project) tuple — one per active connection.
	rows, _ := s.store.ListMCPServers(userID, projectID)
	var composioRows []MCPServerRecord
	for _, r := range rows {
		if r.Source == "remote" && r.ProviderID == providerID {
			composioRows = append(composioRows, r)
		}
	}
	writeJSON(w, map[string]any{
		"status":      "ok",
		"mcp_servers": composioRows,
	})
}

// handleGetComposioToolkit proxies the per-toolkit detail endpoint so the
// dashboard can render a credential form for toolkits that need custom auth.
//
// Route: GET /composio/toolkit/<slug>?provider_id=<id>
func (s *Server) handleGetComposioToolkit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	providerID, err := atoi64(r.URL.Query().Get("provider_id"))
	if err != nil {
		http.Error(w, "provider_id required", http.StatusBadRequest)
		return
	}
	slug := strings.TrimPrefix(r.URL.Path, "/composio/toolkit/")
	if slug == "" {
		http.Error(w, "toolkit slug required in path", http.StatusBadRequest)
		return
	}
	client, err := s.composioClientFor(userID, providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	details, err := client.GetToolkitDetails(slug)
	if err != nil {
		http.Error(w, "composio: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, details)
}

// handleListComposioApps proxies Composio's toolkit catalog for the dashboard,
// using the user's stored API key so the key never leaves the server.
//
// Route: GET /composio/apps?provider_id=<id>[&search=<text>]
func (s *Server) handleListComposioApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	providerID, err := atoi64(r.URL.Query().Get("provider_id"))
	if err != nil {
		http.Error(w, "provider_id required", http.StatusBadRequest)
		return
	}
	search := r.URL.Query().Get("search")
	client, err := s.composioClientFor(userID, providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	apps, err := client.ListApps(search)
	if err != nil {
		http.Error(w, "composio: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, apps)
}

// testComposioBaseURL is a test-only hook. When non-empty, composioClientFor
// points the returned client at this URL instead of production Composio. Tests
// set it via t.Cleanup to isolate runs.
var testComposioBaseURL string

// composioClientFor returns a Composio client authenticated with the credential
// stored in the given providers row. Returns an error if the row is not a
// Composio provider or the key is missing.
// newComposioClient picks the first active Composio provider for the given
// user and returns a client for it, or nil if none is configured. Used by
// routes that need Composio-side info (toolkit actions, toolkit details)
// without being tied to a specific provider id.
func (s *Server) newComposioClient(userID int64) *ComposioClient {
	providers, err := s.store.ListProviders(userID)
	if err != nil {
		return nil
	}
	for _, p := range providers {
		if !strings.EqualFold(p.Name, "Composio") {
			continue
		}
		if c, err := s.composioClientFor(userID, p.ID); err == nil {
			return c
		}
	}
	return nil
}

func (s *Server) composioClientFor(userID, providerID int64) (*ComposioClient, error) {
	p, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	if !strings.EqualFold(p.Name, "Composio") {
		return nil, fmt.Errorf("provider %d is not Composio (got %q)", providerID, p.Name)
	}
	plain, err := Decrypt(s.secret, encData)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider: %w", err)
	}
	var fields map[string]string
	_ = json.Unmarshal([]byte(plain), &fields)
	apiKey := fields["COMPOSIO_API_KEY"]
	if apiKey == "" {
		return nil, fmt.Errorf("COMPOSIO_API_KEY missing from provider %d", providerID)
	}
	c := NewComposioClient(apiKey)
	if testComposioBaseURL != "" {
		c.BaseURL = testComposioBaseURL
	}
	return c, nil
}

// composioEndUserID returns the stable per-project identifier we pass to
// Composio as the end_user_id. Project-scoped so tenants have isolated
// Composio accounts; falls back to the apteva user id when no project.
func composioEndUserID(userID int64, projectID string) string {
	if projectID != "" {
		return "proj:" + projectID
	}
	return fmt.Sprintf("user:%d", userID)
}

// ensureComposioWebhookSubscription idempotently registers a project-level
// Composio webhook_subscription pointing back at apteva-server's unified
// ingress route. Returns the signing secret so the caller can validate
// inbound HMAC headers.
//
// The webhook URL uses an opaque per-provider token as its path
// component — the unified /webhooks/<token> handler matches that token
// against the providers.webhook_token column and dispatches to the
// Composio trigger flow. No provider-name or project id ever appears
// in the URL, which makes the route provider-agnostic (future trigger
// backends like Svix or n8n will use the same endpoint) and prevents
// URL enumeration.
//
// Flow:
//  1. Load the Composio provider row and its webhook_token column.
//     Mint a fresh token if empty and persist before touching Composio.
//  2. Decrypt its credential blob to check for a cached signing secret.
//  3. Call EnsureWebhookSubscription on Composio with our ingress URL.
//  4. If Composio returns a secret (create path), persist it in the
//     blob. If PATCH returns empty, keep the cached secret.
//  5. Return the secret.
//
// Called lazily on the first Composio-source subscription create in a
// project so users who never touch triggers never pay for the setup.
func (s *Server) ensureComposioWebhookSubscription(userID int64, providerID int64, projectID string) (string, error) {
	p, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		return "", fmt.Errorf("provider: %w", err)
	}
	if !strings.EqualFold(p.Name, "Composio") {
		return "", fmt.Errorf("provider %d is not Composio", providerID)
	}
	// Mint + persist the webhook token before calling Composio so the
	// ingress route resolves correctly the instant Composio starts
	// delivering events. Existing rows keep their token.
	var currentToken string
	s.store.db.QueryRow("SELECT COALESCE(webhook_token,'') FROM providers WHERE id = ?", providerID).Scan(&currentToken)
	if currentToken == "" {
		currentToken = generateToken(16)
		if err := s.store.SetProviderWebhookToken(providerID, currentToken); err != nil {
			return "", fmt.Errorf("persist webhook_token: %w", err)
		}
	}

	plain, err := Decrypt(s.secret, encData)
	if err != nil {
		return "", fmt.Errorf("decrypt provider: %w", err)
	}
	var blob map[string]string
	_ = json.Unmarshal([]byte(plain), &blob)
	if blob == nil {
		blob = map[string]string{}
	}

	wantURL := s.publicBaseURL() + "/webhooks/" + currentToken
	cachedURL := blob["composio_webhook_url"]
	cachedSecret := blob["composio_webhook_secret"]

	// Already bootstrapped at this URL? Reuse.
	if cachedURL == wantURL && cachedSecret != "" {
		return cachedSecret, nil
	}

	client, err := s.composioClientFor(userID, providerID)
	if err != nil {
		return "", err
	}
	sub, err := client.EnsureWebhookSubscription(wantURL, []string{"trigger.event"})
	if err != nil {
		return "", fmt.Errorf("composio webhook subscribe: %w", err)
	}

	// Composio only returns the secret on CREATE, not on PATCH or reuse.
	secret := sub.Secret
	if secret == "" {
		secret = cachedSecret
	}
	if secret == "" {
		return "", fmt.Errorf("composio webhook_subscriptions returned no signing secret — cannot validate inbound HMAC")
	}

	blob["composio_webhook_id"] = sub.ID
	blob["composio_webhook_url"] = wantURL
	blob["composio_webhook_secret"] = secret
	updated, err := json.Marshal(blob)
	if err != nil {
		return "", err
	}
	encUpdated, err := Encrypt(s.secret, string(updated))
	if err != nil {
		return "", err
	}
	if err := s.store.UpdateProvider(userID, providerID, p.Type, p.Name, encUpdated); err != nil {
		return "", err
	}
	return secret, nil
}

// GET /api/connections/:id/triggers — list Composio trigger templates
// for the connection's toolkit. Only composio-source connections return
// content; local-source returns 404. Response shape is designed to feed
// directly into the dashboard trigger picker.
func (s *Server) handleConnectionTriggers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/triggers")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if conn.Source != "composio" {
		http.Error(w, "triggers are only supported for composio connections", http.StatusNotFound)
		return
	}
	if conn.ProviderID == 0 {
		http.Error(w, "composio connection missing provider_id", http.StatusBadRequest)
		return
	}
	client, err := s.composioClientFor(userID, conn.ProviderID)
	if err != nil {
		http.Error(w, "composio client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	triggers, err := client.ListTriggerTypes(conn.AppSlug)
	if err != nil {
		http.Error(w, "list trigger types: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"connection_id": conn.ID,
		"toolkit":       conn.AppSlug,
		"triggers":      triggers,
	})
}

// reconcileComposioMCPServer ensures there is exactly one remote mcp_servers
// row per active Composio connection in the (user, provider, project) scope.
// Each row is named after the connection's toolkit slug (e.g. "pushover",
// "googlesheets") so the Attach picker and MCP Servers tab show meaningful
// entries instead of a single opaque "composio" aggregate.
//
// Upstream, each row corresponds to its own per-toolkit Composio MCP server
// created via POST /mcp/servers/custom with a single-entry toolkits list.
// The server name is hashed from (endUserID, toolkit) so two different
// toolkits produce different upstream server names, sidestepping Composio's
// non-idempotent duplicate-name error.
//
// Semantics:
//   - For each active Composio connection in scope, ensure there's a local
//     row named after its toolkit slug. Create if missing, update URL if
//     the upstream regenerated one.
//   - For any existing local row whose toolkit is no longer in the active
//     connection set, delete it. Also cleans up any legacy "composio"
//     aggregate row from the previous aggregate-per-project design.
//
// Called after every Composio connection create/delete.
func (s *Server) reconcileComposioMCPServer(userID, providerID int64, projectID string) error {
	start := time.Now()
	log.Printf("[COMPOSIO-RECONCILE] begin user=%d provider=%d project=%s", userID, providerID, projectID)
	defer func() {
		log.Printf("[COMPOSIO-RECONCILE] end user=%d provider=%d project=%s dur=%s",
			userID, providerID, projectID, time.Since(start).Round(time.Millisecond))
	}()
	client, err := s.composioClientFor(userID, providerID)
	if err != nil {
		log.Printf("[COMPOSIO-RECONCILE] client resolve failed: %v", err)
		return err
	}

	// Gather desired toolkit slugs (deduped) from active Composio connections
	// in this project scope. Keep a parallel map from slug → connection so
	// we can use the connection's human-readable AppName for the description.
	all, err := s.store.ListConnections(userID, projectID)
	if err != nil {
		return err
	}
	type wanted struct {
		slug    string
		appName string
	}
	desiredBySlug := map[string]*wanted{}
	for _, c := range all {
		if c.Source != "composio" || c.Status != "active" || c.ProviderID != providerID {
			continue
		}
		if _, ok := desiredBySlug[c.AppSlug]; !ok {
			appName := c.AppName
			if appName == "" || appName == c.AppSlug {
				appName = c.AppSlug
			}
			desiredBySlug[c.AppSlug] = &wanted{slug: c.AppSlug, appName: appName}
		}
	}

	// Existing remote rows for this (user, provider, project) tuple, keyed
	// by name. We'll update or create entries to match desiredBySlug, and
	// drop anything left over at the end (toolkits that were removed, plus
	// any legacy "composio" aggregate row).
	existingByName := map[string]*MCPServerRecord{}
	rows, _ := s.store.ListMCPServers(userID, projectID)
	for i := range rows {
		if rows[i].Source == "remote" && rows[i].ProviderID == providerID {
			existingByName[rows[i].Name] = &rows[i]
		}
	}

	endUserID := composioEndUserID(userID, projectID)

	log.Printf("[COMPOSIO-RECONCILE] desired_slugs=%d existing_remote_rows=%d", len(desiredBySlug), len(existingByName))

	// Upsert one row per desired toolkit.
	for slug, w := range desiredBySlug {
		// Preserve any existing allowed_tools filter the user set through
		// the dashboard or agent gateway. Without this, every reconcile
		// would reset the filter back to "all tools".
		existing, existedBefore := existingByName[slug]
		var allowedTools []string
		if existedBefore {
			allowedTools = append([]string(nil), existing.AllowedTools...)
		}
		log.Printf("[COMPOSIO-RECONCILE] toolkit=%s existed=%v allowed_tools=%d", slug, existedBefore, len(allowedTools))

		// One upstream Composio server per (user, toolkit). On the first
		// reconcile we create it; on every subsequent reconcile we PATCH its
		// allowed_tools in place. Composio exposes PATCH /api/v3/mcp/{id}
		// (confirmed in the public API docs), which makes this a true
		// in-place update — no name versioning, no orphaned servers.
		upstreamName := mcpServerNameFor(endUserID, []string{slug})
		var upstreamID string
		if existedBefore {
			upstreamID = stripLegacyVariantSuffix(existing.UpstreamID)
		}

		var upstream *ComposioMCPServer
		if existedBefore && upstreamID != "" {
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s patch upstream=%s allowed_tools=%d", slug, upstreamID, len(allowedTools))
			patched, err := client.UpdateMCPServer(upstreamID, allowedTools)
			if err != nil {
				log.Printf("[COMPOSIO-RECONCILE] toolkit=%s UpdateMCPServer failed: %v", slug, err)
				return fmt.Errorf("update composio mcp for %s: %w", slug, err)
			}
			// Fall back to the stored URL if the PATCH response omits it.
			url := patched.URL
			if url == "" {
				url = existing.URL
			}
			upstream = &ComposioMCPServer{ID: upstreamID, URL: url}
		} else {
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s creating upstream name=%s allowed_tools=%d", slug, upstreamName, len(allowedTools))
			created, err := client.CreateMCPServer(upstreamName, []string{slug}, nil, allowedTools)
			if err != nil {
				log.Printf("[COMPOSIO-RECONCILE] toolkit=%s CreateMCPServer failed: %v", slug, err)
				return fmt.Errorf("create composio mcp for %s: %w", slug, err)
			}
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s created upstream id=%s", slug, created.ID)
			upstream = created
		}

		// Ensure a per-user instance exists.
		if err := client.EnsureMCPInstance(upstream.ID, endUserID); err != nil {
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s EnsureMCPInstance failed: %v", slug, err)
			return fmt.Errorf("ensure composio mcp instance for %s: %w", slug, err)
		}
		// Resolve the per-user connection URL (the /custom response is a
		// server-level base URL with no user routing).
		connectURL, err := client.GenerateMCPURL(upstream.ID, endUserID)
		if err != nil {
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s GenerateMCPURL failed: %v", slug, err)
			return fmt.Errorf("generate composio mcp url for %s: %w", slug, err)
		}
		if connectURL == "" {
			connectURL = upstream.URL
		}
		connectURL = stripComposioHelperActions(connectURL)

		description := fmt.Sprintf("Composio hosted MCP — %s", w.appName)

		var localID int64
		if existedBefore {
			if err := s.store.UpdateMCPServerURL(existing.ID, connectURL); err != nil {
				return err
			}
			// Clean up any legacy "-vXXXX" suffix left on the stored upstream
			// id by the old name-versioning scheme, so later PATCH calls hit
			// the real Composio server id.
			if existing.UpstreamID != upstream.ID {
				if err := s.store.UpdateMCPServerUpstreamID(existing.ID, upstream.ID); err != nil {
					return err
				}
			}
			localID = existing.ID
			delete(existingByName, slug) // mark as handled so it isn't reaped below
		} else {
			row, err := s.store.CreateMCPServerExt(MCPServerInput{
				UserID:       userID,
				Name:         slug,
				Description:  description,
				Source:       "remote",
				Transport:    "http",
				URL:          connectURL,
				ProviderID:   providerID,
				ProjectID:    projectID,
				AllowedTools: allowedTools,
				UpstreamID:   upstream.ID,
			})
			if err != nil {
				log.Printf("[COMPOSIO-RECONCILE] toolkit=%s CreateMCPServerExt failed: %v", slug, err)
				return err
			}
			log.Printf("[COMPOSIO-RECONCILE] toolkit=%s created local mcp_id=%d", slug, row.ID)
			localID = row.ID
		}

		// Best-effort async probe: discover tools and mark the row reachable.
		// Failures downgrade to "unprobed" but do not fail the whole reconcile.
		// For tool_count we prefer the toolkit catalog size over the
		// MCP-advertised list, because the MCP endpoint applies the
		// allowed_tools filter (and inflates by helper actions), so its count
		// would misrepresent the server's capacity in the dashboard header.
		go func(rowID int64, toolkitSlug string) {
			time.Sleep(500 * time.Millisecond)
			row, _, rerr := s.store.GetMCPServer(userID, rowID)
			if rerr != nil {
				return
			}
			s.mcpManager.Stop(rowID)
			proc, perr := s.mcpManager.Start(row, nil)
			if perr != nil {
				s.store.UpdateMCPServerStatus(rowID, "unprobed", 0, 0)
				return
			}
			count := len(proc.Tools)
			if actions, aerr := client.ListToolkitActions(toolkitSlug); aerr == nil && len(actions) > 0 {
				count = len(actions)
			}
			s.store.UpdateMCPServerStatus(rowID, "reachable", count, 0)
		}(localID, slug)
	}

	// Reap leftovers: any remote row for this provider/project whose name
	// isn't a currently-desired toolkit slug. This covers both connection
	// deletions and legacy "composio" aggregate rows from the old design.
	for name, row := range existingByName {
		log.Printf("[COMPOSIO-RECONCILE] reaping stale row id=%d name=%s (no matching active connection)", row.ID, name)
		s.mcpManager.Stop(row.ID)
		s.store.DeleteMCPServer(userID, row.ID)
	}

	return nil
}

// --- Bulk listing used by provider-create sync ---

// ComposioAuthConfigSummary is what ListAllAuthConfigs returns — enough to
// correlate a connected_account's auth_config_id back to a toolkit slug
// when the connected_account payload doesn't carry the slug inline.
type ComposioAuthConfigSummary struct {
	ID                string
	AuthScheme        string
	IsComposioManaged bool
	ToolkitSlug       string
}

// ListAllAuthConfigs pages GET /api/v3/auth_configs across toolkits.
func (c *ComposioClient) ListAllAuthConfigs() ([]ComposioAuthConfigSummary, error) {
	var out []ComposioAuthConfigSummary
	cursor := ""
	for {
		var resp struct {
			Items []struct {
				ID                string `json:"id"`
				AuthScheme        string `json:"auth_scheme"`
				IsComposioManaged bool   `json:"is_composio_managed"`
				Toolkit           struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		path := "/api/v3/auth_configs?limit=100"
		if cursor != "" {
			path += "&cursor=" + urlQueryEscape(cursor)
		}
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			out = append(out, ComposioAuthConfigSummary{
				ID:                it.ID,
				AuthScheme:        it.AuthScheme,
				IsComposioManaged: it.IsComposioManaged,
				ToolkitSlug:       it.Toolkit.Slug,
			})
		}
		if resp.NextCursor == "" || len(resp.Items) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return out, nil
}

// ComposioConnectedAccountSummary is the bulk-list shape — richer than
// ComposioConnectedAccount because the list endpoint returns the nested
// auth_config + toolkit inline.
type ComposioConnectedAccountSummary struct {
	ID           string
	Status       string
	AuthConfigID string
	ToolkitSlug  string
}

// ListConnectedAccounts pages GET /api/v3/connected_accounts.
func (c *ComposioClient) ListConnectedAccounts() ([]ComposioConnectedAccountSummary, error) {
	var out []ComposioConnectedAccountSummary
	cursor := ""
	for {
		var resp struct {
			Items []struct {
				ID           string `json:"id"`
				Status       string `json:"status"`
				AuthConfigID string `json:"auth_config_id"`
				AuthConfig   struct {
					ID      string `json:"id"`
					Toolkit struct {
						Slug string `json:"slug"`
					} `json:"toolkit"`
				} `json:"auth_config"`
				Toolkit struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		path := "/api/v3/connected_accounts?limit=100"
		if cursor != "" {
			path += "&cursor=" + urlQueryEscape(cursor)
		}
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			acID := it.AuthConfigID
			if acID == "" {
				acID = it.AuthConfig.ID
			}
			slug := it.Toolkit.Slug
			if slug == "" {
				slug = it.AuthConfig.Toolkit.Slug
			}
			out = append(out, ComposioConnectedAccountSummary{
				ID:           it.ID,
				Status:       it.Status,
				AuthConfigID: acID,
				ToolkitSlug:  slug,
			})
		}
		if resp.NextCursor == "" || len(resp.Items) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return out, nil
}

// ComposioMCPServerSummary is the bulk-list shape for custom MCP servers.
type ComposioMCPServerSummary struct {
	ID            string
	Name          string
	URL           string
	ToolkitSlugs  []string
	AuthConfigIDs []string
	AllowedTools  []string
}

// ListComposioMCPServers pages GET /api/v3/mcp/servers (no name filter).
func (c *ComposioClient) ListComposioMCPServers() ([]ComposioMCPServerSummary, error) {
	var out []ComposioMCPServerSummary
	cursor := ""
	for {
		var resp struct {
			Items []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				MCPURL        string   `json:"mcp_url"`
				Toolkits      []string `json:"toolkits"`
				AuthConfigIDs []string `json:"auth_config_ids"`
				AllowedTools  []string `json:"allowed_tools"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		path := "/api/v3/mcp/servers?limit=100"
		if cursor != "" {
			path += "&cursor=" + urlQueryEscape(cursor)
		}
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			out = append(out, ComposioMCPServerSummary{
				ID:            it.ID,
				Name:          it.Name,
				URL:           it.MCPURL,
				ToolkitSlugs:  it.Toolkits,
				AuthConfigIDs: it.AuthConfigIDs,
				AllowedTools:  it.AllowedTools,
			})
		}
		if resp.NextCursor == "" || len(resp.Items) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return out, nil
}
