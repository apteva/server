package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// binaryMIMEPrefixes — kept in sync with integrations/src/http-executor.ts
// so the Go integration runner classifies the same response bodies as
// binary that the TS one does. Binary responses are base64-wrapped in
// the {_binary, base64, mimeType, size} envelope; everything else falls
// through to JSON parsing or stringification.
var binaryMIMEPrefixes = []string{
	"audio/",
	"video/",
	"image/",
	"application/octet-stream",
	"application/pdf",
	"application/zip",
	"application/gzip",
	"application/x-gzip",
	"application/x-tar",
	"application/vnd.openxmlformats",
	"application/vnd.ms-",
	"application/msword",
	"font/",
}

var integrationProxyEnvNameRE = regexp.MustCompile(`[^A-Z0-9]+`)

func isBinaryContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	for _, p := range binaryMIMEPrefixes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func decodeIntegrationJSON(raw []byte) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data any
	if err := decoder.Decode(&data); err != nil {
		return nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil
	}
	return data
}

func applyHeaderTransforms(headers map[string]string, transforms []HeaderTransformDef, input map[string]any) (map[string]bool, error) {
	localParams := make(map[string]bool)
	for _, transform := range transforms {
		if transform.StartParam != "" {
			localParams[transform.StartParam] = true
		}
		if transform.EndParam != "" {
			localParams[transform.EndParam] = true
		}
		if transform.Type != "byte_range" {
			return nil, fmt.Errorf("unsupported header transform %q", transform.Type)
		}

		startRaw, hasStart := nonEmptyInput(input, transform.StartParam)
		endRaw, hasEnd := nonEmptyInput(input, transform.EndParam)
		if !hasStart && !hasEnd {
			continue
		}
		if !hasStart {
			return nil, fmt.Errorf("%s requires %s", transform.EndParam, transform.StartParam)
		}
		start, err := nonNegativeInt64(startRaw, transform.StartParam)
		if err != nil {
			return nil, err
		}

		value := fmt.Sprintf("bytes=%d-", start)
		if hasEnd {
			end, err := nonNegativeInt64(endRaw, transform.EndParam)
			if err != nil {
				return nil, err
			}
			if end < start {
				return nil, fmt.Errorf("%s must be greater than or equal to %s", transform.EndParam, transform.StartParam)
			}
			value += strconv.FormatInt(end, 10)
		}
		header := transform.Header
		if header == "" {
			header = "Range"
		}
		headers[header] = value
	}
	return localParams, nil
}

func nonEmptyInput(input map[string]any, name string) (any, bool) {
	if name == "" {
		return nil, false
	}
	value, ok := input[name]
	if !ok || value == nil {
		return nil, false
	}
	if text, isString := value.(string); isString && text == "" {
		return nil, false
	}
	return value, true
}

func nonNegativeInt64(value any, name string) (int64, error) {
	var parsed int64
	var err error
	switch typed := value.(type) {
	case int:
		parsed = int64(typed)
	case int8:
		parsed = int64(typed)
	case int16:
		parsed = int64(typed)
	case int32:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		parsed = int64(typed)
	case uint8:
		parsed = int64(typed)
	case uint16:
		parsed = int64(typed)
	case uint32:
		parsed = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		parsed = int64(typed)
	case float32:
		number := float64(typed)
		if math.Trunc(number) != number || number > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		parsed = int64(number)
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		parsed = int64(typed)
	case json.Number:
		parsed, err = typed.Int64()
	case string:
		parsed, err = strconv.ParseInt(typed, 10, 64)
	default:
		err = fmt.Errorf("unsupported value")
	}
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func buildMultipartRequestBody(tool *AppToolDef, input map[string]any, credentials map[string]string, authBodyParams map[string]string) (io.Reader, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for k, v := range buildAuthBodyParams(authBodyParams, credentials) {
		if v == "" {
			continue
		}
		if err := writer.WriteField(k, multipartTextValue(v)); err != nil {
			return nil, "", fmt.Errorf("multipart auth field %q: %w", k, err)
		}
	}

	seenText := map[string]bool{}
	repeatText := map[string]bool{}
	for _, name := range tool.MultipartForm.RepeatFields {
		repeatText[name] = true
	}
	for _, name := range tool.MultipartForm.FieldNames {
		if seenText[name] {
			continue
		}
		seenText[name] = true
		v, ok := input[name]
		if !ok || v == nil {
			continue
		}
		if str, ok := v.(string); ok && str == "" {
			continue
		}
		values := []any{v}
		if repeatText[name] {
			values = multipartFileValues(v)
		}
		for _, value := range values {
			if err := writer.WriteField(name, multipartTextValue(value)); err != nil {
				return nil, "", fmt.Errorf("multipart field %q: %w", name, err)
			}
		}
	}

	for inputName, formName := range tool.MultipartForm.FileFields {
		if formName == "" {
			formName = inputName
		}
		v, ok := input[inputName]
		if !ok || v == nil {
			continue
		}
		values := multipartFileValues(v)
		for i, raw := range values {
			data, err := decodeMultipartFileValue(raw)
			if err != nil {
				return nil, "", fmt.Errorf("multipart file %q: %w", inputName, err)
			}
			filename := multipartFilename(input, inputName, i, len(values))
			part, err := writer.CreateFormFile(formName, filename)
			if err != nil {
				return nil, "", fmt.Errorf("multipart file field %q: %w", formName, err)
			}
			if _, err := part.Write(data); err != nil {
				return nil, "", fmt.Errorf("write multipart file %q: %w", formName, err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return &buf, writer.FormDataContentType(), nil
}

func multipartTextValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64, bool, int, int64:
		return fmt.Sprintf("%v", x)
	default:
		if data, err := json.Marshal(x); err == nil {
			return string(data)
		}
		return fmt.Sprintf("%v", x)
	}
}

func multipartFileValues(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, item)
		}
		return out
	default:
		return []any{x}
	}
}

func decodeMultipartFileValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		return decodeMultipartFileString(x), nil
	case map[string]any:
		if b64, ok := x["base64"].(string); ok {
			return decodeMultipartFileString(b64), nil
		}
		if b64, ok := x["data"].(string); ok {
			return decodeMultipartFileString(b64), nil
		}
		if text, ok := x["text"].(string); ok {
			return []byte(text), nil
		}
		data, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		return data, nil
	default:
		return []byte(fmt.Sprintf("%v", x)), nil
	}
}

func decodeMultipartFileString(s string) []byte {
	if idx := strings.Index(s, ","); strings.HasPrefix(s, "data:") && idx >= 0 {
		s = s[idx+1:]
	}
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return decoded
	}
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		return decoded
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return decoded
	}
	return []byte(s)
}

func multipartFilename(input map[string]any, inputName string, index, total int) string {
	for _, name := range []string{"filename", "fileName", inputName + "_filename", inputName + "Filename", inputName + "FileName"} {
		if v, ok := input[name].(string); ok && strings.TrimSpace(v) != "" {
			if total <= 1 {
				return v
			}
			return fmt.Sprintf("%d-%s", index+1, v)
		}
	}
	if total > 1 {
		return fmt.Sprintf("%s-%d.bin", inputName, index+1)
	}
	return inputName + ".bin"
}

func integrationProxyEnvName(slug string) string {
	normalized := strings.ToUpper(strings.TrimSpace(slug))
	normalized = integrationProxyEnvNameRE.ReplaceAllString(normalized, "_")
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		return ""
	}
	return "APTEVA_INTEGRATION_PROXY_" + normalized
}

func integrationProxyURL(app *AppTemplate) (string, string, error) {
	if app != nil {
		if name := integrationProxyEnvName(app.Slug); name != "" {
			if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
				if err := validateIntegrationProxyURL(raw); err != nil {
					return "", name, fmt.Errorf("invalid %s: %w", name, err)
				}
				return raw, name, nil
			}
		}
	}
	const fallback = "APTEVA_INTEGRATION_PROXY"
	if raw := strings.TrimSpace(os.Getenv(fallback)); raw != "" {
		if err := validateIntegrationProxyURL(raw); err != nil {
			return "", fallback, fmt.Errorf("invalid %s: %w", fallback, err)
		}
		return raw, fallback, nil
	}
	return "", "", nil
}

func validateIntegrationProxyURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be an absolute proxy URL")
	}
	return nil
}

func integrationHTTPClient(app *AppTemplate, credentials map[string]string, timeout time.Duration) (*http.Client, error) {
	proxyRaw, proxyEnv, err := integrationProxyURL(app)
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	if proxyRaw != "" {
		proxyURL, err := neturl.Parse(proxyRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", proxyEnv, err)
		}
		base.Proxy = http.ProxyURL(proxyURL)
	}
	if app.Auth.MTLS != nil {
		certField := strings.TrimSpace(app.Auth.MTLS.CertField)
		if certField == "" {
			certField = "client_certificate_pem"
		}
		keyField := strings.TrimSpace(app.Auth.MTLS.KeyField)
		if keyField == "" {
			keyField = "client_private_key_pem"
		}
		certPEM := normalizeCredentialPEM(credentials[certField])
		keyPEM := normalizeCredentialPEM(credentials[keyField])
		if certPEM != "" && keyPEM != "" {
			cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
			if err != nil {
				return nil, fmt.Errorf("mTLS certificate: %w", err)
			}
			cfg := base.TLSClientConfig
			if cfg == nil {
				cfg = &tls.Config{}
			} else {
				cfg = cfg.Clone()
			}
			cfg.Certificates = append(cfg.Certificates, cert)
			base.TLSClientConfig = cfg
		}
	}
	return &http.Client{Timeout: timeout, Transport: base}, nil
}

func normalizeCredentialPEM(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, `\n`, "\n"))
}

// --- DB Model ---

type Connection struct {
	ID                int64     `json:"id"`
	UserID            int64     `json:"user_id"`
	AppSlug           string    `json:"app_slug"`
	AppName           string    `json:"app_name"`
	Name              string    `json:"name"`
	AuthType          string    `json:"auth_type"`
	Status            string    `json:"status"`
	Source            string    `json:"source"`                // 'local' | 'composio'
	ProviderID        int64     `json:"provider_id,omitempty"` // FK → providers (for hosted sources)
	ExternalID        string    `json:"external_id,omitempty"` // composio connected_account_id, etc.
	ProjectID         string    `json:"project_id,omitempty"`
	CreatedVia        string    `json:"created_via,omitempty"`
	OwnerAppInstallID int64     `json:"owner_app_install_id,omitempty"`
	AutoMCP           bool      `json:"auto_mcp"`
	CreatedAt         time.Time `json:"created_at"`
}

// ConnectionInput carries the full set of fields for creating a connection via
// any source (local, composio, ...). Use this for new code paths; the legacy
// CreateConnection(...) helper below is kept so existing tests and mcp_gateway
// don't need to change.
// containsString returns true when needle appears in haystack.
// Tiny helper used by the auth-type selector — pulled out so the switch
// statement above stays readable.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

type ConnectionInput struct {
	UserID         int64
	AppSlug        string
	AppName        string
	Name           string
	AuthType       string
	EncryptedCreds string
	ProjectID      string
	Source         string // '' → 'local'
	Status         string // '' → 'active'
	ProviderID     int64
	ExternalID     string
	// CreatedVia distinguishes operator-driven integration installs
	// ('integration', auto-creates an MCP server row exposing the
	// integration's tools to agents) from app-driven creations
	// ('app_install', no auto-MCP — the app is the only intended
	// consumer). Empty defaults to 'integration' for back-compat.
	CreatedVia string
	// OwnerAppInstallID identifies which app install owns this
	// connection when CreatedVia='app_install'. Used by the platform
	// to scope DisconnectConnection callbacks (an app can only manage
	// connections it owns) and by the admin UI to filter app-owned
	// rows out of the operator's Integrations list. Zero for legacy /
	// operator-managed rows.
	OwnerAppInstallID int64
	// AutoMCP — when explicitly true, an mcp_servers row is auto-
	// created on connect so agents in the project can call the
	// integration's tools globally. **Default is false** — apps
	// creating connections via the SDK, composio backend, and other
	// programmatic paths typically just want a credential, not a
	// public tool surface. The dashboard's "Add integration" flow
	// passes auto_mcp=true explicitly so its UX is unchanged.
	// Pointer so absent vs. explicit-false is distinguishable at
	// the API boundary; nil means "use the default of false".
	AutoMCP *bool
}

// --- Store methods ---

// CreateConnection is the legacy helper — local-source, active status, no provider.
// Prefer CreateConnectionExt for new code.
func (s *Store) CreateConnection(userID int64, appSlug, appName, name, authType, encryptedCreds, projectID string) (*Connection, error) {
	return s.CreateConnectionExt(ConnectionInput{
		UserID: userID, AppSlug: appSlug, AppName: appName, Name: name,
		AuthType: authType, EncryptedCreds: encryptedCreds, ProjectID: projectID,
	})
}

