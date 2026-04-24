package main

// Credential-group ("suite") support. A template that declares
// `credential_group` + `scopes.account` opts into two behaviors:
//
//   1. Dedup in the catalog UI — all members collapse into one card.
//   2. Master/child storage — one encrypted credential per (user,
//      project, group) lives on a "master" connections row. Child
//      rows for each enabled (sub-app × external-project) store no
//      credentials; instead their JSON blob carries
//      { "_type": "child", "_master_id": 42, "_project_id": "proj_x" }.
//      The executor dereferences the master at call time and applies
//      the declared ProjectBinding to the outbound request.
//
// Zero new DB tables: everything lives inside existing
// `connections.encrypted_credentials` JSON and a reserved app_slug
// prefix `_group:` for master rows.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MasterSlug is the app_slug value used for master-credential rows.
// The `_group:` prefix can never collide with a real template slug
// (JSON slugs don't start with underscore) so we can filter master
// rows out of the user-facing connections list with a prefix check.
func MasterSlug(groupID string) string { return "_group:" + groupID }

// IsMasterSlug returns true when the given app_slug is a master row.
func IsMasterSlug(slug string) bool { return strings.HasPrefix(slug, "_group:") }

// GroupIDFromMasterSlug extracts the group ID from a master slug,
// or "" when the slug is not a master slug.
func GroupIDFromMasterSlug(slug string) string {
	if !IsMasterSlug(slug) {
		return ""
	}
	return strings.TrimPrefix(slug, "_group:")
}

// Reserved keys inside `encrypted_credentials` JSON. The prefix `_`
// was chosen because no known integration uses it as a credential
// field name. If any future template does, rename all six keys.
const (
	credKeyType          = "_type"            // "master" | "child"
	credKeyGroup         = "_group"           // "omnikit" (master only)
	credKeyScope         = "_scope"           // "account" | "project" (master only)
	credKeyProjectsCache = "_projects_cache"  // discovery snapshot (master only)
	credKeyMasterID      = "_master_id"       // child → master row id
	credKeyProjectID     = "_project_id"      // child → external project id
)

// CachedProject is the shape stored in _projects_cache on masters and
// returned to the dashboard.
type CachedProject struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// --- Resolution at execution time ---------------------------------------------

// connectionContext is what the HTTP executor actually consumes for a
// single tool call. Built by resolveConnectionContext below.
type connectionContext struct {
	App              *AppTemplate      // possibly cloned to carry binding headers
	Credentials      map[string]string // master creds for child rows, own creds otherwise
	Input            map[string]any    // possibly augmented with path_param project id
	ProjectBinding   *ProjectBinding   // copy of the active binding, for the header path
	ExternalProjectID string           // child's project id (empty for non-children)
	// MasterConnID is the connection row that owns the credentials.
	// Non-zero when the request resolved through a master. Refresh
	// persistence must write back to this row, not the child.
	MasterConnID int64
}

// resolveConnectionContext inspects `credentials` for master/child
// metadata and builds the execution context the executor needs.
//
// For legacy rows (no `_type` key) it returns the inputs unchanged.
// For master rows (`_type == "master"`) it strips the reserved keys
// and returns the real credentials — master rows shouldn't be used
// directly for tool calls, but we handle it defensively.
// For child rows (`_type == "child"`) it loads the referenced master,
// decrypts its credentials, applies the declared ProjectBinding to
// the outgoing request by cloning the AppTemplate's headers / base
// URL, and injects the external project id into `input` when the
// binding is `path_param`.
func (s *Server) resolveConnectionContext(userID int64, app *AppTemplate, credentials map[string]string, input map[string]any) (*connectionContext, error) {
	return resolveConnectionContextRaw(s.store, s.secret, userID, app, credentials, input)
}

