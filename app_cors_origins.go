package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxAppCORSRegistrationKeyRunes = 255
	maxAppCORSOriginsPerClient     = 100
	appCORSPreflightPlatform       = "platform"
	appCORSPreflightApp            = "app"
)

type appCORSOriginsRequest struct {
	Origins     []string `json:"origins"`
	Preflight   string   `json:"preflight,omitempty"`
	Credentials *bool    `json:"credentials,omitempty"`
}

type appCORSOriginRegistration struct {
	Key     string   `json:"key"`
	Origins []string `json:"origins"`
}

type appCORSOriginPolicyRegistration struct {
	Key         string   `json:"key"`
	Origins     []string `json:"origins"`
	Preflight   string   `json:"preflight"`
	Credentials bool     `json:"credentials"`
}

// handleCallbackCORSOrigins lets an authenticated sidecar maintain the exact
// browser origins for one of its own clients. Registrations are intentionally
// install-scoped and only affect that install's /api/apps/<name>/... surface;
// they never grant access to platform routes or another app.
//
//	GET    /apps/callback/cors-origins
//	GET    /apps/callback/cors-origins/:key
//	PUT    /apps/callback/cors-origins/:key  {"origins":[...], "preflight":"platform|app", "credentials":true|false}
//	DELETE /apps/callback/cors-origins/:key
//
// PUT replaces the complete set atomically. An empty set has the same durable
// result as DELETE, which makes app-side reconciliation idempotent.
func (s *Server) handleCallbackCORSOrigins(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		registrations, err := s.listAppCORSOriginRegistrations(installID, "")
		if err != nil {
			http.Error(w, "list CORS origins", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"registrations": registrations})
		return
	}
	if len(parts) != 1 {
		http.Error(w, "invalid CORS origin registration path", http.StatusNotFound)
		return
	}
	key, err := validateAppCORSRegistrationKey(parts[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		registrations, err := s.listAppCORSOriginRegistrations(installID, key)
		if err != nil {
			http.Error(w, "list CORS origins", http.StatusInternalServerError)
			return
		}
		if len(registrations) == 0 {
			http.Error(w, "CORS origin registration not found", http.StatusNotFound)
			return
		}
		writeJSON(w, registrations[0])
	case http.MethodPut:
		var body appCORSOriginsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		origins, err := normalizeAppCORSOrigins(body.Origins)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		preflight, err := normalizeAppCORSPreflight(body.Preflight)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		credentials := true
		if body.Credentials != nil {
			credentials = *body.Credentials
		}
		if err := s.replaceAppCORSOrigins(installID, key, origins, preflight, credentials); err != nil {
			http.Error(w, "save CORS origins", http.StatusInternalServerError)
			return
		}
		writeJSON(w, appCORSOriginPolicyRegistration{
			Key: key, Origins: origins, Preflight: preflight, Credentials: credentials,
		})
	case http.MethodDelete:
		if _, err := s.store.db.Exec(
			`DELETE FROM app_cors_origins WHERE install_id=? AND registration_key=?`, installID, key,
		); err != nil {
			http.Error(w, "delete CORS origins", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PUT, or DELETE only", http.StatusMethodNotAllowed)
	}
}

func normalizeAppCORSPreflight(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return appCORSPreflightPlatform, nil
	}
	if mode != appCORSPreflightPlatform && mode != appCORSPreflightApp {
		return "", errors.New(`preflight must be "platform" or "app"`)
	}
	return mode, nil
}

func validateAppCORSRegistrationKey(raw string) (string, error) {
	// net/http has already decoded URL.Path before the callback router splits
	// it, so decoding again would reject valid client ids containing a literal
	// percent sign.
	key := strings.TrimSpace(raw)
	if key == "" || len([]rune(key)) > maxAppCORSRegistrationKeyRunes {
		return "", errors.New("registration key is required and must be at most 255 characters")
	}
	for _, r := range key {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return "", errors.New("registration key cannot contain slashes or control characters")
		}
	}
	return key, nil
}

func normalizeAppCORSOrigins(raw []string) ([]string, error) {
	if len(raw) > maxAppCORSOriginsPerClient {
		return nil, fmt.Errorf("at most %d origins are allowed per registration", maxAppCORSOriginsPerClient)
	}
	seen := map[string]bool{}
	origins := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		parsed, err := url.Parse(item)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, fmt.Errorf("invalid origin %q", item)
		}
		origin := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
		if !seen[origin] {
			seen[origin] = true
			origins = append(origins, origin)
		}
	}
	return origins, nil
}