func (s *Store) CreateConnectionExt(in ConnectionInput) (*Connection, error) {
	if in.Source == "" {
		in.Source = "local"
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if in.CreatedVia == "" {
		in.CreatedVia = "integration"
	}
	// Default OFF: callers that want tool exposure must opt in. The
	// dashboard's connect form sets AutoMCP=true explicitly; SDK +
	// composio + other programmatic paths leave it nil and get no
	// auto-MCP. This stops "every connection an app creates leaks
	// its tools as a global MCP server."
	autoMCP := 0
	if in.AutoMCP != nil && *in.AutoMCP {
		autoMCP = 1
	}
	result, err := s.db.Exec(
		"INSERT INTO connections (user_id, app_slug, app_name, name, auth_type, encrypted_credentials, status, project_id, source, provider_id, external_id, created_via, owner_app_install_id, auto_mcp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.UserID, in.AppSlug, in.AppName, in.Name, in.AuthType, in.EncryptedCreds, in.Status, in.ProjectID, in.Source, in.ProviderID, in.ExternalID, in.CreatedVia, in.OwnerAppInstallID, autoMCP,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Connection{
		ID: id, UserID: in.UserID, AppSlug: in.AppSlug, AppName: in.AppName, Name: in.Name,
		AuthType: in.AuthType, Status: in.Status, Source: in.Source, ProviderID: in.ProviderID,
		ExternalID: in.ExternalID, ProjectID: in.ProjectID, CreatedVia: in.CreatedVia,
		OwnerAppInstallID: in.OwnerAppInstallID, AutoMCP: autoMCP != 0, CreatedAt: time.Now(),
	}, nil
}

func isAppOwnedConnection(c Connection) bool {
	return c.CreatedVia == "app_install" || c.OwnerAppInstallID != 0
}

func (s *Store) ListConnections(userID int64, projectID ...string) ([]Connection, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		// "Visible from project X" = X-scoped rows + global rows
		// (project_id = ''). The global tier was creatable only via
		// the API pre-v0.15.0, so this OR is additive — existing
		// installs without global connections see the same result
		// set. With the v0.15.0 UI, an operator who promotes a
		// connection to global gets it appearing in every project's
		// list, which is the whole point.
		rows, err = s.db.Query(
			`SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''),
			        COALESCE(created_via,'integration'), COALESCE(owner_app_install_id,0), COALESCE(auto_mcp,1), created_at
			 FROM connections WHERE user_id = ? AND (project_id = ? OR project_id = '')`, userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			`SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''),
			        COALESCE(created_via,'integration'), COALESCE(owner_app_install_id,0), COALESCE(auto_mcp,1), created_at
			 FROM connections WHERE user_id = ?`, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conns []Connection
	for rows.Next() {
		var c Connection
		var createdAt string
		var autoMCP int
		rows.Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &c.Status, &c.Source, &c.ProviderID, &c.ExternalID, &c.ProjectID, &c.CreatedVia, &c.OwnerAppInstallID, &autoMCP, &createdAt)
		c.UserID = userID
		c.AutoMCP = autoMCP != 0
		c.CreatedAt, _ = parseTime(createdAt)
		conns = append(conns, c)
	}
	return conns, nil
}

func (s *Store) GetConnection(userID, connID int64) (*Connection, string, error) {
	var c Connection
	var encCreds, createdAt string
	var autoMCP int
	err := s.db.QueryRow(
		`SELECT id, app_slug, app_name, name, auth_type, encrypted_credentials, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''),
		        COALESCE(created_via,'integration'), COALESCE(owner_app_install_id,0), COALESCE(auto_mcp,1), created_at
		 FROM connections WHERE id = ? AND user_id = ?`,
		connID, userID,
	).Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &encCreds, &c.Status, &c.Source, &c.ProviderID, &c.ExternalID, &c.ProjectID, &c.CreatedVia, &c.OwnerAppInstallID, &autoMCP, &createdAt)
	if err != nil {
		return nil, "", err
	}
	c.UserID = userID
	c.AutoMCP = autoMCP != 0
	c.CreatedAt, _ = parseTime(createdAt)
	return &c, encCreds, nil
}

// UpdateConnectionStatus flips a connection's status (pending → active → failed).
func (s *Store) UpdateConnectionStatus(connID int64, status string) error {
	_, err := s.db.Exec("UPDATE connections SET status = ? WHERE id = ?", status, connID)
	return err
}

// UpdateConnectionCredentials replaces the encrypted credential blob (used after
// local OAuth token exchange and on refresh).
func (s *Store) UpdateConnectionCredentials(connID int64, encryptedCreds string) error {
	_, err := s.db.Exec("UPDATE connections SET encrypted_credentials = ? WHERE id = ?", encryptedCreds, connID)
	return err
}

// MarkLegacyCJDropshippingConnectionsForReconnect invalidates connections
// created under the old manual-access-token catalog contract. The replacement
// contract stores the user's durable CJ API key and exchanges it for access
// tokens automatically. New connections already contain api_key and are left
// untouched, making this safe to run on every boot.
func (s *Store) MarkLegacyCJDropshippingConnectionsForReconnect(secret []byte) (int, error) {
	rows, err := s.db.Query(`
		SELECT id, encrypted_credentials
		FROM connections
		WHERE app_slug = 'cjdropshipping' AND status != 'failed'`)
	if err != nil {
		return 0, err
	}
	var legacyIDs []int64
	for rows.Next() {
		var id int64
		var encrypted string
		if err := rows.Scan(&id, &encrypted); err != nil {
			rows.Close()
			return 0, err
		}
		plain, err := Decrypt(secret, encrypted)
		if err != nil {
			continue
		}
		credentials := map[string]string{}
		if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
			continue
		}
		if strings.TrimSpace(credentials["api_key"]) != "" {
			continue
		}
		if strings.TrimSpace(credentials["token"]) != "" ||
			strings.TrimSpace(credentials["access_token"]) != "" ||
			strings.TrimSpace(credentials["accessToken"]) != "" {
			legacyIDs = append(legacyIDs, id)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range legacyIDs {
		if _, err := s.db.Exec("UPDATE connections SET status = 'failed' WHERE id = ?", id); err != nil {
			return 0, err
		}
	}
	return len(legacyIDs), nil
}

// UpdateMCPServerEnv replaces the encrypted_env blob for a single
// mcp_servers row. Used by the OAuth refresh path: when an upstream
// hosted MCP returns 401 because the access token expired, we
// refresh the token on the source connection AND need to reflect it
// in the mcp_servers row that probeRemoteMCP / callRemoteMCPTool
// read at call time. Without this, the row's frozen env would keep
// failing every call until the user manually disconnect/reconnects.
func (s *Store) UpdateMCPServerEnv(serverID int64, encryptedEnv string) error {
	_, err := s.db.Exec("UPDATE mcp_servers SET encrypted_env = ? WHERE id = ?", encryptedEnv, serverID)
	return err
}

func (s *Store) DeleteConnection(userID, connID int64) error {
	_, err := s.db.Exec("DELETE FROM connections WHERE id = ? AND user_id = ?", connID, userID)
	return err
}

// CreateMCPServerFromConnection creates an MCP server entry for a local
// integration. allowedTools is optional — pass nil or empty for "all tools
// exposed" (legacy behaviour). A populated list scopes the resulting MCP
// server row to that subset, enforced by handleMCPEndpoint on every request.
//
// `name` is the CANONICAL SLUG (e.g. "omnikit-storage"), not the human
// display name. The slug is what shows up everywhere downstream:
//   - Entry name in the instance's config.json
//   - Prefix in the system prompt's [AVAILABLE MCP SERVERS] block
//   - Prefix of tool names registered with core ("omnikit-storage_get_file")
//   - Exact-match key when a sub-thread looks up an MCP by name at spawn
//     time (core/thread.go does string equality there)
//
// The display name (conn.AppName, e.g. "OmniKit Storage") goes into the
// description so the dashboard can still show it, but the canonical name
// is the slug. Mixing them was the bug behind "spawn(mcp=\"omnikit-storage\")
// silently produces a worker with zero tools" — the LLM used the slug (which
// it inferred from tool prefixes) but the config stored the display name, so
// the lookup failed.
func (s *Store) CreateMCPServerFromConnection(userID int64, conn *Connection, toolCount int, allowedTools ...[]string) (int64, error) {
	return s.CreateMCPServerFromConnectionWithSlug(userID, conn, toolCount, "", allowedTools...)
}

// CreateMCPServerFromConnectionWithSlug is the explicit-base variant.
// Pass `slugBase` to override the name derivation — required for
// suite fan-outs where the connection Name is just the project label
// (e.g. "Real Estate") and the MCP slug needs to encode the service
// too (e.g. "omnikit-storage-real-estate") so tool prefixes stay
// distinct when the same project hosts several services.
func (s *Store) CreateMCPServerFromConnectionWithSlug(userID int64, conn *Connection, toolCount int, slugBase string, allowedTools ...[]string) (int64, error) {
	var allowedJSON string
	if len(allowedTools) > 0 && len(allowedTools[0]) > 0 {
		b, _ := json.Marshal(allowedTools[0])
		allowedJSON = string(b)
	}
	// Pick a unique slug for this MCP row. Explicit slugBase wins;
	// otherwise fall back to the user-chosen integration name — if
	// they typed "mybusiness-socialcast" on create, sub-threads
	// reference it as `mcp="mybusiness-socialcast"` and tool-name
	// prefixes come from that slug (e.g. `mybusiness-socialcast_post`).
	// We slugify whatever we get rather than accepting it verbatim so
	// the result stays safe for prompts and downstream consumers that
	// treat it as an identifier.
	base := slugify(slugBase)
	if base == "" {
		base = slugify(conn.Name)
	}
	if base == "" {
		base = conn.AppSlug
	}
	mcpName := s.uniqueMCPName(userID, conn.ProjectID, base, conn.ID)
	// Description is what the dashboard renders as the row's headline.
	// We want the suite + service context to read at a glance — e.g.
	// "OmniKit Messaging M" rather than bare "M" — so prepend the
	// app's display name when the connection name doesn't already
	// carry it. Suite fan-outs save the connection as just the project
	// label (sel.Label), which is the case this branch fixes; legacy
	// single-app connections where the user typed "OmniKit Messaging
	// work" stay as-is because HasPrefix catches the duplication.
	description := conn.Name
	if description == "" {
		description = conn.AppName
	} else if conn.AppName != "" && !strings.HasPrefix(description, conn.AppName) {
		description = conn.AppName + " " + description
	}
	result, err := s.db.Exec(
		"INSERT INTO mcp_servers (user_id, name, description, status, tool_count, source, connection_id, project_id, allowed_tools) VALUES (?, ?, ?, 'running', ?, 'local', ?, ?, ?)",
		userID, mcpName, description, toolCount, conn.ID, conn.ProjectID, allowedJSON,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// createRemoteMcpFromConnection writes the mcp_servers row for a
// kind=remote_mcp app connection. Mirrors CreateMCPServerFromConnection
// for legacy REST integrations, but instead of pointing the row at the
// per-server proxy URL, it points at the vendor's hosted MCP and stores
// the resolved auth header in encrypted_env so MCPManager.Start can
// supply it on every probe + forwarded call (probeRemoteMCP reads
// env["AUTHORIZATION"] / env["API_KEY"]).
//
// `encCreds` is the same encrypted credentials blob GetConnection
// returns — kept as the parameter shape so callers don't have to
// re-fetch from the DB. The function decrypts, resolves the
// auth-header template ({{token}} / {{api_key}}), and re-encrypts the
// derived env JSON. Returns the new mcp_servers row id.
func (s *Server) createRemoteMcpFromConnection(userID int64, conn *Connection, app *AppTemplate, encCreds string) (int64, error) {
	log.Printf("[REMOTE-MCP] enter conn=%d slug=%s user=%d project=%q name=%q", conn.ID, conn.AppSlug, userID, conn.ProjectID, conn.Name)
	if app == nil || app.Kind != "remote_mcp" {
		return 0, fmt.Errorf("createRemoteMcpFromConnection requires kind=remote_mcp, got %q", app.Kind)
	}
	if app.MCP == nil || app.MCP.URL == "" {
		return 0, fmt.Errorf("app %s missing mcp.url", app.Slug)
	}
	log.Printf("[REMOTE-MCP] app config: url=%s transport=%s auth_header_set=%t", app.MCP.URL, app.MCP.Transport, app.MCP.AuthHeader != nil)

	// Default header pattern matches the TS generator: Authorization:
	// Bearer {{token}}. Templates can override per-app for vendors that
	// expect a different header (e.g. X-Auth-Token) or value layout.
	headerName := "Authorization"
	headerValue := "Bearer {{token}}"
	if app.MCP.AuthHeader != nil {
		if app.MCP.AuthHeader.Name != "" {
			headerName = app.MCP.AuthHeader.Name
		}
		if app.MCP.AuthHeader.Value != "" {
			headerValue = app.MCP.AuthHeader.Value
		}
	}
	log.Printf("[REMOTE-MCP] header template: %s = %q", headerName, headerValue)

	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		log.Printf("[REMOTE-MCP] decrypt creds FAILED conn=%d: %v", conn.ID, err)
		return 0, fmt.Errorf("decrypt creds: %w", err)
	}
	var credentials map[string]string
	if uerr := json.Unmarshal([]byte(plain), &credentials); uerr != nil {
		log.Printf("[REMOTE-MCP] parse creds FAILED conn=%d: %v", conn.ID, uerr)
		return 0, fmt.Errorf("parse creds: %w", uerr)
	}
	credKeys := make([]string, 0, len(credentials))
	for k := range credentials {
		credKeys = append(credKeys, k)
	}
	log.Printf("[REMOTE-MCP] cred keys available: %v", credKeys)

	resolved, ok := resolveRemoteMcpCredTemplate(headerValue, credentials)
	if !ok {
		log.Printf("[REMOTE-MCP] template resolve FAILED — placeholder unresolved in %q against keys %v", headerValue, credKeys)
		// At least one {{name}} placeholder did not resolve to a real
		// credential value. Fail loud — the upstream MCP would reject
		// the call anyway, and a silent "this connection is broken"
		// row is worse than a startup error operators can see.
		return 0, fmt.Errorf("could not resolve auth header %q from connection credentials", headerValue)
	}
	// Show length only — never log the resolved bearer value itself.
	log.Printf("[REMOTE-MCP] template resolved OK len=%d prefix=%s", len(resolved), shortPrefix(resolved, 12))

	// probeRemoteMCP / MCPManager.Start read env keys case-sensitively
	// for "AUTHORIZATION" and "API_KEY". Normalize standard headers to
	// those keys; pass through any other header verbatim under its
	// upper-cased name so the proxy can apply it generically.
	envKey := strings.ToUpper(headerName)
	if envKey == "AUTHORIZATION" || envKey == "API_KEY" || envKey == "X-API-KEY" {
		// X-Api-Key ends up under API_KEY for the existing branch.
		if envKey == "X-API-KEY" {
			envKey = "API_KEY"
		}
	}
	envMap := map[string]string{envKey: resolved}
	envJSON, _ := json.Marshal(envMap)
	encEnv, err := Encrypt(s.secret, string(envJSON))
	if err != nil {
		log.Printf("[REMOTE-MCP] encrypt env FAILED conn=%d: %v", conn.ID, err)
		return 0, fmt.Errorf("encrypt env: %w", err)
	}
	log.Printf("[REMOTE-MCP] env prepared envKey=%s encrypted_len=%d", envKey, len(encEnv))

	// Stable upstream_id keyed on the connection so a retry / re-OAuth
	// updates the same row instead of multiplying entries (matches
	// Composio's discipline).
	upstreamID := fmt.Sprintf("remote_mcp:%d", conn.ID)

	// Dedup against the connection_id — if this is a re-run after
	// re-OAuth, drop the prior row first so the insert below produces a
	// single, fresh entry with the new env.
	s.store.DeleteMCPServerByConnection(conn.ID)
	log.Printf("[REMOTE-MCP] cleared any prior mcp_servers row for conn=%d", conn.ID)

	base := slugify(conn.Name)
	if base == "" {
		base = conn.AppSlug
	}
	mcpName := s.store.uniqueMCPName(userID, conn.ProjectID, base, conn.ID)

	description := conn.Name
	if description == "" {
		description = conn.AppName
	} else if conn.AppName != "" && !strings.HasPrefix(description, conn.AppName) {
		description = conn.AppName + " " + description
	}
	log.Printf("[REMOTE-MCP] inserting mcp_servers row name=%s desc=%q upstream_id=%s url=%s", mcpName, description, upstreamID, app.MCP.URL)

	rec, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID:       userID,
		Name:         mcpName,
		Description:  description,
		ProjectID:    conn.ProjectID,
		Source:       "remote",
		Transport:    "http",
		URL:          app.MCP.URL,
		ConnectionID: conn.ID,
		EncryptedEnv: encEnv,
		UpstreamID:   upstreamID,
	})
	if err != nil {
		log.Printf("[REMOTE-MCP] CreateMCPServerExt FAILED conn=%d: %v", conn.ID, err)
		return 0, err
	}
	log.Printf("[REMOTE-MCP] mcp_servers row created id=%d conn=%d slug=%s", rec.ID, conn.ID, conn.AppSlug)
	return rec.ID, nil
}