// resolveConnectionContextRaw is the Server-free variant used by
// mcp_proxy, which runs as a subprocess with only store + secret +
// catalog in hand. Behaviour identical to the Server method.
func resolveConnectionContextRaw(store *Store, secret []byte, userID int64, app *AppTemplate, credentials map[string]string, input map[string]any) (*connectionContext, error) {
	ctx := &connectionContext{App: app, Credentials: credentials, Input: input}

	t := credentials[credKeyType]
	if t == "" {
		return ctx, nil
	}
	if t == "master" {
		ctx.Credentials = stripReservedCreds(credentials)
		return ctx, nil
	}
	if t != "child" {
		return nil, fmt.Errorf("unknown credential _type: %q", t)
	}

	// --- child path ---
	masterIDStr := credentials[credKeyMasterID]
	projectID := credentials[credKeyProjectID]
	if masterIDStr == "" || projectID == "" {
		return nil, fmt.Errorf("child credential missing _master_id or _project_id")
	}
	masterID, err := strconv.ParseInt(masterIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("child credential _master_id not numeric: %v", err)
	}
	// Load master. When userID is 0 (mcp_proxy subprocess has no session
	// user), fall back to a direct lookup by id — the subprocess is
	// trusted and launched by the server itself.
	var encCreds string
	if userID != 0 {
		if _, ec, err := store.GetConnection(userID, masterID); err == nil {
			encCreds = ec
		} else {
			return nil, fmt.Errorf("master connection %d not found: %v", masterID, err)
		}
	} else {
		err := store.db.QueryRow(
			"SELECT encrypted_credentials FROM connections WHERE id = ?", masterID,
		).Scan(&encCreds)
		if err != nil {
			return nil, fmt.Errorf("master connection %d not found: %v", masterID, err)
		}
	}
	plain, err := Decrypt(secret, encCreds)
	if err != nil {
		return nil, fmt.Errorf("decrypt master: %v", err)
	}
	var masterCreds map[string]string
	if err := json.Unmarshal([]byte(plain), &masterCreds); err != nil {
		return nil, fmt.Errorf("parse master creds: %v", err)
	}

	// Look up the app's account-scope binding. If the app doesn't have
	// Scopes, fall back to no binding (credentials-only share).
	var binding *ProjectBinding
	if app.Scopes != nil && app.Scopes.Account != nil {
		binding = app.Scopes.Account.ProjectBinding
	}

	ctx.Credentials = stripReservedCreds(masterCreds)
	ctx.ProjectBinding = binding
	ctx.ExternalProjectID = projectID
	ctx.MasterConnID = masterID

	// Apply the binding. For `header` and `path_prefix` we clone the
	// AppTemplate (cheap — reference-shared slices except the two
	// fields we modify) because the catalog's AppTemplate is shared
	// across every goroutine and must not be mutated.
	if binding != nil {
		switch binding.Type {
		case "header":
			clone := cloneAppForBinding(app)
			if clone.Scopes != nil && clone.Scopes.Account != nil && clone.Scopes.Account.AuthHeaders != nil {
				// When the app uses scope-specific auth_headers they
				// take precedence — inject the binding alongside.
				clone.Scopes.Account.AuthHeaders[binding.Name] = resolveBindingValue(binding.Value, projectID)
			}
			if clone.Auth.Headers == nil {
				clone.Auth.Headers = map[string]string{}
			}
			clone.Auth.Headers[binding.Name] = resolveBindingValue(binding.Value, projectID)
			ctx.App = clone
		case "path_prefix":
			clone := cloneAppForBinding(app)
			clone.BaseURL = strings.TrimRight(clone.BaseURL, "/") + "/" + strings.TrimLeft(resolveBindingValue(binding.Value, projectID), "/")
			ctx.App = clone
		case "path_param":
			// Inject into input so the existing `{name}` substitution
			// in buildURL picks it up. Don't overwrite if the caller
			// already passed a value (agent-controlled override).
			if _, already := input[binding.Name]; !already {
				newInput := make(map[string]any, len(input)+1)
				for k, v := range input {
					newInput[k] = v
				}
				newInput[binding.Name] = projectID
				ctx.Input = newInput
			}
		default:
			return nil, fmt.Errorf("unsupported project_binding.type: %q", binding.Type)
		}
	}

	// If the scope declares auth_headers, merge them into the cloned
	// app's Auth.Headers so the existing buildHeaders path works with
	// no extra codepath. Account scope wins over the default auth.
	if app.Scopes != nil && app.Scopes.Account != nil && app.Scopes.Account.AuthHeaders != nil {
		if ctx.App == app { // haven't cloned yet
			ctx.App = cloneAppForBinding(app)
		}
		if ctx.App.Auth.Headers == nil {
			ctx.App.Auth.Headers = map[string]string{}
		}
		for k, v := range app.Scopes.Account.AuthHeaders {
			ctx.App.Auth.Headers[k] = v
		}
	}

	return ctx, nil
}