func (s *Server) replaceAppCORSOrigins(installID int64, key string, origins []string, preflight string, credentials bool) error {
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM app_cors_origins WHERE install_id=? AND registration_key=?`, installID, key); err != nil {
		return err
	}
	for _, origin := range origins {
		if _, err := tx.Exec(
			`INSERT INTO app_cors_origins(install_id, registration_key, origin, preflight_mode, credentials) VALUES(?,?,?,?,?)`,
			installID, key, origin, preflight, boolToInt(credentials),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) listAppCORSOriginRegistrations(installID int64, key string) ([]appCORSOriginPolicyRegistration, error) {
	query := `SELECT registration_key, origin, preflight_mode, credentials FROM app_cors_origins WHERE install_id=?`
	args := []any{installID}
	if key != "" {
		query += ` AND registration_key=?`
		args = append(args, key)
	}
	query += ` ORDER BY registration_key, origin`
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*appCORSOriginPolicyRegistration{}
	order := []string{}
	for rows.Next() {
		var registrationKey, origin, preflight string
		var credentials int
		if err := rows.Scan(&registrationKey, &origin, &preflight, &credentials); err != nil {
			return nil, err
		}
		registration := byKey[registrationKey]
		if registration == nil {
			order = append(order, registrationKey)
			registration = &appCORSOriginPolicyRegistration{
				Key: registrationKey, Preflight: preflight, Credentials: credentials != 0,
			}
			byKey[registrationKey] = registration
		}
		registration.Origins = append(registration.Origins, origin)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]appCORSOriginPolicyRegistration, 0, len(order))
	for _, registrationKey := range order {
		out = append(out, *byKey[registrationKey])
	}
	return out, nil
}

// dynamicAppCORSOriginAllowed is the complete live origin resolver used by the
// outer preflight middleware. Browser preflights carry no credentials, so it
// uses durable server-side registrations and still leaves authentication and
// app-level client validation to the subsequent real request.
func (s *Server) dynamicAppCORSOriginAllowed(r *http.Request, origin string) bool {
	return s.dynamicAppCORSPolicy(r, origin).Allowed
}

// dynamicAppCORSPolicy preserves the broad platform/admin and delegated-key
// behavior, while app-owned registrations can opt into sidecar-managed CORS.
// A matching app registration is authoritative for that app route: restrictive
// delegated-preflight and credential settings must not be broadened by a
// platform-wide grant for the same origin.
func (s *Server) dynamicAppCORSPolicy(r *http.Request, origin string) dynamicCORSPolicy {
	if registered := s.registeredAppCORSPolicy(r, origin); registered.Allowed {
		return registered
	}
	if s.platformCORSOriginAllowed(origin) ||
		s.delegatedAppCORSOriginAllowed(r, origin) ||
		s.publicClientCORSOriginAllowed(r, origin) {
		return dynamicCORSPolicy{Allowed: true, Credentials: true}
	}
	return dynamicCORSPolicy{}
}

func (s *Server) platformCORSOriginAllowed(origin string) bool {
	if s == nil || s.store == nil || strings.TrimSpace(origin) == "" {
		return false
	}
	var found int
	err := s.store.db.QueryRow(
		`SELECT 1 FROM platform_cors_origins WHERE origin=? LIMIT 1`, origin,
	).Scan(&found)
	return err == nil && found == 1
}

func (s *Server) publicClientCORSOriginAllowed(r *http.Request, origin string) bool {
	if s == nil || s.store == nil || r == nil {
		return false
	}
	appName, appPath, ok := splitAppProxyPath(r.URL.Path)
	if !ok || isAppManagementRoute(appName) || appPath != "/mcp" || corsRequestedMethod(r) != http.MethodPost {
		return false
	}
	rows, err := s.store.db.Query(`
		SELECT COALESCE(allowed_origins, '[]'), COALESCE(scopes, '[]')
		FROM api_keys
		WHERE kind='public_client'
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var allowed, scopes string
		if rows.Scan(&allowed, &scopes) == nil && delegatedUserScopeHasApp(scopes, appName) && publicClientOriginAllowed(allowed, origin) {
			return true
		}
	}
	return false
}

func corsRequestedMethod(r *http.Request) string {
	if r.Method == http.MethodOptions {
		return strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	}
	return r.Method
}

func (s *Server) registeredAppCORSOriginAllowed(r *http.Request, origin string) bool {
	return s.registeredAppCORSPolicy(r, origin).Allowed
}