// refreshRemoteMcpAuth runs the OAuth refresh-token flow for the
// connection backing a kind=remote_mcp mcp_servers row, persists the
// new tokens on the connection, AND rewrites the mcp_servers row's
// encrypted_env so the next callRemoteMCPTool / probeRemoteMCP picks
// up the fresh access token. Mutates the in-memory env map in place
// so the caller can retry the upstream call without a re-fetch.
//
// Triggered by the 401-retry branch in handleCallMCPTool. HubSpot's
// access tokens expire after 30 minutes; without this, every demo
// session breaks the moment the user steps away long enough.
func (s *Server) refreshRemoteMcpAuth(serverID int64, connectionID int64, env map[string]string) error {
	if connectionID == 0 {
		return fmt.Errorf("no connection_id on mcp_servers row %d", serverID)
	}
	// Need both the connection (for user_id, app_slug, encrypted creds)
	// and the catalog entry (for token_url + auth_header template).
	var userID int64
	var appSlug string
	if err := s.store.db.QueryRow(
		"SELECT user_id, app_slug FROM connections WHERE id = ?", connectionID,
	).Scan(&userID, &appSlug); err != nil {
		return fmt.Errorf("load connection %d: %w", connectionID, err)
	}
	conn, encCreds, err := s.store.GetConnection(userID, connectionID)
	if err != nil {
		return fmt.Errorf("get connection %d: %w", connectionID, err)
	}
	app := s.catalog.Get(appSlug)
	if app == nil {
		return fmt.Errorf("app %s missing from catalog", appSlug)
	}
	if app.Kind != "remote_mcp" || app.MCP == nil {
		return fmt.Errorf("app %s is not kind=remote_mcp", appSlug)
	}

	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return fmt.Errorf("decrypt creds: %w", err)
	}
	var credentials map[string]string
	if uerr := json.Unmarshal([]byte(plain), &credentials); uerr != nil {
		return fmt.Errorf("parse creds: %w", uerr)
	}
	log.Printf("[REMOTE-MCP-REFRESH] conn=%d slug=%s — running token refresh", connectionID, appSlug)
	if rerr := refreshOAuthAccessToken(app, credentials); rerr != nil {
		return fmt.Errorf("refresh token: %w", rerr)
	}

	// Persist new tokens on the connection so they survive restarts.
	newCredsJSON, _ := json.Marshal(credentials)
	newEncCreds, err := Encrypt(s.secret, string(newCredsJSON))
	if err != nil {
		return fmt.Errorf("encrypt new creds: %w", err)
	}
	if err := s.store.UpdateConnectionCredentials(connectionID, newEncCreds); err != nil {
		return fmt.Errorf("persist refreshed conn creds: %w", err)
	}

	// Re-resolve the auth header template against the refreshed
	// credentials and rewrite the mcp_servers row's env so the next
	// upstream call picks up the new access token.
	headerName := "Authorization"
	headerValue := "Bearer {{token}}"
	if app.MCP.AuthHeader != nil {
		if app.MCP.AuthHeader.Name != "" {
			headerName = app.MCP.AuthHeader.Name
		}
		if app.MCP.AuthHeader.Value != "" {
			headerValue = app.MCP.AuthHeader.Value
		}
	}
	resolved, ok := resolveRemoteMcpCredTemplate(headerValue, credentials)
	if !ok {
		return fmt.Errorf("template resolve failed after refresh: %q", headerValue)
	}
	envKey := strings.ToUpper(headerName)
	if envKey == "X-API-KEY" {
		envKey = "API_KEY"
	}
	newEnvMap := map[string]string{envKey: resolved}
	envJSON, _ := json.Marshal(newEnvMap)
	encEnv, err := Encrypt(s.secret, string(envJSON))
	if err != nil {
		return fmt.Errorf("encrypt new env: %w", err)
	}
	if err := s.store.UpdateMCPServerEnv(serverID, encEnv); err != nil {
		return fmt.Errorf("persist refreshed mcp env: %w", err)
	}

	// Mutate the caller's env map so the immediate retry uses the
	// refreshed value without going back through the DB.
	for k := range env {
		delete(env, k)
	}
	for k, v := range newEnvMap {
		env[k] = v
	}
	log.Printf("[REMOTE-MCP-REFRESH] OK conn=%d server=%d — new bearer length=%d", connectionID, serverID, len(resolved))
	_ = conn // silence unused var; conn fields not needed for refresh itself
	return nil
}

// shortPrefix returns up to `n` characters from the start of s with a
// length stamp. Used in debug logs to give a sanity check that a
// resolved bearer header looks plausible without leaking the token.
func shortPrefix(s string, n int) string {
	if len(s) <= n {
		return fmt.Sprintf("%q(len=%d)", s, len(s))
	}
	return fmt.Sprintf("%q…(len=%d)", s[:n], len(s))
}

// resolveRemoteMcpCredTemplate substitutes {{name}} placeholders in an
// auth-header template against a flat credential map. Mirrors the
// alias logic in @apteva/integrations/src/mcp-generator.ts so an app
// declaring `Bearer {{token}}` works whether the connection stored
// the OAuth result under `token`, `access_token`, or `bearer_token`.
//
// Returns (resolvedString, allPlaceholdersMatched). The boolean is
// false if ANY placeholder resolved to an empty string — the caller
// should reject the connection rather than persist a half-substituted
// header (which the upstream would 401 anyway, with no clue why).
func resolveRemoteMcpCredTemplate(template string, creds map[string]string) (string, bool) {
	pick := func(names ...string) string {
		for _, n := range names {
			if v, ok := creds[n]; ok && v != "" {
				return v
			}
		}
		return ""
	}
	allOK := true
	out := template
	for {
		i := strings.Index(out, "{{")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "}}")
		if j < 0 {
			break
		}
		key := out[i+2 : i+j]
		var val string
		switch key {
		case "token":
			val = pick("token", "access_token", "bearer_token", "api_key")
		case "api_key":
			val = pick("api_key", "token")
		default:
			val = creds[key]
		}
		if val == "" {
			allOK = false
		}
		out = out[:i] + val + out[i+j+2:]
	}
	return out, allOK
}

// CanonicalMCPNameForConnection returns the canonical MCP server name used
// as the tool-name prefix for a connection. It prefers the default (non-
// scoped) mcp_servers row bound to that connection; if none is found (e.g.
// the default row was deleted), it falls back to the app slug. This is the
// string every dashboard-facing "list tools for connection X" path should
// use to prefix bare tool names so agents can tell two connections for the
// same app apart.
func (s *Store) CanonicalMCPNameForConnection(connID int64) string {
	var name string
	// Prefer the oldest row (= auto-created default) over scoped copies.
	s.db.QueryRow(
		"SELECT name FROM mcp_servers WHERE connection_id = ? ORDER BY id ASC LIMIT 1",
		connID,
	).Scan(&name)
	if name != "" {
		return name
	}
	// Fallback: look up the connection's app slug directly.
	var slug string
	s.db.QueryRow("SELECT app_slug FROM connections WHERE id = ?", connID).Scan(&slug)
	return slug
}

// uniqueMCPName returns a per-project unique MCP server name for the given
// app slug. First connection of the app in the project keeps the bare slug
// (backward-compat with existing scenarios + tool-prefix expectations);
// any subsequent connection gets `${slug}-${connID}`, with a counter
// appended if that's also already taken (can happen when a legacy row was
// renamed by migration to exactly the suffix we'd otherwise generate).
// slugify collapses a human-friendly label into a lowercase identifier
// safe to use as an MCP name. Keeps letters, digits, dot, dash, and
// underscore; everything else (spaces, punctuation, emoji) becomes a
// single dash. Leading / trailing / doubled dashes are trimmed.
//
// Examples:
//
//	"MyBusiness — SocialCast"  → "mybusiness-socialcast"
//	"Acme / Gmail Inbox"        → "acme-gmail-inbox"
//	"  "                        → ""
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	return out
}

func (s *Store) uniqueMCPName(userID int64, projectID, appSlug string, connID int64) string {
	nameTaken := func(candidate string) bool {
		var count int
		s.db.QueryRow(
			"SELECT COUNT(*) FROM mcp_servers WHERE user_id = ? AND project_id = ? AND name = ?",
			userID, projectID, candidate,
		).Scan(&count)
		return count > 0
	}
	if !nameTaken(appSlug) {
		return appSlug
	}
	base := fmt.Sprintf("%s-%d", appSlug, connID)
	if !nameTaken(base) {
		return base
	}
	// Walk a counter until we land on a free name. Bounded to 1000 to
	// avoid an infinite loop if the DB is in an unexpected state.
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !nameTaken(candidate) {
			return candidate
		}
	}
	return base // caller will still fail the insert — better than hanging
}

func (s *Store) DeleteMCPServerByConnection(connID int64) {
	s.db.Exec("DELETE FROM mcp_servers WHERE connection_id = ?", connID)
}

// --- HTTP Executor ---

type ExecuteResult struct {
	Success bool              `json:"success"`
	Status  int               `json:"status"`
	Data    any               `json:"data"`
	Headers map[string]string `json:"headers,omitempty"`
}

// forwardableHeaders is the allowlist of upstream response headers we
// surface to apps via ExecuteResult.Headers. Kept deliberately small —
// most apps only need this to pick up redirect-style metadata from
// flows like YouTube's resumable-upload init (which returns the session
// URL only via Location). Add headers here when a real use case shows
// up; do not blanket-forward, since arbitrary headers can include
// Set-Cookie, X-API-Key echoes, debug info, etc. that apps shouldn't
// see.
var forwardableHeaders = []string{
	"Location",
	"Content-Type",
	"Content-Range",
	"Accept-Ranges",
	"Etag",
	"Last-Modified",
	"Content-Length",
	"Request-Id",
	"X-Request-Id",
	"Apns-Id",
	"X-ElevenLabs-Request-Id",
	"Xi-Request-Id",
}

