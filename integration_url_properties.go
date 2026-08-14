package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const integrationURLPropertyPrefix = "integration.url_properties."

var verificationFilenameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.txt$`)

type integrationURLPropertyState struct {
	Type                         string `json:"type"`
	Value                        string `json:"value"`
	VerificationMethod           string `json:"verification_method"`
	VerificationFilename         string `json:"verification_filename"`
	VerificationContent          string `json:"verification_content"`
	HostingStatus                string `json:"hosting_status"`
	OperatorConfirmedAt          string `json:"operator_confirmed_at,omitempty"`
	LastSuccessfulProviderPullAt string `json:"last_successful_provider_pull_at,omitempty"`
	LastTestAt                   string `json:"last_test_at,omitempty"`
	RelayStatus                  string `json:"relay_status"`
	UpdatedAt                    string `json:"updated_at"`
}

type integrationRelayClaims struct {
	Version     int      `json:"v"`
	SourceURL   string   `json:"source_url"`
	Integration string   `json:"integration"`
	Property    string   `json:"property"`
	Fingerprint string   `json:"fingerprint"`
	Filename    string   `json:"filename"`
	ExpiresAt   int64    `json:"expires_at"`
	MaxBytes    int64    `json:"max_bytes,omitempty"`
	MIMETypes   []string `json:"mime_types,omitempty"`
}

func integrationOAuthFingerprint(credentials map[string]string) string {
	var value string
	for _, key := range []string{"client_id", "clientId", "client_key", "clientKey"} {
		if value = strings.TrimSpace(credentials[key]); value != "" {
			break
		}
	}
	if value == "" {
		return "default"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func integrationURLPropertyKey(slug, property, fingerprint string) string {
	return integrationURLPropertyPrefix + slug + "." + property + "." + fingerprint
}

func findURLProperty(app *AppTemplate, id string) *IntegrationURLProperty {
	if app == nil {
		return nil
	}
	for i := range app.URLProperties {
		if app.URLProperties[i].ID == id {
			return &app.URLProperties[i]
		}
	}
	return nil
}

func (s *Server) urlPropertyContext(r *http.Request, slug, property string) (*Connection, map[string]string, *AppTemplate, *IntegrationURLProperty, string, error) {
	connID, err := strconv.ParseInt(r.URL.Query().Get("connection_id"), 10, 64)
	if err != nil || connID <= 0 {
		return nil, nil, nil, nil, "", errors.New("connection_id required")
	}
	conn, encrypted, err := s.store.GetConnection(getUserID(r), connID)
	if err != nil || conn == nil || conn.AppSlug != slug {
		return nil, nil, nil, nil, "", errors.New("connection not found")
	}
	app := s.catalog.Get(slug)
	def := findURLProperty(app, property)
	if app == nil || (property != "" && def == nil) {
		return nil, nil, nil, nil, "", errors.New("URL property not found")
	}
	credentials := map[string]string{}
	if encrypted != "" {
		plain, err := Decrypt(s.secret, encrypted)
		if err != nil || json.Unmarshal([]byte(plain), &credentials) != nil {
			return nil, nil, nil, nil, "", errors.New("connection credentials unavailable")
		}
	}
	return conn, credentials, app, def, integrationOAuthFingerprint(credentials), nil
}

func (s *Server) readURLPropertyState(slug, property, fingerprint string) integrationURLPropertyState {
	var state integrationURLPropertyState
	_ = json.Unmarshal([]byte(s.store.GetSetting(integrationURLPropertyKey(slug, property, fingerprint))), &state)
	return state
}

func (s *Server) writeURLPropertyState(slug, property, fingerprint string, state integrationURLPropertyState) error {
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.store.SetSetting(integrationURLPropertyKey(slug, property, fingerprint), string(b))
}

func (s *Server) urlPropertyResponse(app *AppTemplate, property IntegrationURLProperty, fingerprint string) map[string]any {
	state := s.readURLPropertyState(app.Slug, property.ID, fingerprint)
	ready := state.HostingStatus == "ready" && state.OperatorConfirmedAt != ""
	return map[string]any{
		"definition":        property,
		"state":             state,
		"ready":             ready,
		"fingerprint":       fingerprint,
		"configured_prefix": strings.TrimRight(s.publicBaseURL(), "/") + "/api/relay/",
	}
}

// handleIntegrationURLProperties dispatches authenticated catalog subroutes:
// GET list, PUT configure, POST test, and POST confirm.
func (s *Server) handleIntegrationURLProperties(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/integrations/catalog/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "url-properties" {
		s.handleGetCatalogApp(w, r)
		return
	}
	slug := parts[0]
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		_, _, app, _, fingerprint, err := s.urlPropertyContext(r, slug, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		properties := make([]map[string]any, 0, len(app.URLProperties))
		for _, property := range app.URLProperties {
			properties = append(properties, s.urlPropertyResponse(app, property, fingerprint))
		}
		writeJSON(w, map[string]any{"integration": slug, "properties": properties})
		return
	}
	property := parts[2]
	_, _, app, def, fingerprint, err := s.urlPropertyContext(r, slug, property)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 3 {
		action = parts[3]
	}
	switch {
	case r.Method == http.MethodPut && action == "":
		var body struct {
			VerificationMethod   string `json:"verification_method"`
			VerificationFilename string `json:"verification_filename"`
			VerificationContent  string `json:"verification_content"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.VerificationMethod != "file" || !verificationFilenameRE.MatchString(body.VerificationFilename) || len(body.VerificationContent) == 0 || len(body.VerificationContent) > 4096 {
			http.Error(w, "a valid .txt verification file (up to 4 KiB) is required", http.StatusBadRequest)
			return
		}
		publicURL, parseErr := url.Parse(s.publicBaseURL())
		if parseErr != nil || !strings.EqualFold(publicURL.Scheme, "https") {
			http.Error(w, "an HTTPS public URL must be configured in Server settings", http.StatusConflict)
			return
		}
		if s.verificationFilenameConflicts(body.VerificationFilename, body.VerificationContent, integrationURLPropertyKey(slug, property, fingerprint)) {
			http.Error(w, "that verification filename is already used with different content", http.StatusConflict)
			return
		}
		state := integrationURLPropertyState{
			Type: "url_prefix", Value: strings.TrimRight(s.publicBaseURL(), "/") + "/api/relay/",
			VerificationMethod: "file", VerificationFilename: body.VerificationFilename,
			VerificationContent: body.VerificationContent, HostingStatus: "configured", RelayStatus: "ready",
		}
		if err := s.writeURLPropertyState(slug, property, fingerprint, state); err != nil {
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.urlPropertyResponse(app, *def, fingerprint))
	case r.Method == http.MethodPost && action == "test":
		state := s.readURLPropertyState(slug, property, fingerprint)
		if state.VerificationFilename == "" {
			http.Error(w, "verification file is not configured", http.StatusConflict)
			return
		}
		testURL := strings.TrimRight(s.publicBaseURL(), "/") + "/api/relay/" + url.PathEscape(state.VerificationFilename)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(testURL)
		if err != nil {
			http.Error(w, "verification URL is not publicly reachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4097))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != state.VerificationContent {
			http.Error(w, "verification URL returned unexpected content", http.StatusBadGateway)
			return
		}
		state.HostingStatus = "ready"
		state.LastTestAt = time.Now().UTC().Format(time.RFC3339)
		_ = s.writeURLPropertyState(slug, property, fingerprint, state)
		writeJSON(w, s.urlPropertyResponse(app, *def, fingerprint))
	case r.Method == http.MethodPost && action == "confirm":
		state := s.readURLPropertyState(slug, property, fingerprint)
		if state.HostingStatus != "ready" {
			http.Error(w, "test the verification URL before confirming", http.StatusConflict)
			return
		}
		state.OperatorConfirmedAt = time.Now().UTC().Format(time.RFC3339)
		_ = s.writeURLPropertyState(slug, property, fingerprint, state)
		writeJSON(w, s.urlPropertyResponse(app, *def, fingerprint))
	default:
		http.Error(w, "unsupported URL property operation", http.StatusMethodNotAllowed)
	}
}