func (s *Server) registeredAppCORSPolicy(r *http.Request, origin string) dynamicCORSPolicy {
	if s == nil || s.store == nil || r == nil || strings.TrimSpace(origin) == "" {
		return dynamicCORSPolicy{}
	}
	appName, appPath, ok := splitAppProxyPath(r.URL.Path)
	if !ok || isAppManagementRoute(appName) {
		return dynamicCORSPolicy{}
	}
	installID, ok := s.resolveCORSAppInstall(appName, appPath, r)
	if !ok {
		return dynamicCORSPolicy{}
	}
	return s.registeredAppCORSInstallPolicy(installID, origin)
}

// dynamicIngressCORSPolicy is the custom-host counterpart of
// dynamicAppCORSPolicy. HostRouter already resolved the route owner, so it can
// enforce that installation's policy without synthesizing an /apps/... path.
func (s *Server) dynamicIngressCORSPolicy(installID int64, origin string) dynamicCORSPolicy {
	if registered := s.registeredAppCORSInstallPolicy(installID, origin); registered.Allowed {
		return registered
	}
	if s.platformCORSOriginAllowed(origin) {
		return dynamicCORSPolicy{Allowed: true, Credentials: true}
	}
	return dynamicCORSPolicy{}
}

func (s *Server) registeredAppCORSInstallPolicy(installID int64, origin string) dynamicCORSPolicy {
	if s == nil || s.store == nil || installID <= 0 || strings.TrimSpace(origin) == "" {
		return dynamicCORSPolicy{}
	}
	rows, err := s.store.db.Query(`
		SELECT preflight_mode, credentials FROM app_cors_origins
		WHERE install_id=? AND origin=?`, installID, origin)
	if err != nil {
		return dynamicCORSPolicy{}
	}
	defer rows.Close()
	policy := dynamicCORSPolicy{Credentials: true}
	for rows.Next() {
		var preflight string
		var credentials int
		if rows.Scan(&preflight, &credentials) != nil {
			return dynamicCORSPolicy{}
		}
		policy.Allowed = true
		// Requests do not identify a registration key. If multiple clients
		// share an origin, apply the intersection: any credential-free entry
		// keeps the installation response credential-free.
		policy.Credentials = policy.Credentials && credentials != 0
		// App-managed policy is the more restrictive choice for an
		// install/origin shared by multiple registration keys: the sidecar
		// still has to approve the concrete route before the browser proceeds.
		policy.DelegatePreflight = policy.DelegatePreflight || preflight == appCORSPreflightApp
	}
	if rows.Err() != nil {
		return dynamicCORSPolicy{}
	}
	if !policy.Allowed {
		policy.Credentials = false
	}
	return policy
}

// resolveCORSAppInstall mirrors the proxy's explicit install/project routing,
// but resolves from the database because preflight occurs before auth and proxy
// dispatch. With no selector, only a global install is eligible; we never pick
// an arbitrary project install by app name.
func (s *Server) resolveCORSAppInstall(appName, appPath string, r *http.Request) (int64, bool) {
	var selectedID int64
	if _, pathID, hasSelector, err := splitAppInstallSelector(appPath); err != nil {
		return 0, false
	} else if hasSelector {
		selectedID = pathID
	}
	q := r.URL.Query()
	if rawInstallID := strings.TrimSpace(q.Get("install_id")); rawInstallID != "" {
		queryID, err := strconv.ParseInt(rawInstallID, 10, 64)
		if err != nil || queryID <= 0 || (selectedID > 0 && selectedID != queryID) {
			return 0, false
		}
		selectedID = queryID
	}
	if selectedID > 0 {
		var id int64
		err := s.store.db.QueryRow(`
			SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id
			WHERE i.id=? AND a.name=? AND i.status='running'`, selectedID, appName,
		).Scan(&id)
		return id, err == nil
	}

	projectValues, hasProject := q["project_id"]
	projectID := strings.TrimSpace(q.Get("project_id"))
	if hasProject && (len(projectValues) == 0 || projectID == "") {
		return 0, false
	}
	var id int64
	var err error
	if projectID == "" {
		err = s.store.db.QueryRow(`
			SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id
			WHERE a.name=? AND COALESCE(i.project_id,'')='' AND i.status='running'
			LIMIT 1`, appName,
		).Scan(&id)
	} else {
		err = s.store.db.QueryRow(`
			SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id
			WHERE a.name=? AND i.status='running'
			  AND COALESCE(i.project_id,'') IN (?, '')
			ORDER BY CASE WHEN COALESCE(i.project_id,'')=? THEN 0 ELSE 1 END
			LIMIT 1`, appName, projectID, projectID,
		).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return 0, false
	}
	return id, true
}