// pickForwardableHeaders extracts the allowlisted header values from a
// response. Lower-cased keys (HTTP/2 normalises to lowercase, http.Header
// canonicalises on access) — we use the canonical form on output.
func pickForwardableHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(forwardableHeaders))
	for _, name := range forwardableHeaders {
		if v := h.Get(name); v != "" {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// onCredsRefresh is the optional callback executeIntegrationTool invokes
// when it auto-refreshes an OAuth2 access token. Callers wire it to write
// the new credentials map back to the DB so the refreshed tokens survive
// process restarts. Pass nil to skip persistence (e.g. dry-run / tests).
type onCredsRefresh func(updated map[string]string) error

// executeIntegrationToolWithRefresh wraps executeIntegrationTool with an
// auto-refresh + retry-once loop on HTTP 401. The credentials map is
// mutated in place when a refresh succeeds, and the optional onRefresh
// callback fires so the caller can persist the new tokens.
//
// Refresh fires only when:
//  1. The HTTP response status is 401 (Unauthorized)
//  2. The app has an OAuth2 config (so we know the token endpoint)
//  3. The credentials map contains a refresh_token (or refreshToken)
//
// All other failure modes (network, 4xx other than 401, 5xx) bubble up
// unchanged. Refresh failures are non-fatal — we return the original 401
// so the caller can surface the auth error to the user.
func executeIntegrationToolWithRefresh(
	app *AppTemplate,
	tool *AppToolDef,
	credentials map[string]string,
	input map[string]any,
	environmentID string,
	onRefresh onCredsRefresh,
) (*ExecuteResult, error) {
	if app != nil && app.Auth.TokenExchange != nil &&
		(environmentID == "" || environmentIntegrationMode(environmentID) == IntegrationModeReal) {
		changed, err := ensureCredentialExchangeToken(app, credentials, false)
		if err != nil {
			return nil, err
		}
		if changed && onRefresh != nil {
			if err := onRefresh(credentials); err != nil {
				fmt.Fprintf(os.Stderr, "[token-exchange] persist failed for %s: %v\n", app.Slug, err)
			}
		}
	}
	if app != nil && app.Slug == integrationOpenAICodexSlug && connectionOpenAICodexNeedsRefresh(credentials, 10*time.Minute) {
		if err := refreshIntegrationOpenAICodexCredentials(credentials); err == nil && onRefresh != nil {
			if perr := onRefresh(credentials); perr != nil {
				fmt.Fprintf(os.Stderr, "[codex-refresh] persist failed for %s: %v\n", app.Slug, perr)
			}
		}
	}
	result, err := executeIntegrationTool(app, tool, credentials, input, environmentID)
	if err != nil {
		return result, err
	}
	if app != nil && app.Slug == integrationOpenAICodexSlug && (result.Status == 401 || result.Status == 403) {
		if err := refreshIntegrationOpenAICodexCredentials(credentials); err != nil {
			fmt.Fprintf(os.Stderr, "[codex-refresh] %s: %v\n", app.Slug, err)
			return result, nil
		}
		if onRefresh != nil {
			if err := onRefresh(credentials); err != nil {
				fmt.Fprintf(os.Stderr, "[codex-refresh] persist failed for %s: %v\n", app.Slug, err)
			}
		}
		return executeIntegrationTool(app, tool, credentials, input, environmentID)
	}
	if result.Status != 401 {
		return result, nil
	}
	if app != nil && app.Auth.TokenExchange != nil {
		changed, exchangeErr := ensureCredentialExchangeToken(app, credentials, true)
		if exchangeErr != nil {
			fmt.Fprintf(os.Stderr, "[token-exchange] %s: %v\n", app.Slug, exchangeErr)
			return result, nil
		}
		if changed && onRefresh != nil {
			if err := onRefresh(credentials); err != nil {
				fmt.Fprintf(os.Stderr, "[token-exchange] persist failed for %s: %v\n", app.Slug, err)
			}
		}
		return executeIntegrationTool(app, tool, credentials, input, environmentID)
	}
	// 401 — try to refresh and retry once.
	if app.Auth.OAuth2 == nil {
		return result, nil
	}
	rt := credentials["refresh_token"]
	if rt == "" {
		rt = credentials["refreshToken"]
	}
	if rt == "" {
		return result, nil
	}
	if err := refreshOAuthAccessToken(app, credentials); err != nil {
		// Refresh failed — surface the original 401 so the caller knows
		// the connection needs manual re-auth. Log so the operator can
		// see why refresh isn't working (likely revoked refresh token,
		// missing client_id/secret, or upstream provider error).
		fmt.Fprintf(os.Stderr, "[oauth-refresh] %s: %v\n", app.Slug, err)
		return result, nil
	}
	// Persist the refreshed tokens before retrying so a crash mid-retry
	// doesn't lose them.
	if onRefresh != nil {
		if err := onRefresh(credentials); err != nil {
			fmt.Fprintf(os.Stderr, "[oauth-refresh] persist failed for %s: %v\n", app.Slug, err)
			// Don't bail — the refreshed token still works in this
			// process, we just lose it on restart. Better than a hard
			// failure on what was a successful refresh.
		}
	}
	// Retry the original call with the refreshed token. executeIntegrationTool
	// reads from the same credentials map so the new token is picked up.
	return executeIntegrationTool(app, tool, credentials, input, environmentID)
}

func ensureCredentialExchangeToken(app *AppTemplate, credentials map[string]string, force bool) (bool, error) {
	cfg := app.Auth.TokenExchange
	if cfg == nil || cfg.URL == "" {
		return false, nil
	}
	skew := time.Duration(cfg.ExpirySkewSeconds) * time.Second
	if skew <= 0 {
		skew = time.Minute
	}
	if !force && credentials["access_token"] != "" {
		if expiresAt, err := time.Parse(time.RFC3339, credentials["token_expires_at"]); err == nil &&
			expiresAt.After(time.Now().Add(skew)) {
			return false, nil
		}
	}

	values := map[string]string{}
	for key, template := range cfg.BodyParams {
		if value := resolveTemplate(template, credentials); value != "" {
			values[key] = value
		}
	}
	contentType := cfg.ContentType
	if contentType == "" {
		contentType = "application/x-www-form-urlencoded"
	}
	var body io.Reader
	if contentType == "application/json" {
		raw, err := json.Marshal(values)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(raw)
	} else {
		form := neturl.Values{}
		for key, value := range values {
			form.Set(key, value)
		}
		body = strings.NewReader(form.Encode())
	}

	method := cfg.Method
	if method == "" {
		method = http.MethodPost
	}
	exchangeURL := resolveTemplate(cfg.URL, credentials)
	req, err := http.NewRequest(method, exchangeURL, body)
	if err != nil {
		return false, credentialExchangeError(app, credentials, nil, "credential token exchange request is invalid: "+err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	for key, template := range cfg.Headers {
		if value := resolveTemplate(template, credentials); value != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return false, credentialExchangeError(app, credentials, nil, "credential token exchange request failed: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var response map[string]any
		_ = json.Unmarshal(raw, &response)
		return false, credentialExchangeError(app, credentials, response,
			fmt.Sprintf("credential token exchange failed (HTTP %d)", resp.StatusCode))
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false, credentialExchangeError(app, credentials, nil, "credential token exchange returned invalid JSON")
	}
	tokenPath := cfg.AccessTokenPath
	if tokenPath == "" {
		tokenPath = "access_token"
	}
	root, _ := decoded.(map[string]any)
	token := fmt.Sprint(extractPath(root, tokenPath))
	if token == "" || token == "<nil>" {
		return false, credentialExchangeError(app, credentials, root, "credential token exchange returned no access token")
	}
	credentials["access_token"] = token
	credentials["accessToken"] = token
	credentials["token"] = token

	now := time.Now()
	var expiresAt time.Time
	if cfg.ExpiresAtPath != "" {
		rawExpiresAt := strings.TrimSpace(fmt.Sprint(extractPath(root, cfg.ExpiresAtPath)))
		if rawExpiresAt != "" && rawExpiresAt != "<nil>" {
			if parsed, err := time.Parse(time.RFC3339, rawExpiresAt); err == nil {
				expiresAt = parsed
			}
		}
	}
	if expiresAt.IsZero() {
		expiresPath := cfg.ExpiresInPath
		if expiresPath == "" {
			expiresPath = "expires_in"
		}
		expiresIn, _ := strconv.ParseFloat(fmt.Sprint(extractPath(root, expiresPath)), 64)
		if expiresIn > 0 {
			expiresAt = now.Add(time.Duration(expiresIn * float64(time.Second)))
		}
	}
	if expiresAt.IsZero() {
		expiresAt = now.Add(5 * time.Minute)
	}
	credentials["token_expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	return true, nil
}

func credentialExchangeError(app *AppTemplate, credentials map[string]string, response map[string]any, fallback string) error {
	name := "Integration"
	if app != nil {
		if strings.TrimSpace(app.Name) != "" {
			name = strings.TrimSpace(app.Name)
		} else if strings.TrimSpace(app.Slug) != "" {
			name = strings.TrimSpace(app.Slug)
		}
	}
	message := ""
	if response != nil {
		message = strings.TrimSpace(fmt.Sprint(response["message"]))
		if message == "<nil>" || strings.EqualFold(message, "success") {
			message = ""
		}
	}
	if message == "" {
		message = fallback
	}
	for _, secret := range credentials {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 4 {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return errors.New(name + ": " + message)
}

func addQueryValue(q neturl.Values, key string, v any) {
	if v == nil {
		return
	}
	switch vv := v.(type) {
	case []any:
		for _, item := range vv {
			if item != nil {
				q.Add(key, fmt.Sprintf("%v", item))
			}
		}
	case []string:
		for _, item := range vv {
			if item != "" {
				q.Add(key, item)
			}
		}
	default:
		q.Add(key, fmt.Sprintf("%v", vv))
	}
}

// refreshOAuthAccessToken POSTs to the app's OAuth2 token endpoint with
// grant_type=refresh_token and merges the response back into the credentials
// map. Mutates credentials in place. Returns an error if the refresh fails.
//
// Some providers (notably Google) only return a NEW access_token on
// refresh — they do NOT return a new refresh_token. We preserve the
// existing refresh_token in that case. Other providers (Microsoft, some
// Atlassian flows) rotate the refresh_token on every refresh — we accept
// the new one and overwrite. The merge handles both correctly: any field
// present in the response replaces the matching field in credentials.
func refreshOAuthAccessToken(app *AppTemplate, credentials map[string]string) error {
	cfg := app.Auth.OAuth2
	if cfg == nil || cfg.TokenURL == "" {
		return fmt.Errorf("no oauth2 token_url for %s", app.Slug)
	}
	rt := credentials["refresh_token"]
	if rt == "" {
		rt = credentials["refreshToken"]
	}
	if rt == "" {
		return fmt.Errorf("no refresh_token in credentials")
	}
	clientID := credentials["client_id"]
	if clientID == "" {
		clientID = credentials["clientId"]
	}
	clientSecret := credentials["client_secret"]
	if clientSecret == "" {
		clientSecret = credentials["clientSecret"]
	}
	// Fall back to env vars so headless deploys without inline creds
	// (the original env-var-only flow) still get auto-refresh.
	if clientID == "" {
		clientID = oauthEnvClientID(app.Slug)
	}
	if clientSecret == "" {
		clientSecret = oauthEnvClientSecret(app.Slug)
	}
	if clientID == "" {
		return fmt.Errorf("no client_id available for refresh")
	}

	// Same client-id-param override as the rest of the OAuth flow — see
	// OAuthConfig.ClientIDParamName. TikTok needs "client_key" here too.
	clientIDParam := cfg.ClientIDParamName
	if clientIDParam == "" {
		clientIDParam = "client_id"
	}
	form := neturl.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	useBasicOnly := cfg.TokenAuthBasicOnly && clientSecret != ""
	if !useBasicOnly {
		form.Set(clientIDParam, clientID)
	}
	if clientSecret != "" && !useBasicOnly {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequest("POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token endpoint http %d: %s", resp.StatusCode, string(body))
	}

	// Accept either JSON or form-encoded responses (matching exchangeOAuthCode).
	out := make(map[string]string)
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || (len(body) > 0 && body[0] == '{') {
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("json decode: %w", err)
		}
		for k, v := range raw {
			out[k] = fmt.Sprint(v)
		}
	} else {
		values, err := neturl.ParseQuery(string(body))
		if err != nil {
			return fmt.Errorf("form decode: %w", err)
		}
		for k := range values {
			out[k] = values.Get(k)
		}
	}
	if out["access_token"] == "" {
		return fmt.Errorf("no access_token in refresh response: %s", string(body))
	}
	// Merge new tokens into credentials. Don't clobber the refresh_token
	// if the response didn't include a new one (Google's behavior).
	for k, v := range out {
		credentials[k] = v
	}
	// Update the camelCase mirrors so resolveTemplate's normalization
	// stays consistent for templates that use {{accessToken}} etc.
	if v := out["access_token"]; v != "" {
		credentials["accessToken"] = v
		credentials["token"] = v
	}
	if v := out["refresh_token"]; v != "" {
		credentials["refreshToken"] = v
	}
	return nil
}

// integrationInterceptorFn answers an integration call from a fixture
// instead of the real API. Returns (result, handled); handled=false means
// "not mine — make the real call".
type integrationInterceptorFn func(app *AppTemplate, tool *AppToolDef, input map[string]any) (*ExecuteResult, bool)

// environmentInterceptors maps an environment id to its integration interceptor. Empty in
// production; populated by the manager for the lifetime of an Environment.
// Per-environment keying (vs a single global) is what makes this multi-environment
// safe: a call only consults the interceptor for ITS environment id, threaded in
// from the X-Apteva-Environment-Id header that in-environment sidecars send on their
// platform callbacks. environmentID=="" — every production call — never touches
// this map, so behavior off the test path is byte-identical to before.
var environmentInterceptors sync.Map     // environmentID string -> integrationInterceptorFn
var environmentIntegrationModes sync.Map // environmentID string -> integration mode

func registerEnvironmentIntegrationMode(environmentID, mode string) func() {
	if environmentID == "" {
		return func() {}
	}
	environmentIntegrationModes.Store(environmentID, normalizeEnvironmentIntegrationMode(mode, ""))
	return func() { environmentIntegrationModes.Delete(environmentID) }
}

func inputSchemaRequires(schema map[string]any, field string) bool {
	if field == "" || schema == nil {
		return false
	}
	switch required := schema["required"].(type) {
	case []string:
		for _, name := range required {
			if name == field {
				return true
			}
		}
	case []any:
		for _, raw := range required {
			if name, ok := raw.(string); ok && name == field {
				return true
			}
		}
	}
	return false
}

func executeIntegrationTool(app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any, environmentID string) (*ExecuteResult, error) {
	// Environment test-mode seam: a call inside a Environment must NEVER reach the real
	// API. Resolve it fail-safe, in order:
	//   1. a per-environment interceptor fixture;
	//   2. the catalog tool's curated mock_response (faithful default);
	//   3. a generic stub-ok.
	if environmentID != "" && environmentIntegrationMode(environmentID) != IntegrationModeReal {
		if v, ok := environmentInterceptors.Load(environmentID); ok {
			if res, handled := v.(integrationInterceptorFn)(app, tool, input); handled {
				return res, nil
			}
		}
		if len(tool.MockResponse) > 0 {
			var data any
			_ = json.Unmarshal(tool.MockResponse, &data)
			if tool.ResponseTransform != nil && data != nil {
				transformed, _, err := buildResponseTransformData(tool.ResponseTransform, data, input)
				if err != nil {
					return nil, err
				}
				data = transformed
			}
			return &ExecuteResult{Success: true, Status: 200, Data: data}, nil
		}
		return &ExecuteResult{Success: true, Status: 200, Data: map[string]any{"ok": true, "_stub": true}}, nil
	}

	if app != nil && app.Slug == integrationOpenAICodexSlug {
		return executeOpenAICodexIntegrationTool(app, tool, credentials, input)
	}

	// Coerce input values to match the tool's schema types.
	// LLMs often send scalars where arrays are expected (e.g. account_ids=33 instead of [33]).
	if props, ok := tool.InputSchema["properties"].(map[string]any); ok {
		for k, v := range input {
			propDef, exists := props[k].(map[string]any)
			if !exists {
				continue
			}
			schemaType, _ := propDef["type"].(string)
			if schemaType == "array" {
				if _, isSlice := v.([]any); !isSlice {
					// Scalar value for an array field — wrap it
					input[k] = []any{v}
				}
			}
		}
	}

	// Build URL with credential templating + path param interpolation.
	// Both base_url and tool.path may contain {{X}} placeholders that
	// resolve against credentials (SES uses {{region}} on base_url;
	// Twilio uses {{account_sid}} on tool.path). Resolve both before
	// path-param interpolation so a missing credential surfaces as a
	// URL parse error rather than as a silent string concat.
	baseURL := app.BaseURL
	if tool.BaseURL != "" {
		baseURL = tool.BaseURL
	}
	resolvedBase := resolveTemplate(baseURL, credentials)
	resolvedPath := resolveTemplate(tool.Path, credentials)
	url := buildURL(resolvedBase, resolvedPath, input)
	usingContinuationURL := false
	if tool.ContinuationURLParam != "" {
		if raw := strings.TrimSpace(fmt.Sprint(input[tool.ContinuationURLParam])); raw != "" && raw != "<nil>" {
			continuationURL, err := validateContinuationURL(raw, resolvedBase)
			if err != nil {
				return nil, err
			}
			url = continuationURL
			usingContinuationURL = true
		}
		delete(input, tool.ContinuationURLParam)
	}

	// Add auth query params. buildAuthQuery returns raw "k=v&k=v" — pick
	// the separator based on whether tool.path already injected a "?".
	if authQ := buildAuthQuery(app.Auth.QueryParams, credentials); authQ != "" && !usingContinuationURL {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + authQ
	}

	// Build headers
	headers := buildHeaders(app.Auth.Headers, credentials)
	for _, key := range tool.OmitAuthHeaders {
		delete(headers, key)
	}
	for key, tmpl := range tool.Headers {
		headers[key] = resolveTemplate(tmpl, credentials)
	}
	for inputName, headerName := range tool.HeaderParams {
		if headerName == "" {
			continue
		}
		v, ok := input[inputName]
		if !ok || v == nil {
			continue
		}
		if str, isStr := v.(string); isStr && str == "" {
			continue
		}
		headers[headerName] = fmt.Sprint(v)
	}
	localHeaderTransformParams, err := applyHeaderTransforms(headers, tool.HeaderTransforms, input)
	if err != nil {
		return nil, err
	}
	if _, set := headers["Accept"]; !set {
		if _, lowerSet := headers["accept"]; !lowerSet {
			headers["Accept"] = "application/json"
		}
	}

	// Tool-level query_params: a set of input field names that must be
	// sent in the URL query string regardless of HTTP method. Required
	// for APIs that mix query+body on POST/PUT (Google Sheets'
	// values:append wants valueInputOption in the URL but the ValueRange
	// in the body). The set is built once and consulted for both the
	// body-building path (POST/PUT/PATCH) and the all-params-to-query
	// path (GET/DELETE) below. Empty when the template doesn't declare
	// query_params, in which case the new code path is a complete no-op
	// and behavior is identical to before — which is why this change is
	// safe for the other 261 templates that don't use the field.
	toolQuerySet := make(map[string]bool, len(tool.QueryParams)+len(tool.QueryParamAliases))
	localResponseParams := responseTransformLocalParams(tool.ResponseTransform)
	for _, name := range tool.QueryParams {
		if localResponseParams[name] || localHeaderTransformParams[name] {
			continue
		}
		toolQuerySet[name] = true
	}
	for name := range tool.QueryParamAliases {
		if localResponseParams[name] || localHeaderTransformParams[name] {
			continue
		}
		toolQuerySet[name] = true
	}
	// Collect tool-declared query params from input. Skip empty-string
	// values so optional fields don't become noisy ?foo= in the URL.
	toolQuery := neturl.Values{}
	for _, name := range tool.QueryParams {
		if localResponseParams[name] || localHeaderTransformParams[name] {
			continue
		}
		v, ok := input[name]
		if !ok || v == nil {
			continue
		}
		if str, isStr := v.(string); isStr && str == "" {
			continue
		}
		addQueryValue(toolQuery, name, v)
	}
	for inputName, queryName := range tool.QueryParamAliases {
		if localResponseParams[inputName] || localHeaderTransformParams[inputName] {
			continue
		}
		if queryName == "" {
			continue
		}
		v, ok := input[inputName]
		if !ok || v == nil {
			continue
		}
		if str, isStr := v.(string); isStr && str == "" {
			continue
		}
		addQueryValue(toolQuery, queryName, v)
	}
	if encoded := toolQuery.Encode(); encoded != "" && !usingContinuationURL {
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url += sep + encoded
	}

	transformedBody, hasTransformedBody, err := buildRequestTransformBody(tool.RequestTransform, input)
	if err != nil {
		return nil, err
	}
	var binaryBody any
	binaryBodyPresent := false
	if tool.BodyBinaryParam != "" {
		binaryBody, binaryBodyPresent = input[tool.BodyBinaryParam]
		binaryBodyPresent = binaryBodyPresent && binaryBody != nil
		if !binaryBodyPresent && inputSchemaRequires(tool.InputSchema, tool.BodyBinaryParam) {
			return nil, fmt.Errorf("body_binary_param %q is required", tool.BodyBinaryParam)
		}
	}

	// Build body for POST/PUT/PATCH, plus DELETE only when a template
	// explicitly declares a body slot. Some APIs, including Spaceship's
	// DNS delete endpoint, require a JSON body on DELETE; keeping the
	// default DELETE path query-only preserves existing integrations.
	var bodyReader io.Reader
	if tool.Method != "GET" && (tool.Method != "DELETE" || tool.BodyInput != "" || tool.BodyBinaryParam != "" || tool.BodyRoot != "" || tool.MultipartForm != nil || hasTransformedBody) {
		// Raw-body path: tool declared a single input field that
		// carries the request body verbatim (S3 PutObject, R2
		// PutObject, etc.). Skip the JSON map assembly entirely.
		//
		// Wire-format note: Go's encoding/json marshals []byte as a
		// base64 string and there's no way to distinguish a
		// "literal base64-shaped string" from "encoded binary" once
		// it's deserialized into map[string]any. We try base64-decode
		// first; if that fails (text body with spaces / non-base64
		// characters), we send the raw string. App-side: pass []byte
		// for binary, strings for text — this keeps the round-trip
		// lossless for both.
		if tool.MultipartForm != nil {
			body, contentType, err := buildMultipartRequestBody(tool, input, credentials, app.Auth.BodyParams)
			if err != nil {
				return nil, err
			}
			bodyReader = body
			headers["Content-Type"] = contentType
		} else if tool.BodyBinaryParam != "" && binaryBodyPresent {
			env, ok := binaryBody.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("body_binary_param %q must be a binary envelope", tool.BodyBinaryParam)
			}
			if binary, _ := env["_binary"].(bool); !binary {
				return nil, fmt.Errorf("body_binary_param %q must have _binary=true", tool.BodyBinaryParam)
			}
			encoded, _ := env["base64"].(string)
			if encoded == "" {
				return nil, fmt.Errorf("body_binary_param %q must include base64 data", tool.BodyBinaryParam)
			}
			bodyBytes, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("decode body_binary_param %q: %w", tool.BodyBinaryParam, err)
			}
			bodyReader = bytes.NewReader(bodyBytes)
			if mimeType, _ := env["mimeType"].(string); mimeType != "" {
				headers["Content-Type"] = mimeType
			} else if _, set := headers["Content-Type"]; !set {
				headers["Content-Type"] = "application/octet-stream"
			}
		} else if tool.BodyInput != "" {
			if v, ok := input[tool.BodyInput]; ok && v != nil {
				var bodyBytes []byte
				switch raw := v.(type) {
				case []byte:
					bodyBytes = raw
				case string:
					if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
						bodyBytes = decoded
					} else {
						bodyBytes = []byte(raw)
					}
				default:
					bodyBytes = []byte(fmt.Sprintf("%v", raw))
				}
				bodyReader = bytes.NewReader(bodyBytes)
			}
			// If the template didn't set a Content-Type, default to
			// octet-stream — anything calling raw-body path is
			// almost always sending binary.
			if _, set := headers["Content-Type"]; !set {
				headers["Content-Type"] = "application/octet-stream"
			}
		} else if tool.BodyRoot != "" {
			// Root-body path: the named input field's value IS the
			// whole JSON body (e.g. a bare array). Marshal it verbatim
			// instead of wrapping all inputs in a flat object. Path
			// params + tool-declared query_params were already peeled
			// into the URL above; any other inputs are dropped (the
			// body slot belongs to this one field).
			if v, ok := input[tool.BodyRoot]; ok && v != nil {
				data, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("marshal body_root_param %q: %w", tool.BodyRoot, err)
				}
				bodyReader = strings.NewReader(string(data))
			}
			if _, set := headers["Content-Type"]; !set {
				headers["Content-Type"] = "application/json"
			}
		} else if hasTransformedBody {
			body := transformedBody
			if bodyMap, ok := transformedBody.(map[string]any); ok {
				merged := buildAuthBodyParams(app.Auth.BodyParams, credentials)
				for k, v := range bodyMap {
					merged[k] = v
				}
				body = merged
			}
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal request_transform body: %w", err)
			}
			bodyReader = strings.NewReader(string(data))
			if _, set := headers["Content-Type"]; !set {
				headers["Content-Type"] = "application/json"
			}
		} else {
			// Merge default credential fields into body
			bodyMap := buildAuthBodyParams(app.Auth.BodyParams, credentials)
			for _, f := range app.Auth.CredentialFields {
				if val, ok := credentials[f.Name]; ok {
					// Map credential fields to common input names
					if f.Name == "user_key" {
						bodyMap["user"] = val
					}
				}
			}
			// Merge user input (overrides defaults, skip empty values)
			for k, v := range input {
				// Skip path params
				if strings.Contains(tool.Path, "{"+k+"}") {
					continue
				}
				// Skip tool-declared query params — they were peeled out
				// above and added to the URL.
				if toolQuerySet[k] {
					continue
				}
				if localResponseParams[k] {
					continue
				}
				if localHeaderTransformParams[k] {
					continue
				}
				if _, isHeaderParam := tool.HeaderParams[k]; isHeaderParam {
					continue
				}
				// Don't override credential defaults with empty values
				if str, ok := v.(string); ok && str == "" {
					continue
				}
				bodyMap[k] = v
			}
			if len(bodyMap) > 0 {
				// Default JSON body. If the integration declared
				// application/x-www-form-urlencoded in auth.headers,
				// marshal the body as form fields instead — Twilio,
				// Stripe, and a handful of others want this.
				ct := headers["Content-Type"]
				if strings.Contains(strings.ToLower(ct), "x-www-form-urlencoded") {
					bodyReader = strings.NewReader(formEncode(bodyMap))
				} else {
					data, _ := json.Marshal(bodyMap)
					bodyReader = strings.NewReader(string(data))
					headers["Content-Type"] = "application/json"
				}
			}
		}
	} else {
		// GET/DELETE: add remaining params as query string. Skip
		// path params, tool-declared query params (already added
		// above), and body_input (which has nowhere to go on
		// GET/DELETE — caller is misconfigured but don't leak it
		// into the URL).
		//
		// Use neturl.Values to percent-encode values per RFC 3986.
		// Without this, payloads containing special chars (the most
		// painful case: AWS Query API tools like sns:SetTopicAttributes
		// where AttributeValue is a JSON access policy) end up with
		// literal "{", '"', ":", "," in the URL — AWS rejects with
		// HTTP 400 empty body, and SigV4 signing canonicalizes the
		// query differently than the wire URL so the signature also
		// mismatches. Encoding fixes both at once.
		q := neturl.Values{}
		for k, v := range input {
			if usingContinuationURL {
				continue
			}
			if strings.Contains(tool.Path, "{"+k+"}") {
				continue
			}
			if toolQuerySet[k] {
				continue
			}
			if localResponseParams[k] {
				continue
			}
			if localHeaderTransformParams[k] {
				continue
			}
			if _, isHeaderParam := tool.HeaderParams[k]; isHeaderParam {
				continue
			}
			if k == tool.BodyInput || k == tool.BodyBinaryParam || k == tool.BodyRoot {
				continue
			}
			addQueryValue(q, k, v)
		}
		if encoded := q.Encode(); encoded != "" {
			sep := "&"
			if !strings.Contains(url, "?") {
				sep = "?"
			}
			url += sep + encoded
		}
	}

	// Snapshot the body bytes before we wrap them as an io.Reader so
	// SigV4 (below) can hash them. Cheap; bodies for these calls are
	// kilobytes at most.
	var bodyBytes []byte
	if bodyReader != nil {
		buf, _ := io.ReadAll(bodyReader)
		bodyBytes = buf
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(tool.Method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Request signing — dispatched via the Signer registry (signer.go).
	// Replaces the old aws_sigv4-only inline branch. The legacy
	// auth.types=["aws_sigv4"] declaration still works (translated by
	// effectiveSigners); new catalog entries should declare
	// auth.signers[] / tools[].signing.signers[] directly.
	//
	// Body-mutating signers (EIP-712 typed-data) may rewrite bodyBytes;
	// re-attach the new bytes to req before client.Do.
	if specs := effectiveSigners(app, tool); len(specs) > 0 {
		newBody, err := runSigners(req.Context(), req, bodyBytes, credentials, specs)
		if err != nil {
			return nil, fmt.Errorf("sign: %w", err)
		}
		if newBody != nil && !bytes.Equal(newBody, bodyBytes) {
			bodyBytes = newBody
			req.Body = io.NopCloser(strings.NewReader(string(newBody)))
			req.ContentLength = int64(len(newBody))
		}
	}

	timeout := 30 * time.Second
	if tool.TimeoutMS > 0 {
		timeout = time.Duration(tool.TimeoutMS) * time.Millisecond
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
	}
	client, err := integrationHTTPClient(app, credentials, timeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Response cap: small for JSON/text (10 MB — anything bigger from
	// these is almost certainly a runaway, not a real tool result).
	// Big for binary (200 MB) so apps that legitimately stream tarballs
	// / images / audio (Code's repos_import_github, Cloudinary uploads,
	// Deepgram audio responses, …) can complete. The size split mirrors
	// the TS executor's `maxBinaryBytes` knob; we don't expose a per-tool
	// override yet because no template needs one.
	ct := resp.Header.Get("Content-Type")
	binary := isBinaryContentType(ct)
	maxBytes := int64(10_000_000)
	if binary {
		maxBytes = 200_000_000
	}
	hdrs := pickForwardableHeaders(resp.Header)
	if cl := resp.ContentLength; cl > 0 && cl > maxBytes {
		return &ExecuteResult{
			Success: false,
			Status:  resp.StatusCode,
			Data: map[string]any{
				"error": "response too large",
				"size":  cl,
				"max":   maxBytes,
			},
			Headers: hdrs,
		}, nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(respBody)) > maxBytes {
		return &ExecuteResult{
			Success: false,
			Status:  resp.StatusCode,
			Data: map[string]any{
				"error": "response too large",
				"size":  len(respBody),
				"max":   maxBytes,
			},
			Headers: hdrs,
		}, nil
	}

	var data any
	switch {
	case binary:
		// Binary responses get wrapped in the same envelope shape the
		// TS executor produces, so apps decoding ExecuteResult.Data
		// see one consistent shape regardless of which runner served
		// the call.
		mime := ct
		if i := strings.Index(mime, ";"); i >= 0 {
			mime = strings.TrimSpace(mime[:i])
		}
		data = map[string]any{
			"_binary":  true,
			"base64":   base64.StdEncoding.EncodeToString(respBody),
			"mimeType": mime,
			"size":     len(respBody),
		}
	case strings.Contains(ct, "json"):
		data = decodeIntegrationJSON(respBody)
	default:
		data = string(respBody)
	}

	// Success transforms describe provider success payloads. Applying them
	// to errors can erase the provider's error object and make failures look
	// like empty resources (notably Gmail thread 404 responses).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ExecuteResult{
			Success: false,
			Status:  resp.StatusCode,
			Data:    normalizeIntegrationHTTPError(resp.StatusCode, data),
			Headers: hdrs,
		}, nil
	}

	// Apply response_path extraction
	if tool.ResponsePath != nil && data != nil {
		if m, ok := data.(map[string]any); ok {
			data = extractPath(m, *tool.ResponsePath)
		}
	}

	if tool.ResponseTransform != nil && data != nil && !binary {
		transformed, _, err := buildResponseTransformData(tool.ResponseTransform, data, input)
		if err != nil {
			return nil, err
		}
		data = transformed
	}

	// Strip any fields the tool declared we shouldn't expose. Runs
	// AFTER response_path so the paths are relative to whatever the
	// agent actually ends up seeing. Silent no-op on unmatched paths
	// so a minor upstream schema drift doesn't break the tool.
	if len(tool.ResponseOmit) > 0 && data != nil {
		for _, p := range tool.ResponseOmit {
			data = omitPath(data, p)
		}
	}

	return &ExecuteResult{
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Status:  resp.StatusCode,
		Data:    data,
		Headers: hdrs,
	}, nil
}

func environmentIntegrationMode(environmentID string) string {
	if environmentID == "" {
		return IntegrationModeReal
	}
	if v, ok := environmentIntegrationModes.Load(environmentID); ok {
		if mode, ok := v.(string); ok && mode != "" {
			return normalizeEnvironmentIntegrationMode(mode, "")
		}
	}
	return IntegrationModeMock
}

func extractPath(data map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = data
	for _, p := range parts {
		if m, ok := current.(map[string]any); ok {
			current = m[p]
		} else {
			return current
		}
	}
	return current
}

// omitPath walks `data` along the dot-separated path and deletes the
// leaf segment wherever it matches. `[]` in a segment means "for every
// element of this array, continue the walk". Non-matching structures
// are left alone — this is an in-place filter that's forgiving about
// schema drift.
//
// Examples:
//
//	"metadata.sha256"                              → delete root.metadata.sha256
//	"results.channels[].alternatives[].words"     → delete words from every alt of every channel
//	"utterances"                                   → delete root.utterances
func omitPath(data any, path string) any {
	parts := strings.Split(path, ".")
	omitWalk(data, parts)
	return data
}

func omitWalk(node any, parts []string) {
	if len(parts) == 0 {
		return
	}
	head := parts[0]
	rest := parts[1:]

	// Normalise "foo[]" into two steps: descend into `foo`, then step
	// across the array. This lets the caller write
	// `results.channels[].alternatives[].words` naturally.
	arrayStep := strings.HasSuffix(head, "[]")
	if arrayStep {
		head = strings.TrimSuffix(head, "[]")
	}

	switch n := node.(type) {
	case map[string]any:
		if len(rest) == 0 && !arrayStep {
			// Leaf: remove the key if it exists. Silent if absent.
			delete(n, head)
			return
		}
		child, ok := n[head]
		if !ok {
			return
		}
		if arrayStep {
			// `head` is expected to be an array; iterate and recurse
			// with the remaining parts.
			if arr, ok := child.([]any); ok {
				for i := range arr {
					omitWalk(arr[i], rest)
				}
			}
			return
		}
		omitWalk(child, rest)

	case []any:
		// Array with no `[]` marker in the pattern — fan out anyway so
		// callers can write shorter paths when there's exactly one
		// array in the chain. Safer as a fallback than a no-op.
		for i := range n {
			omitWalk(n[i], append([]string{head}, rest...))
		}
	}
}

// --- HTTP Handlers ---

// POST /connections
//
// Source dispatch:
//   - source=='local' (default) + auth_type=='oauth2' → startLocalOAuth, return authorize_url
//   - source=='local' otherwise → existing api_key / basic path, return active connection
//   - source=='composio' → InitiateConnection on Composio, return redirect_url and pending row
func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	// Log the whole lifecycle under a single tag so failures are easy
	// to locate in the server log. Follow-up lines read "[CONN] step …
	// source=X slug=Y project=Z outcome=…".
	reqStart := time.Now()
	defer func() {
		log.Printf("[CONN] POST /connections completed in %s", time.Since(reqStart).Round(time.Millisecond))
	}()
	userID := getUserID(r)

	var body struct {
		Source      string            `json:"source"`
		AppSlug     string            `json:"app_slug"`
		Name        string            `json:"name"`
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
		ProjectID   string            `json:"project_id"`
		ProviderID  int64             `json:"provider_id"` // required for source=composio
		// Local OAuth2 only: the user's own OAuth app credentials, collected
		// from the dashboard form on first connect to a given app+project.
		// Folded into the connection's encrypted blob so subsequent connects
		// to the same app skip the form entirely.
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		// Composio-only: which upstream auth mode to configure (OAUTH2, API_KEY, BASIC, ...)
		// and two credential maps — one for auth_config creation and one for
		// the per-connection link (Composio schema distinguishes them).
		ComposioAuthMode    string            `json:"composio_auth_mode"`
		ComposioConfigCreds map[string]string `json:"composio_config_creds"`
		ComposioInitCreds   map[string]string `json:"composio_init_creds"`
		// CreatedVia: 'integration' (default — top-level install via the
		// Integrations page; auto-creates an mcp_servers row exposing the
		// integration's tools to agents) or 'app_install' (created inside
		// an app's dependency picker; no auto-MCP — the consuming app is
		// the only intended caller). The dashboard's app-install modal
		// passes 'app_install' when minting a new connection through the
		// integration picker.
		CreatedVia string `json:"created_via"`
		// AutoMCP: when omitted or true, an mcp_servers row is created
		// on connect so agents in the project can call the integration's
		// tools globally. When false, the connection exists but no MCP
		// server materialises — useful when the operator wants the
		// connection bound to a specific app (e.g. Facebook for Social)
		// rather than exposed to every agent. Pointer so the dashboard
		// can omit the field for back-compat with the auto-MCP default.
		AutoMCP *bool `json:"auto_mcp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.AppSlug == "" {
		http.Error(w, "app_slug required", http.StatusBadRequest)
		return
	}
	if body.Source == "" {
		body.Source = "local"
	}

	// --- Composio (hosted) ---
	if body.Source == "composio" {
		log.Printf("[CONN] composio create: user=%d slug=%s project=%s provider=%d auth_mode=%s",
			userID, body.AppSlug, body.ProjectID, body.ProviderID, body.ComposioAuthMode)
		if body.ProviderID == 0 {
			http.Error(w, "provider_id required for composio source", http.StatusBadRequest)
			return
		}
		client, err := s.composioClientFor(userID, body.ProviderID)
		if err != nil {
			log.Printf("[CONN] composio client resolve failed user=%d provider=%d: %v", userID, body.ProviderID, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Dedup: each call to /api/v3/connected_accounts/link creates a fresh
		// upstream `ca_*` on Composio, and Composio does not clean up the
		// prior one when the user retries (old links expire instead). Result:
		// one real attempt leaves two or more connected accounts against the
		// same (auth_config, user_id), and the MCP endpoint may route tool
		// calls through the stale one — which is what breaks tools for the
		// user after a retry.
		//
		// Before initiating, look for an existing connection row in this
		// (user, project, provider, app_slug) scope:
		//   - status=active: return it as-is. The UI treats this as "already
		//     connected" so the user doesn't double-click into a duplicate.
		//   - status=pending: revoke the stale composio-side connected
		//     account (best-effort) and delete the local row, then continue
		//     with a fresh InitiateConnection.
		existing, lerr := s.store.ListConnections(userID, body.ProjectID)
		if lerr == nil {
			for i := range existing {
				c := &existing[i]
				if c.Source != "composio" || c.ProviderID != body.ProviderID || c.AppSlug != body.AppSlug {
					continue
				}
				if c.Status == "active" {
					log.Printf("[CONN] composio reuse active connection id=%d external_id=%s", c.ID, c.ExternalID)
					writeJSON(w, map[string]any{
						"connection":   c,
						"redirect_url": "",
					})
					return
				}
				if c.Status == "pending" {
					log.Printf("[CONN] composio pruning stale pending connection id=%d external_id=%s", c.ID, c.ExternalID)
					if c.ExternalID != "" {
						if rerr := client.RevokeConnection(c.ExternalID); rerr != nil {
							// Non-fatal — the upstream record may already be
							// expired/gone. Log and continue so the user's
							// retry still works.
							log.Printf("[CONN] composio revoke stale external_id=%s failed (continuing): %v", c.ExternalID, rerr)
						}
					}
					if derr := s.store.DeleteConnection(userID, c.ID); derr != nil {
						log.Printf("[CONN] composio delete stale local row id=%d failed: %v", c.ID, derr)
					}
				}
			}
		}

		endUserID := composioEndUserID(userID, body.ProjectID)
		acct, redirectURL, err := client.InitiateConnection(
			body.AppSlug, body.ComposioAuthMode, endUserID,
			body.ComposioConfigCreds, body.ComposioInitCreds,
		)
		if err != nil {
			log.Printf("[CONN] composio InitiateConnection failed slug=%s auth_mode=%s: %v", body.AppSlug, body.ComposioAuthMode, err)
			http.Error(w, "composio initiate: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("[CONN] composio InitiateConnection ok slug=%s external_id=%s redirect=%v", body.AppSlug, acct.ID, redirectURL != "")
		connName := body.Name
		if connName == "" {
			connName = body.AppSlug
		}
		// Composio's hosted flow is the source of truth for credential
		// collection. Every new connection starts as pending and flips to
		// active only after the user completes the Connect Link on
		// Composio's side. Reconcile runs later in the polling path
		// (handleGetConnection) when we observe the upstream ACTIVE state.
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:     userID,
			AppSlug:    body.AppSlug,
			AppName:    body.AppSlug,
			Name:       connName,
			AuthType:   "composio",
			ProjectID:  body.ProjectID,
			Source:     "composio",
			Status:     "pending",
			ProviderID: body.ProviderID,
			ExternalID: acct.ID,
		})
		if err != nil {
			// Surface the underlying DB error so the dashboard shows
			// something actionable instead of a generic message. Most
			// common cause: the UNIQUE (user_id, project_id, app_slug,
			// name) index fires when the user already has a connection
			// with this name.
			log.Printf("[CONN] composio CreateConnectionExt failed slug=%s name=%s project=%s: %v",
				body.AppSlug, connName, body.ProjectID, err)
			http.Error(w, "failed to create connection: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[CONN] composio connection row id=%d slug=%s status=pending external_id=%s",
			conn.ID, conn.AppSlug, conn.ExternalID)
		writeJSON(w, map[string]any{
			"connection":   conn,
			"redirect_url": redirectURL,
		})
		return
	}

	// --- Local catalog ---
	app := s.catalog.Get(body.AppSlug)
	if app == nil {
		http.Error(w, "app not found in catalog", http.StatusNotFound)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if body.AuthType == "" {
		// Auto-pick the most appropriate auth type for this app. Many
		// templates list both "bearer" and "oauth2" — bearer because the
		// access token IS a bearer token, oauth2 because that's how to
		// obtain it. We always prefer oauth2 in that case (and prefer
		// it whenever an oauth2 block exists), otherwise fall back to
		// the first declared type, otherwise default to api_key.
		//
		// Without this preference, Google Sheets and similar apps were
		// silently routed through the non-OAuth path on connect: the
		// server stored an empty credentials blob and marked the row
		// active without ever triggering the OAuth popup.
		switch {
		case app.Auth.OAuth1 != nil && containsString(app.Auth.Types, "oauth1"):
			body.AuthType = "oauth1"
		case app.Auth.OAuth2 != nil && containsString(app.Auth.Types, "oauth2"):
			body.AuthType = "oauth2"
		case len(app.Auth.Types) > 0:
			body.AuthType = app.Auth.Types[0]
		default:
			body.AuthType = "api_key"
		}
	}

	// Enforce (user, project, app, name) uniqueness upfront so the user
	// gets a readable error instead of a raw UNIQUE constraint violation.
	var existingCount int
	s.store.db.QueryRow(
		"SELECT COUNT(*) FROM connections WHERE user_id = ? AND project_id = ? AND app_slug = ? AND name = ?",
		userID, body.ProjectID, body.AppSlug, body.Name,
	).Scan(&existingCount)
	if existingCount > 0 {
		http.Error(w, "a connection for this app with that name already exists in this project — pick a different name", http.StatusConflict)
		return
	}

	// Local browser OAuth — two-phase: start flow, return authorize URL, finish in callback.
	if body.AuthType == "oauth1" || body.AuthType == "oauth2" {
		supplementalCredentials, err := collectOAuthSupplementalCredentials(app, body.Credentials)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conn, authURL, err := s.startLocalOAuth(userID, app, body.Name, body.ProjectID, body.ClientID, body.ClientSecret, supplementalCredentials, 0, "", body.AutoMCP)
		if err != nil {
			http.Error(w, "oauth start: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"connection":   conn,
			"redirect_url": authURL,
		})
		return
	}

	// Local device-code auth — two-phase without a popup. The connection
	// row is created pending, the UI shows the upstream user_code, and
	// /connections/auth/:session polls until credentials can be stored.
	if body.AuthType == connectionAuthTypeDeviceCode {
		if !supportsConnectionDeviceAuth(app) {
			http.Error(w, "device-code auth is not supported for this integration", http.StatusBadRequest)
			return
		}
		empty, _ := json.Marshal(map[string]string{})
		encrypted, err := Encrypt(s.secret, string(empty))
		if err != nil {
			log.Printf("[CONN] device-code encrypt failed slug=%s: %v", body.AppSlug, err)
			http.Error(w, "encryption failed", http.StatusInternalServerError)
			return
		}
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:         userID,
			AppSlug:        body.AppSlug,
			AppName:        app.Name,
			Name:           body.Name,
			AuthType:       body.AuthType,
			EncryptedCreds: encrypted,
			ProjectID:      body.ProjectID,
			Source:         "local",
			Status:         "pending",
			CreatedVia:     body.CreatedVia,
			AutoMCP:        body.AutoMCP,
		})
		if err != nil {
			log.Printf("[CONN] device-code CreateConnectionExt failed slug=%s name=%s project=%s: %v",
				body.AppSlug, body.Name, body.ProjectID, err)
			http.Error(w, "failed to create connection: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		deviceAuth, err := s.startConnectionDeviceAuth(ctx, userID, app, conn)
		if err != nil {
			_ = s.store.DeleteConnection(userID, conn.ID)
			http.Error(w, "device auth start: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"connection":  conn,
			"device_auth": deviceAuth,
		})
		return
	}

	// Local non-OAuth (api_key, basic, bearer, ...): store creds immediately.
	log.Printf("[CONN] local create: user=%d slug=%s name=%s auth=%s project=%s",
		userID, body.AppSlug, body.Name, body.AuthType, body.ProjectID)
	generatedCredentials, generationErr := materializeGeneratedConnectionCredentials(app, body.Credentials)
	if generationErr != nil {
		log.Printf("[CONN] generated credentials failed slug=%s: %v", body.AppSlug, generationErr)
		http.Error(w, "credential generation failed", http.StatusInternalServerError)
		return
	}
	body.Credentials = generatedCredentials
	credsJSON, _ := json.Marshal(body.Credentials)
	encrypted, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		log.Printf("[CONN] local encrypt failed slug=%s: %v", body.AppSlug, err)
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	isDelegatedProvider := isDelegatedProviderCredentialsMap(body.Credentials)

	// Pre-flight: if the catalog declares a health_check, run it
	// against the encrypted blob BEFORE persisting. The motivation
	// is operator UX — pre-v0.13 the user could type a wrong API
	// key, see "active" instantly, and only learn the creds were
	// bogus when an agent failed days later. Now the dashboard's
	// create form gets back a 400 with a human-readable error
	// ("HTTP 401: invalid_token") and nothing is saved. The OAuth
	// branch above doesn't go through this path because the
	// callback-time token exchange already serves as proof-of-life.
	//
	// Skip when there's no health_check: 0-arg probes aren't
	// universally available across 431 catalog entries; absence is
	// the catalog author's signal of "I haven't characterised a
	// safe probe for this app yet" rather than an error.
	if app.HealthCheck != nil && (app.HealthCheck.Tool != "" || app.HealthCheck.Path != "") && !isDelegatedProvider {
		probe := s.runHealthCheck(app, encrypted)
		if !probe.OK && !probe.Skipped {
			log.Printf("[CONN] preflight FAILED slug=%s status=%d err=%q",
				body.AppSlug, probe.StatusCode, probe.Error)
			// Return JSON so the dashboard can render the error
			// inline next to the credential fields rather than
			// surfacing a generic toast. 400 is conventional for
			// "your input was rejected by the upstream"; 502 would
			// imply the failure was on our side.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":        "credential check failed",
				"detail":       probe.Error,
				"status_code":  probe.StatusCode,
				"latency_ms":   probe.LatencyMS,
				"health_check": true,
			})
			return
		}
		log.Printf("[CONN] preflight OK slug=%s latency_ms=%d", body.AppSlug, probe.LatencyMS)
	} else if isDelegatedProvider {
		log.Printf("[CONN] delegated provider connection slug=%s project=%s skips local credential health check", body.AppSlug, body.ProjectID)
	}

	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         userID,
		AppSlug:        body.AppSlug,
		AppName:        app.Name,
		Name:           body.Name,
		AuthType:       body.AuthType,
		EncryptedCreds: encrypted,
		ProjectID:      body.ProjectID,
		Source:         "local",
		Status:         "active",
		CreatedVia:     body.CreatedVia,
		AutoMCP:        body.AutoMCP,
	})
	if err != nil {
		log.Printf("[CONN] local CreateConnectionExt failed slug=%s name=%s project=%s: %v",
			body.AppSlug, body.Name, body.ProjectID, err)
		http.Error(w, "failed to create connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[CONN] local connection row id=%d slug=%s status=active created_via=%s",
		conn.ID, conn.AppSlug, body.CreatedVia)
	// Auto-create an mcp_servers row only when:
	//   1. the connection was born at the top-level Integrations
	//      page (app-install minted connections always skip — the
	//      consuming app is the intended caller, not every agent
	//      in the project), AND
	//   2. the operator hasn't opted out via auto_mcp=false.
	// Operators can flip the auto_mcp flag later via PATCH
	// /connections/:id/expose if they change their mind.
	autoMCP := connectionAutoMCPFlag(s, conn.ID)
	if body.CreatedVia != "app_install" && autoMCP {
		if app.Kind == "remote_mcp" {
			// Hosted-MCP apps go through the vendor's MCP server, not a
			// generated local stdio. Re-fetch the encrypted creds blob
			// the connection just persisted so we can resolve the auth
			// header template once and stash the upstream URL + creds in
			// mcp_servers as source=remote.
			_, encCreds, gerr := s.store.GetConnection(userID, conn.ID)
			if gerr != nil {
				log.Printf("[CONN] remote-mcp auto-mcp FAILED (load creds) conn=%d: %v", conn.ID, gerr)
			} else if mcpID, merr := s.createRemoteMcpFromConnection(userID, conn, app, encCreds); merr != nil {
				log.Printf("[CONN] remote-mcp auto-mcp FAILED conn=%d (%s/%s): %v", conn.ID, conn.AppSlug, conn.Name, merr)
			} else {
				log.Printf("[CONN] remote-mcp auto-mcp created mcp_id=%d conn=%d slug=%s url=%s", mcpID, conn.ID, conn.AppSlug, app.MCP.URL)
			}
		} else if mcpID, merr := s.store.CreateMCPServerFromConnection(userID, conn, len(app.Tools)); merr != nil {
			log.Printf("[CONN] local auto-mcp FAILED conn=%d (%s/%s): %v", conn.ID, conn.AppSlug, conn.Name, merr)
		} else {
			log.Printf("[CONN] local auto-mcp created mcp_id=%d conn=%d slug=%s tools=%d", mcpID, conn.ID, conn.AppSlug, len(app.Tools))
		}
	} else {
		if body.CreatedVia == "app_install" {
			log.Printf("[CONN] skipping auto-mcp for app_install conn=%d", conn.ID)
		} else {
			log.Printf("[CONN] skipping auto-mcp (operator opted out via auto_mcp=false) conn=%d", conn.ID)
		}
	}
	// New connection may unblock optional dep prompts on existing installs.
	s.recomputePendingOptions()
	writeJSON(w, conn)
}

// collectOAuthSupplementalCredentials validates and returns only fields
// explicitly classified as user-supplied. Legacy OAuth templates without
// source metadata retain their existing behaviour; OAuth-generated/hidden
// fields are never accepted from the operator.
func collectOAuthSupplementalCredentials(app *AppTemplate, credentials map[string]string) (map[string]string, error) {
	if app == nil {
		return nil, nil
	}
	collected := make(map[string]string)
	for _, field := range app.Auth.CredentialFields {
		if field.Source != "user" || field.Hidden {
			continue
		}
		value := strings.TrimSpace(credentials[field.Name])
		required := field.Required == nil || *field.Required
		if required && value == "" {
			label := strings.TrimSpace(field.Label)
			if label == "" {
				label = field.Name
			}
			return nil, fmt.Errorf("%s required", label)
		}
		if value != "" {
			collected[field.Name] = value
		}
	}
	if len(collected) == 0 {
		return nil, nil
	}
	return collected, nil
}

// connectionAutoMCPFlag reads the auto_mcp boolean off the row.
// Returns true on lookup failure so we don't silently skip auto-MCP
// when the column is missing on a freshly-migrated DB.
func connectionAutoMCPFlag(s *Server, connID int64) bool {
	var v int
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(auto_mcp, 1) FROM connections WHERE id=?`,
		connID,
	).Scan(&v); err != nil {
		return true
	}
	return v != 0
}

func hasMCPServerForConnection(s *Server, connID int64) bool {
	var count int
	_ = s.store.db.QueryRow(
		`SELECT COUNT(*) FROM mcp_servers WHERE connection_id=?`,
		connID,
	).Scan(&count)
	return count > 0
}

// GET /connections/:id — single connection (used by dashboard to poll pending
// states during OAuth flows).
func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Composio pending connections: poll upstream and flip to active on ACTIVE.
	if conn.Source == "composio" && conn.Status == "pending" && conn.ExternalID != "" {
		if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil {
			if acct, perr := client.GetConnectedAccount(conn.ExternalID); perr == nil {
				switch strings.ToUpper(acct.Status) {
				case "ACTIVE":
					s.store.UpdateConnectionStatus(conn.ID, "active")
					conn.Status = "active"
					// Reconcile the project's aggregate Composio MCP server.
					if rerr := s.reconcileComposioMCPServer(userID, conn.ProviderID, conn.ProjectID); rerr != nil {
						fmt.Fprintf(os.Stderr, "composio reconcile: %v\n", rerr)
					}
				case "FAILED", "EXPIRED":
					s.store.UpdateConnectionStatus(conn.ID, "failed")
					conn.Status = "failed"
				}
			}
		}
	}
	writeJSON(w, conn)
}

// GET /connections/:id/credentials — owner-only credential reveal.
// Decrypts the stored blob and returns the credential map.
//
// Threat model: once exposed via this endpoint, a token is reachable
// through the dashboard session cookie (the default storage model
// requires DB access). Operator feature, owner-only — the dashboard
// gates it behind an explicit "Reveal" click. Each call is logged so
// post-hoc audit can see who viewed which connection.
func (s *Server) handleGetConnectionCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/connections/"), "/credentials")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	creds := map[string]string{}
	if encCreds != "" {
		plain, derr := Decrypt(s.secret, encCreds)
		if derr != nil {
			http.Error(w, "decrypt failed", http.StatusInternalServerError)
			return
		}
		if jerr := json.Unmarshal([]byte(plain), &creds); jerr != nil {
			http.Error(w, "parse creds failed", http.StatusInternalServerError)
			return
		}
	}
	log.Printf("[CONNECTIONS] credentials revealed: user=%d conn=%d app=%s name=%q",
		userID, connID, conn.AppSlug, conn.Name)
	writeJSON(w, map[string]any{"credentials": creds})
}

// PATCH /connections/:id — rename an existing connection.
// Body: { "name": "..." }. Only the name is editable via this endpoint;
// credential swap goes through the invite flow or the OAuth callback.
func (s *Server) handleRenameConnection(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if conn.Name == name {
		writeJSON(w, conn)
		return
	}
	// Uniqueness: (user, project, app, name) must stay unique — match the
	// guard in handleCreateConnection so rename failures are readable.
	var existing int
	s.store.db.QueryRow(
		"SELECT COUNT(*) FROM connections WHERE user_id = ? AND project_id = ? AND app_slug = ? AND name = ? AND id != ?",
		userID, conn.ProjectID, conn.AppSlug, name, connID,
	).Scan(&existing)
	if existing > 0 {
		http.Error(w, "a connection with that name already exists for this app in this project", http.StatusConflict)
		return
	}
	if _, err := s.store.db.Exec(
		"UPDATE connections SET name = ? WHERE id = ? AND user_id = ?",
		name, connID, userID,
	); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	conn.Name = name
	writeJSON(w, conn)
}

// GET /connections
func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	includeAppOwned := r.URL.Query().Get("include_app_owned") == "1"
	conns, err := s.store.ListConnections(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if conns == nil {
		conns = []Connection{}
	}

	// Enrich with tool count + credential-group metadata. Master rows
	// (slug prefix `_group:`) are hidden from the default list — they
	// have no user-facing tools and exist only as credential storage
	// for their children. Pass ?include=masters to see them (used by
	// the credential manager screen).
	includeMasters := r.URL.Query().Get("include") == "masters"

	type ConnectionWithTools struct {
		Connection
		ToolCount         int    `json:"tool_count"`
		Logo              string `json:"logo,omitempty"`
		GroupID           string `json:"group_id,omitempty"`
		IsGroupChild      bool   `json:"is_group_child,omitempty"`
		ExternalProjectID string `json:"external_project_id,omitempty"`
	}
	var enriched []ConnectionWithTools
	for _, c := range conns {
		if !includeAppOwned && isAppOwnedConnection(c) {
			continue
		}
		if !includeMasters && IsMasterSlug(c.AppSlug) {
			continue
		}
		tc := 0
		logo := ""
		var groupID string
		if app := s.catalog.Get(c.AppSlug); app != nil {
			tc = len(app.Tools)
			if app.Logo != nil {
				logo = *app.Logo
			}
			if app.CredentialGroup != nil {
				groupID = app.CredentialGroup.ID
			}
		}
		row := ConnectionWithTools{Connection: c, ToolCount: tc, Logo: logo, GroupID: groupID}
		// Detect child rows cheaply by peeking at the decrypted blob.
		// We only need `_type` + `_project_id` so this is a light JSON
		// parse per connection — acceptable for the N ~ 100 case.
		if _, enc, err := s.store.GetConnection(userID, c.ID); err == nil {
			if plain, derr := Decrypt(s.secret, enc); derr == nil {
				var blob map[string]string
				if json.Unmarshal([]byte(plain), &blob) == nil && blob[credKeyType] == "child" {
					row.IsGroupChild = true
					row.ExternalProjectID = blob[credKeyProjectID]
				}
			}
		}
		enriched = append(enriched, row)
	}
	if enriched == nil {
		enriched = []ConnectionWithTools{}
	}
	writeJSON(w, enriched)
}

// DELETE /connections/:id
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	// Load the row first so we know the source and can revoke upstream.
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Cascade-protect: refuse to delete when one or more app installs
	// have this connection bound. The operator must unbind the apps
	// first (uninstall, or rebind to a different connection). 409
	// ships the dependents list so the dashboard can render a useful
	// error. ?force=1 overrides — for power users who know what
	// they're doing; the dependent apps will quietly degrade on
	// their next ExecuteIntegrationTool call.
	if r.URL.Query().Get("force") != "1" {
		if deps, derr := s.dependentsOfConnection(connID); derr == nil && len(deps) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":      "connection has dependents",
				"message":    formatDependents(deps),
				"dependents": deps,
				"hint":       "Unbind the dependent apps first, or pass ?force=1 to override (apps will degrade).",
			})
			return
		}
	}

	// Suite master rows cascade-delete their children. We route
	// straight to the suite handler so the cleanup path is shared
	// between the UI's "Disconnect all" button and a plain DELETE
	// /connections/:id call on a master row.
	if IsMasterSlug(conn.AppSlug) {
		// Rewrite the URL path so the suite handler can extract
		// groupID from it, then delegate.
		gid := GroupIDFromMasterSlug(conn.AppSlug)
		newReq := r.Clone(r.Context())
		newReq.URL.Path = "/integrations/groups/" + gid + "/master"
		q := newReq.URL.Query()
		q.Set("project_id", conn.ProjectID)
		newReq.URL.RawQuery = q.Encode()
		s.handleDeleteGroupMaster(w, newReq)
		return
	}

	// Cascade delete any subscriptions bound to this connection. When the
	// app template has webhook registration config we also try to
	// unregister each external webhook upstream — best-effort, we don't
	// block the local delete on a 4xx/5xx from a third-party.
	if subs, _ := s.store.ListSubscriptionsByConnection(userID, connID); len(subs) > 0 {
		app := s.catalog.Get(conn.AppSlug)
		for _, sub := range subs {
			if app != nil && app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.DeletePath != "" && sub.ExternalWebhookID != "" {
				s.unregisterUpstreamWebhook(conn, app, sub.ExternalWebhookID)
			}
			s.store.DeleteSubscription(userID, sub.ID)
		}
	}

	switch conn.Source {
	case "composio":
		if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil && conn.ExternalID != "" {
			if rerr := client.RevokeConnection(conn.ExternalID); rerr != nil {
				fmt.Fprintf(os.Stderr, "composio revoke %s: %v\n", conn.ExternalID, rerr)
			}
		}
		s.store.DeleteConnection(userID, connID)
		if rerr := s.reconcileComposioMCPServer(userID, conn.ProviderID, conn.ProjectID); rerr != nil {
			fmt.Fprintf(os.Stderr, "composio reconcile: %v\n", rerr)
		}
	default:
		s.store.DeleteMCPServerByConnection(connID)
		s.store.DeleteConnection(userID, connID)
	}

	// A connection just disappeared — installs that had it bound
	// could now be eligible for an opt-in nudge if their manifest
	// includes the same role and another candidate exists.
	s.recomputePendingOptions()

	writeJSON(w, map[string]string{"status": "deleted"})
}