func (s *Server) verificationFilenameConflicts(filename, content, ownKey string) bool {
	rows, err := s.store.db.Query(`SELECT key, value FROM server_settings WHERE key LIKE ?`, integrationURLPropertyPrefix+"%")
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		var state integrationURLPropertyState
		if rows.Scan(&key, &raw) == nil && key != ownKey && json.Unmarshal([]byte(raw), &state) == nil && state.VerificationFilename == filename && state.VerificationContent != content {
			return true
		}
	}
	return false
}

// handleIntegrationRelay serves verification files and encrypted media relay
// tokens. It is intentionally public because providers fetch these URLs.
func (s *Server) handleIntegrationRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/relay/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 {
		s.serveIntegrationVerificationFile(w, r, parts[0])
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	plain, err := Decrypt(s.secret, parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var claims integrationRelayClaims
	if json.Unmarshal([]byte(plain), &claims) != nil || claims.Version != 1 || claims.ExpiresAt <= time.Now().Unix() || path.Base(parts[1]) != parts[1] || claims.Filename != parts[1] {
		http.NotFound(w, r)
		return
	}
	app := s.catalog.Get(claims.Integration)
	if findURLProperty(app, claims.Property) == nil {
		http.NotFound(w, r)
		return
	}
	state := s.readURLPropertyState(claims.Integration, claims.Property, claims.Fingerprint)
	if state.HostingStatus != "ready" || state.OperatorConfirmedAt == "" {
		http.Error(w, "relay is not confirmed", http.StatusForbidden)
		return
	}
	if err := s.validateRelaySource(claims.SourceURL); err != nil {
		http.Error(w, "invalid relay source", http.StatusForbidden)
		return
	}
	s.proxyIntegrationRelay(w, r, claims)
}

func (s *Server) serveIntegrationVerificationFile(w http.ResponseWriter, r *http.Request, filename string) {
	if !verificationFilenameRE.MatchString(filename) {
		http.NotFound(w, r)
		return
	}
	rows, err := s.store.db.Query(`SELECT value FROM server_settings WHERE key LIKE ?`, integrationURLPropertyPrefix+"%")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var state integrationURLPropertyState
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &state) == nil && state.VerificationFilename == filename {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, state.VerificationContent)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) validateRelaySource(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return errors.New("source must be HTTPS")
	}
	base, err := url.Parse(s.publicBaseURL())
	if err != nil || !strings.EqualFold(u.Host, base.Host) {
		return errors.New("source is not from this Apteva instance")
	}
	if !isStorageRelayContentPath(u.Path) {
		return errors.New("source is not a Storage content URL")
	}
	if u.Query().Get("sig") == "" {
		return errors.New("source is unsigned")
	}
	exp, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if exp <= time.Now().Unix() {
		return errors.New("source is expired")
	}
	return nil
}