func cloneAppForBinding(app *AppTemplate) *AppTemplate {
	clone := *app
	// Shallow-copy the auth headers map so mutations don't leak into
	// the shared catalog entry.
	if app.Auth.Headers != nil {
		clone.Auth.Headers = make(map[string]string, len(app.Auth.Headers))
		for k, v := range app.Auth.Headers {
			clone.Auth.Headers[k] = v
		}
	}
	if app.Scopes != nil {
		scopesClone := *app.Scopes
		if app.Scopes.Account != nil {
			accClone := *app.Scopes.Account
			if app.Scopes.Account.AuthHeaders != nil {
				accClone.AuthHeaders = make(map[string]string, len(app.Scopes.Account.AuthHeaders))
				for k, v := range app.Scopes.Account.AuthHeaders {
					accClone.AuthHeaders[k] = v
				}
			}
			scopesClone.Account = &accClone
		}
		clone.Scopes = &scopesClone
	}
	return &clone
}

func resolveBindingValue(tmpl, projectID string) string {
	return strings.ReplaceAll(tmpl, "{{project_id}}", projectID)
}

func stripReservedCreds(c map[string]string) map[string]string {
	out := make(map[string]string, len(c))
	for k, v := range c {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}
	return out
}

// --- Discovery ---------------------------------------------------------------

// discoverProjects calls the group's list_projects endpoint with the
// master's credentials and returns a list of {id, label} pairs. Used
// by both the initial master-creation flow and explicit refreshes.
func discoverProjects(app *AppTemplate, group *CredentialGroup, credentials map[string]string) ([]CachedProject, error) {
	if group == nil || group.Discovery == nil {
		return nil, fmt.Errorf("no discovery config on group %q", groupIDSafe(group))
	}
	call := group.Discovery.ListProjects
	if call.Method == "" || call.Path == "" || call.IDField == "" || call.LabelField == "" {
		return nil, fmt.Errorf("discovery config incomplete (need method, path, id_field, label_field)")
	}

	baseURL := call.BaseURL
	if baseURL == "" {
		baseURL = app.BaseURL
	}
	url := strings.TrimRight(baseURL, "/") + call.Path

	req, err := http.NewRequest(call.Method, url, nil)
	if err != nil {
		return nil, err
	}
	// Re-use the app's account-scope auth headers. The credentials
	// passed in are the already-stripped real creds so template
	// placeholders like {{api_key}} resolve cleanly.
	var authHeaders map[string]string
	if app.Scopes != nil && app.Scopes.Account != nil && app.Scopes.Account.AuthHeaders != nil {
		authHeaders = app.Scopes.Account.AuthHeaders
	} else {
		authHeaders = app.Auth.Headers
	}
	for k, v := range buildHeaders(authHeaders, credentials) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2_000_000))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("discovery %s %s returned %d: %s", call.Method, url, resp.StatusCode, truncateStr(string(body), 200))
	}

	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("discovery response not JSON: %v", err)
	}

	// Walk to the project array. extractPath returns `raw` itself when
	// ResponsePath is empty, which handles the case where the array is
	// at the root (e.g. `GET /workspaces` returns `[...]`).
	if call.ResponsePath != "" {
		if m, ok := raw.(map[string]any); ok {
			raw = extractPath(m, call.ResponsePath)
		}
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("discovery response_path %q did not resolve to an array", call.ResponsePath)
	}

	out := make([]CachedProject, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := stringFromAny(m[call.IDField])
		if id == "" {
			continue
		}
		label := stringFromAny(m[call.LabelField])
		if label == "" {
			label = id
		}
		out = append(out, CachedProject{ID: id, Label: label})
	}
	return out, nil
}

func groupIDSafe(g *CredentialGroup) string {
	if g == nil {
		return ""
	}
	return g.ID
}

func stringFromAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