// GET /connections/:id/tools
func (s *Server) handleConnectionTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/tools")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}

	// Return tools with prefixed names
	type ToolInfo struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Method      string         `json:"method"`
		Path        string         `json:"path"`
		InputSchema map[string]any `json:"input_schema"`
	}
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tools []ToolInfo
	for _, t := range app.Tools {
		tools = append(tools, ToolInfo{
			Name:        prefix + "_" + t.Name,
			Description: fmt.Sprintf("[%s] %s", app.Name, t.Description),
			Method:      t.Method,
			Path:        t.Path,
			InputSchema: t.InputSchema,
		})
	}
	writeJSON(w, tools)
}

// POST /connections/:id/execute
func (s *Server) handleExecuteTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/execute")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}

	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Find the tool. Accept the bare name, the canonical MCP-prefixed form
	// (for this specific connection), or the legacy app-slug-prefixed form
	// so scenarios created before unique MCP names keep working.
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == body.Tool || prefix+"_"+t.Name == body.Tool || conn.AppSlug+"_"+t.Name == body.Tool {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}

	// Decrypt credentials
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	var credentials map[string]string
	json.Unmarshal([]byte(plain), &credentials)

	// Resolve master/child indirection + project binding. For legacy
	// connections (no `_type` key) this is a no-op passthrough.
	ctx, err := s.resolveConnectionContext(userID, app, credentials, body.Input)
	if err != nil {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "dashboard", tool.Name, body.Input, nil, err))
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Auto-refresh OAuth tokens on 401 + persist back to DB. For child
	// connections that resolved through a master, persist the refreshed
	// tokens to the master row so all siblings inherit the update.
	persistTargetID := connID
	if ctx.MasterConnID != 0 {
		persistTargetID = ctx.MasterConnID
	}
	persist := func(updated map[string]string) error {
		blob, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(s.secret, string(blob))
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionCredentials(persistTargetID, enc)
	}
	environmentID := r.Header.Get("X-Apteva-Environment-Id")
	if environmentID == "" {
		environmentID = r.Header.Get("X-Apteva-Environment-Id")
	}
	if environmentID == "" {
		err = s.prepareIntegrationExternalFetch(ctx.App, tool, ctx.Credentials, ctx.Input)
	}
	var result *ExecuteResult
	if err == nil {
		result, err = executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, environmentID, persist)
	}
	if err != nil {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "dashboard", tool.Name, body.Input, nil, err))
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "dashboard", tool.Name, body.Input, result, nil))

	writeJSON(w, result)
}