// isStorageRelayContentPath accepts both Storage URL generations:
//
//	/api/apps/storage/files/<id>/content[/<name>]         (legacy)
//	/api/apps/storage/public/files/<id>/content[/<name>]  (current)
//
// The current public route is the canonical signed URL emitted by
// storage.files_get_url. "public" describes the unauthenticated HTTP route;
// private files are still protected by the sig/exp pair checked below. Keep
// this deliberately limited to content reads: metadata and download routes
// are not valid provider-fetch sources.
func isStorageRelayContentPath(path string) bool {
	for _, prefix := range []string{
		"/api/apps/storage/files/",
		"/api/apps/storage/public/files/",
	} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			return false
		}
		action := rest[slash+1:]
		return action == "content" || strings.HasPrefix(action, "content/")
	}
	return false
}

func validateExternalRelayURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return errors.New("redirect target must be HTTPS")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return errors.New("redirect target cannot be resolved")
	}
	for _, addr := range addrs {
		ip := addr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("redirect target resolves to a private address")
		}
	}
	return nil
}

func (s *Server) proxyIntegrationRelay(w http.ResponseWriter, r *http.Request, claims integrationRelayClaims) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, claims.SourceURL, nil)
	if err != nil {
		http.Error(w, "relay request failed", http.StatusBadGateway)
		return
	}
	for _, header := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(header); value != "" {
			req.Header.Set(header, value)
		}
	}
	client := s.integrationRelayClient()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > 4 {
			return errors.New("too many redirects")
		}
		base, _ := url.Parse(s.publicBaseURL())
		if base != nil && strings.EqualFold(next.URL.Host, base.Host) {
			return nil
		}
		return validateExternalRelayURL(next.Context(), next.URL.String())
	}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "media source unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if claims.MaxBytes > 0 && resp.ContentLength > claims.MaxBytes {
		http.Error(w, "media exceeds provider limit", http.StatusRequestEntityTooLarge)
		return
	}
	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(claims.MIMETypes) > 0 {
		if contentType == "" {
			http.Error(w, "media source did not provide a content type", http.StatusUnsupportedMediaType)
			return
		}
		allowed := false
		for _, candidate := range claims.MIMETypes {
			allowed = allowed || strings.EqualFold(candidate, contentType)
		}
		if !allowed {
			http.Error(w, "media type is not allowed", http.StatusUnsupportedMediaType)
			return
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		state := s.readURLPropertyState(claims.Integration, claims.Property, claims.Fingerprint)
		state.LastSuccessfulProviderPullAt = time.Now().UTC().Format(time.RFC3339)
		_ = s.writeURLPropertyState(claims.Integration, claims.Property, claims.Fingerprint, state)
	}
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Cache-Control"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, claims.Filename))
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodGet {
		if claims.MaxBytes > 0 && resp.ContentLength < 0 {
			_, _ = io.Copy(w, io.LimitReader(resp.Body, claims.MaxBytes))
		} else {
			_, _ = io.Copy(w, resp.Body)
		}
	}
}

