package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ingressPermRead  = "platform.ingress.read"
	ingressPermWrite = "platform.ingress.write"
)

type IngressRoute struct {
	ID             int64  `json:"id"`
	Hostname       string `json:"hostname"`
	Target         string `json:"target"`
	ProjectID      string `json:"project_id"`
	OwnerInstallID int64  `json:"owner_install_id"`
	OwnerKind      string `json:"owner_kind"`
	CertFQDN       string `json:"cert_fqdn,omitempty"`
	AllowHTTP      bool   `json:"allow_http"`
	TLSMode        string `json:"tls_mode"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

type IngressCertCacheInfo struct {
	FQDN      string `json:"fqdn"`
	Status    string `json:"status"`
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Issuer    string `json:"issuer,omitempty"`
	CachePath string `json:"cache_path,omitempty"`
	RouteHost string `json:"route_host,omitempty"`
	TLSMode   string `json:"tls_mode,omitempty"`
	Error     string `json:"error,omitempty"`
}

type IngressExposeRequest struct {
	Hostname       string `json:"hostname"`
	Target         string `json:"target"`
	ProjectID      string `json:"project_id,omitempty"`
	OwnerInstallID int64  `json:"owner_install_id,omitempty"`
	OwnerKind      string `json:"owner_kind,omitempty"`
	CertFQDN       string `json:"cert_fqdn,omitempty"`
	AllowHTTP      bool   `json:"allow_http,omitempty"`
	TLSMode        string `json:"tls_mode,omitempty"`
	TLS            string `json:"tls,omitempty"`
}

func normalizeIngressHostname(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	if host == "" {
		return "", errors.New("hostname required")
	}
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/?#[]@ \t\r\n") {
		return "", errors.New("hostname must not include scheme, path, credentials, or whitespace")
	}
	if strings.HasPrefix(host, "*.") || strings.Contains(host, "*") {
		return "", errors.New("wildcard hostnames require DNS-01 delegation and are not supported by server-native HTTP-01 ingress yet")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "", errors.New("hostname must not include a port")
	}
	if strings.Contains(host, ":") {
		return "", errors.New("hostname must be a DNS name, not an IP literal")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("hostname must be a fully-qualified domain name")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("invalid hostname label %q", label)
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", fmt.Errorf("invalid hostname character %q", r)
			}
		}
	}
	return host, nil
}

func normalizeIngressTLSMode(req IngressExposeRequest) string {
	mode := strings.ToLower(strings.TrimSpace(req.TLSMode))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(req.TLS))
	}
	switch mode {
	case "", "auto", "managed", "on":
		return "auto"
	case "off", "none", "http":
		return "off"
	default:
		return mode
	}
}

func validateIngressTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("target required")
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("target must be an absolute http://, https://, or app:// URL")
	}
	switch u.Scheme {
	case "http", "https", "app":
		return nil
	default:
		return fmt.Errorf("unsupported target scheme %q", u.Scheme)
	}
}

func ingressInstallProjectID(s *Server, installID int64) string {
	if s == nil || s.store == nil || installID <= 0 {
		return ""
	}
	var projectID string
	_ = s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&projectID)
	return projectID
}

func (s *Server) ExposeIngressRoute(req IngressExposeRequest) (*IngressRoute, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("server store unavailable")
	}
	host, err := normalizeIngressHostname(req.Hostname)
	if err != nil {
		return nil, err
	}
	if err := validateIngressTarget(req.Target); err != nil {
		return nil, err
	}
	tlsMode := normalizeIngressTLSMode(req)
	if tlsMode != "auto" && tlsMode != "off" {
		return nil, fmt.Errorf("unsupported tls mode %q", tlsMode)
	}
	certFQDN := strings.TrimSpace(strings.ToLower(req.CertFQDN))
	if tlsMode == "auto" && certFQDN == "" {
		certFQDN = host
	}
	if certFQDN != "" {
		certHost, err := normalizeIngressHostname(certFQDN)
		if err != nil {
			return nil, fmt.Errorf("cert_fqdn: %w", err)
		}
		certFQDN = certHost
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" && req.OwnerInstallID > 0 {
		projectID = ingressInstallProjectID(s, req.OwnerInstallID)
	}
	ownerKind := strings.TrimSpace(req.OwnerKind)
	if ownerKind == "" {
		if req.OwnerInstallID > 0 {
			ownerKind = "app"
		} else {
			ownerKind = "operator"
		}
	}

	var existingOwner int64
	err = s.store.db.QueryRow(`SELECT owner_install_id FROM ingress_routes WHERE hostname=?`, host).Scan(&existingOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && req.OwnerInstallID > 0 && existingOwner > 0 && existingOwner != req.OwnerInstallID {
		return nil, fmt.Errorf("hostname %q is already owned by install %d", host, existingOwner)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.store.db.Exec(`
		INSERT INTO ingress_routes
			(hostname, target, project_id, owner_install_id, owner_kind, cert_fqdn, allow_http, tls_mode, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET
			target=excluded.target,
			project_id=excluded.project_id,
			owner_install_id=excluded.owner_install_id,
			owner_kind=excluded.owner_kind,
			cert_fqdn=excluded.cert_fqdn,
			allow_http=excluded.allow_http,
			tls_mode=excluded.tls_mode,
			status='active',
			updated_at=excluded.updated_at
	`, host, strings.TrimSpace(req.Target), projectID, req.OwnerInstallID, ownerKind, certFQDN, ingressBoolToInt(req.AllowHTTP), tlsMode, now, now)
	if err != nil {
		return nil, err
	}
	route, err := s.GetIngressRoute(host)
	if err != nil {
		return nil, err
	}
	if s.routeCache != nil {
		s.routeCache.Apply("updated", route.Hostname, route.Target, route.CertFQDN, route.AllowHTTP, route.OwnerInstallID, route.OwnerKind)
	}
	if s.ingressCerts != nil && route.CertFQDN != "" && route.TLSMode != "off" {
		s.ingressCerts.Allow(route.CertFQDN)
	}
	return route, nil
}

func (s *Server) DeleteIngressRoute(hostname string, ownerInstallID int64) error {
	host, err := normalizeIngressHostname(hostname)
	if err != nil {
		return err
	}
	if ownerInstallID > 0 {
		var existingOwner int64
		if err := s.store.db.QueryRow(`SELECT owner_install_id FROM ingress_routes WHERE hostname=?`, host).Scan(&existingOwner); err != nil {
			return err
		}
		if existingOwner > 0 && existingOwner != ownerInstallID {
			return fmt.Errorf("hostname %q is owned by install %d", host, existingOwner)
		}
	}
	if _, err := s.store.db.Exec(`DELETE FROM ingress_routes WHERE hostname=?`, host); err != nil {
		return err
	}
	if s.routeCache != nil {
		s.routeCache.Apply("removed", host, "", "", false, 0, "")
	}
	return nil
}

func (s *Server) GetIngressRoute(hostname string) (*IngressRoute, error) {
	host, err := normalizeIngressHostname(hostname)
	if err != nil {
		return nil, err
	}
	row := s.store.db.QueryRow(`
		SELECT id, hostname, target, project_id, owner_install_id, owner_kind, cert_fqdn,
		       allow_http, tls_mode, status, created_at, updated_at
		FROM ingress_routes WHERE hostname=?
	`, host)
	return scanIngressRoute(row)
}

func (s *Server) ListIngressRoutes(projectID string, ownerInstallID int64) ([]IngressRoute, error) {
	projectID = strings.TrimSpace(projectID)
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case ownerInstallID > 0:
		rows, err = s.store.db.Query(`
			SELECT id, hostname, target, project_id, owner_install_id, owner_kind, cert_fqdn,
			       allow_http, tls_mode, status, created_at, updated_at
			FROM ingress_routes WHERE owner_install_id=? ORDER BY hostname
		`, ownerInstallID)
	case projectID != "":
		rows, err = s.store.db.Query(`
			SELECT id, hostname, target, project_id, owner_install_id, owner_kind, cert_fqdn,
			       allow_http, tls_mode, status, created_at, updated_at
			FROM ingress_routes WHERE project_id=? ORDER BY hostname
		`, projectID)
	default:
		rows, err = s.store.db.Query(`
			SELECT id, hostname, target, project_id, owner_install_id, owner_kind, cert_fqdn,
			       allow_http, tls_mode, status, created_at, updated_at
			FROM ingress_routes ORDER BY hostname
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngressRoute
	for rows.Next() {
		r, err := scanIngressRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Server) loadIngressRoutesForRouter() []Route {
	routes, err := s.ListIngressRoutes("", 0)
	if err != nil {
		return nil
	}
	out := make([]Route, 0, len(routes))
	for _, r := range routes {
		if r.Status != "active" {
			continue
		}
		out = append(out, Route{
			ID:             r.ID,
			Hostname:       r.Hostname,
			Target:         r.Target,
			OwnerInstallID: r.OwnerInstallID,
			OwnerKind:      r.OwnerKind,
			CertFQDN:       r.CertFQDN,
			AllowHTTP:      r.AllowHTTP,
		})
	}
	return out
}

func (s *Server) ingressAllowsCertificate(hostname string) bool {
	host, err := normalizeIngressHostname(stripHostPort(hostname))
	if err != nil || s == nil || s.store == nil {
		return false
	}
	if primary, primaryErr := normalizeIngressHostname(stripHostPort(s.primaryHost)); primaryErr == nil && host == primary {
		return true
	}
	var n int
	_ = s.store.db.QueryRow(`
		SELECT COUNT(*) FROM ingress_routes
		WHERE status='active' AND tls_mode!='off' AND (hostname=? OR cert_fqdn=?)
	`, host, host).Scan(&n)
	return n > 0
}

type ingressScanner interface {
	Scan(dest ...any) error
}

func scanIngressRoute(row ingressScanner) (*IngressRoute, error) {
	var r IngressRoute
	var allowHTTP int
	if err := row.Scan(
		&r.ID, &r.Hostname, &r.Target, &r.ProjectID, &r.OwnerInstallID, &r.OwnerKind,
		&r.CertFQDN, &allowHTTP, &r.TLSMode, &r.Status, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	r.AllowHTTP = allowHTTP != 0
	return &r, nil
}

func ingressBoolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func stripHostPort(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

func (s *Server) handleIngressRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		if projectID == "" {
			if _, ok := s.requirePlatformAdmin(w, r); !ok {
				return
			}
		} else if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
		routes, err := s.ListIngressRoutes(projectID, 0)
		if err != nil {
			http.Error(w, "list ingress routes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"routes": routes})
	case http.MethodPost:
		var req IngressExposeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireScopedProjectAccess(w, r, strings.TrimSpace(req.ProjectID), ProjectEditor) {
			return
		}
		route, err := s.ExposeIngressRoute(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"route": route})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIngressRoute(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimPrefix(r.URL.Path, "/ingress/routes/")
	host = strings.Trim(host, "/")
	if host == "" {
		http.Error(w, "hostname required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		route, err := s.GetIngressRoute(host)
		if err != nil {
			http.Error(w, "ingress route not found", http.StatusNotFound)
			return
		}
		if !s.requireScopedProjectAccess(w, r, route.ProjectID, ProjectViewer) {
			return
		}
		writeJSON(w, map[string]any{"route": route})
	case http.MethodDelete:
		route, err := s.GetIngressRoute(host)
		if err != nil {
			http.Error(w, "ingress route not found", http.StatusNotFound)
			return
		}
		if !s.requireScopedProjectAccess(w, r, route.ProjectID, ProjectEditor) {
			return
		}
		if err := s.DeleteIngressRoute(host, 0); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIngressCerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
	} else if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	routes, err := s.ListIngressRoutes(projectID, 0)
	if err != nil {
		http.Error(w, "list ingress routes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{}
	certs := make([]IngressCertCacheInfo, 0, len(routes))
	for _, route := range routes {
		fqdn := strings.TrimSpace(route.CertFQDN)
		if fqdn == "" {
			fqdn = route.Hostname
		}
		key := fqdn + "|" + route.Hostname
		if seen[key] {
			continue
		}
		seen[key] = true
		info := IngressCertCacheInfo{
			FQDN:      fqdn,
			RouteHost: route.Hostname,
			TLSMode:   route.TLSMode,
		}
		if route.TLSMode == "off" {
			info.Status = "disabled"
			certs = append(certs, info)
			continue
		}
		if s.ingressCerts == nil {
			info.Status = "manager_unavailable"
			certs = append(certs, info)
			continue
		}
		got, err := s.ingressCerts.CachedCertificateInfo(fqdn)
		if err != nil {
			info.Status = "error"
			info.Error = err.Error()
			certs = append(certs, info)
			continue
		}
		got.RouteHost = route.Hostname
		got.TLSMode = route.TLSMode
		certs = append(certs, got)
	}
	writeJSON(w, map[string]any{"certs": certs})
}