// handleCreateScopedMCP creates an additional mcp_servers row over an
// existing connection with a specific tool subset. Lets the dashboard
// give different scopes to different sub-threads (read-only worker,
// full-access main, etc.) without re-authorizing the upstream service.
//
// Body: { name: "google-sheets-readonly", allowed_tools: ["read_range", ...] }
//
// Validation:
//   - name is required and unique within the project
//   - allowed_tools must be non-empty (otherwise the user should just
//     use the default unscoped MCP that gets created automatically)
//   - every tool name must exist on the underlying app template
//
// The new row gets a fresh URL keyed on its mcp_servers.id, so two
// scoped views over the same connection have distinct routing.
func (s *Server) handleCreateScopedMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	// Path: /connections/:id/mcp
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/mcp")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}

	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	var body struct {
		Name         string   `json:"name"`
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if len(body.AllowedTools) == 0 {
		http.Error(w, "allowed_tools required — use the default mcp_server if you want all tools", http.StatusBadRequest)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found in catalog", http.StatusNotFound)
		return
	}

	// Validate every tool name against the app template. Accept bare
	// names, canonical-MCP-prefixed names (for this specific connection),
	// and the legacy app-slug-prefixed form. The agent might emit any of
	// these depending on how it discovered the tool.
	canonPrefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	valid := make(map[string]bool, len(app.Tools)*3)
	for _, t := range app.Tools {
		valid[t.Name] = true
		valid[canonPrefix+"_"+t.Name] = true
		valid[conn.AppSlug+"_"+t.Name] = true
	}
	var bad []string
	for _, name := range body.AllowedTools {
		if !valid[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		http.Error(w, "unknown tool name(s): "+strings.Join(bad, ", "), http.StatusBadRequest)
		return
	}

	// Insert the scoped row.
	row, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID:       userID,
		Name:         body.Name,
		Description:  fmt.Sprintf("Scoped view of %s — %d tools", conn.AppName, len(body.AllowedTools)),
		Source:       "local",
		Transport:    "http",
		ConnectionID: conn.ID,
		ProjectID:    conn.ProjectID,
		AllowedTools: body.AllowedTools,
		ToolCount:    len(app.Tools),
	})
	if err != nil {
		http.Error(w, "create scoped mcp_server: "+err.Error(), http.StatusInternalServerError)
		return
	}

	serverPort := s.port
	if serverPort == "" {
		serverPort = "8080"
	}
	writeJSON(w, map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"connection_id": conn.ID,
		"app_slug":      conn.AppSlug,
		"allowed_tools": body.AllowedTools,
		"url":           fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, row.ID),
	})
}