func (s *Server) integrationRelayClient() *http.Client {
	if s.integrationRelayTransport != nil {
		return &http.Client{Transport: s.integrationRelayTransport, Timeout: 2 * time.Minute}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	base, _ := url.Parse(s.publicBaseURL())
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if base != nil && strings.EqualFold(host, base.Hostname()) {
			return dialer.DialContext(ctx, network, address)
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return nil, errors.New("relay destination cannot be resolved")
		}
		for _, addr := range addrs {
			ip := addr.IP
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		return nil, errors.New("relay destination resolves only to private addresses")
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute}
}

func (s *Server) mintIntegrationRelayURL(source, slug, property, fingerprint, filename string, spec ExternalFetchInput) (string, error) {
	if err := s.validateRelaySource(source); err != nil {
		return "", err
	}
	ttl := spec.TTLSeconds
	if ttl <= 0 || ttl > 7200 {
		ttl = 7200
	}
	expiresAt := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	if sourceURL, err := url.Parse(source); err == nil {
		if sourceExpiry, _ := strconv.ParseInt(sourceURL.Query().Get("exp"), 10, 64); sourceExpiry > 0 && sourceExpiry < expiresAt {
			expiresAt = sourceExpiry
		}
	}
	claims := integrationRelayClaims{Version: 1, SourceURL: source, Integration: slug, Property: property, Fingerprint: fingerprint, Filename: safeRelayFilename(filename), ExpiresAt: expiresAt, MaxBytes: spec.MaxBytes, MIMETypes: spec.MIMETypes}
	b, _ := json.Marshal(claims)
	token, err := Encrypt(s.secret, string(b))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.publicBaseURL(), "/") + "/api/relay/" + token + "/" + url.PathEscape(claims.Filename), nil
}

func safeRelayFilename(filename string) string {
	filename = path.Base(strings.TrimSpace(filename))
	if filename == "." || filename == "/" || filename == "" {
		return "media"
	}
	var out strings.Builder
	for _, r := range filename {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
		if out.Len() >= 128 {
			break
		}
	}
	if out.Len() == 0 {
		return "media"
	}
	return out.String()
}

