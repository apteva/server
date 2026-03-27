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
	DeletePath  string                 `json:"delete_path,omitempty"`
	ManualSetup string                 `json:"manual_setup,omitempty"`
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
}

type AppToolDef struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	InputSchema  map[string]any `json:"input_schema"`
	ResponsePath *string        `json:"response_path,omitempty"`
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
	mu   sync.RWMutex
	apps map[string]*AppTemplate
}

func NewAppCatalog() *AppCatalog {
	return &AppCatalog{apps: make(map[string]*AppTemplate)}
}

func (c *AppCatalog) LoadFromDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

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
	}

	return nil
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
func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	var apps []AppSummary
	if q != "" {
		apps = s.catalog.Search(q)
	} else {
		apps = s.catalog.List()
	}
	if apps == nil {
		apps = []AppSummary{}
	}
	writeJSON(w, apps)
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

func resolveTemplate(template string, credentials map[string]string) string {
	result := template
	for key, val := range credentials {
		result = strings.ReplaceAll(result, "{{"+key+"}}", val)
	}
	// Fallback mappings
	if token, ok := credentials["access_token"]; ok {
		result = strings.ReplaceAll(result, "{{token}}", token)
	}
	if token, ok := credentials["bearer_token"]; ok {
		result = strings.ReplaceAll(result, "{{token}}", token)
	}
	if key, ok := credentials["api_key"]; ok {
		result = strings.ReplaceAll(result, "{{token}}", key)
		result = strings.ReplaceAll(result, "{{api_key}}", key)
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