// prepareIntegrationExternalFetch rewrites only catalog-declared URL inputs.
// It mutates input in place after context resolution, immediately before the
// provider request is executed.
func (s *Server) prepareIntegrationExternalFetch(app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any) error {
	if app == nil || tool == nil || len(tool.ExternalFetchInputs) == 0 {
		return nil
	}
	base, err := url.Parse(s.publicBaseURL())
	if err != nil || !strings.EqualFold(base.Scheme, "https") {
		return errors.New("external media delivery requires an HTTPS public URL in Server settings")
	}
	fingerprint := integrationOAuthFingerprint(credentials)
	for _, spec := range tool.ExternalFetchInputs {
		if spec.When != nil {
			actual, ok := nestedInputValue(input, spec.When.Path)
			if !ok || fmt.Sprint(actual) != fmt.Sprint(spec.When.Equals) {
				continue
			}
		}
		if findURLProperty(app, spec.Property) == nil {
			return fmt.Errorf("integration %s has no URL property %q", app.Slug, spec.Property)
		}
		state := s.readURLPropertyState(app.Slug, spec.Property, fingerprint)
		if state.HostingStatus != "ready" || state.OperatorConfirmedAt == "" {
			return fmt.Errorf("%s media delivery is not ready; configure and verify %s in Integrations", app.Name, spec.Property)
		}
		if err := rewriteNestedURLValues(input, spec.Path, func(source string) (string, error) {
			prefix := strings.TrimRight(s.publicBaseURL(), "/") + "/api/relay/"
			if strings.HasPrefix(source, prefix) {
				return source, nil
			}
			u, err := url.Parse(source)
			if err != nil {
				return "", errors.New("invalid media URL")
			}
			filename := path.Base(u.Path)
			if filename == "." || filename == "/" || filename == "" {
				filename = "media"
			}
			return s.mintIntegrationRelayURL(source, app.Slug, spec.Property, fingerprint, filename, spec)
		}); err != nil {
			return fmt.Errorf("prepare %s: %w", spec.Path, err)
		}
	}
	return nil
}

func nestedInputValue(root map[string]any, dotted string) (any, bool) {
	var current any = root
	for _, segment := range strings.Split(dotted, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[strings.TrimSuffix(segment, "[]")]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func rewriteNestedURLValues(root map[string]any, dotted string, rewrite func(string) (string, error)) error {
	segments := strings.Split(dotted, ".")
	var visit func(any, int) error
	visit = func(current any, index int) error {
		if index >= len(segments) {
			return nil
		}
		segment := segments[index]
		array := strings.HasSuffix(segment, "[]")
		key := strings.TrimSuffix(segment, "[]")
		m, ok := current.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", strings.Join(segments[:index], "."))
		}
		value, ok := m[key]
		if !ok {
			return fmt.Errorf("required URL input is missing")
		}
		if index == len(segments)-1 {
			if array {
				switch values := value.(type) {
				case []string:
					for i, source := range values {
						if source == "" {
							return errors.New("URL array contains an empty value")
						}
						updated, err := rewrite(source)
						if err != nil {
							return err
						}
						values[i] = updated
					}
					m[key] = values
					return nil
				case []any:
					for i, raw := range values {
						source, ok := raw.(string)
						if !ok || source == "" {
							return errors.New("URL array contains a non-string value")
						}
						updated, err := rewrite(source)
						if err != nil {
							return err
						}
						values[i] = updated
					}
					m[key] = values
					return nil
				default:
					return errors.New("URL input must be an array")
				}
			}
			source, ok := value.(string)
			if !ok || source == "" {
				return errors.New("URL input must be a non-empty string")
			}
			updated, err := rewrite(source)
			if err != nil {
				return err
			}
			m[key] = updated
			return nil
		}
		return visit(value, index+1)
	}
	return visit(root, 0)
}
