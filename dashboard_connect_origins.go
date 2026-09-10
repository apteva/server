package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// Ownership is install-scoped; the CSP effect is dashboard-wide. Require both
// the approved permission snapshot and a currently administrator-owned install.
// Pending installs may reconcile during startup before they become running.
func (s *Server) dashboardConnectAllowed(installID int64) bool {
	var owner int64
	var status string
	if err := s.store.db.QueryRow(`SELECT installed_by, status FROM app_installs WHERE id=?`, installID).Scan(&owner, &status); err != nil {
		return false
	}
	return (status == "pending" || status == "running") && owner > 0 && s.isAdmin(owner) && installHasPermission(s, installID, sdk.PermDashboardConnect)
}

func normalizeDashboardConnectOrigins(raw []string) ([]string, error) {
	if len(raw) > 100 {
		return nil, fmt.Errorf("at most 100 origins are allowed per registration")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range raw {
		item = strings.TrimSpace(item)
		u, err := url.Parse(item)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(item, "?#") {
			return nil, fmt.Errorf("expected an exact HTTPS origin, got %q", item)
		}
		host := strings.ToLower(u.Hostname())
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
		} else {
			if strings.ContainsAny(u.Host, "[]") {
				return nil, fmt.Errorf("invalid origin host")
			}
			if len(host) > 253 {
				return nil, fmt.Errorf("invalid origin host")
			}
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
					return nil, fmt.Errorf("invalid origin host")
				}
				for _, c := range label {
					if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
						return nil, fmt.Errorf("invalid origin host")
					}
				}
			}
		}
		port := u.Port()
		if strings.HasSuffix(u.Host, ":") {
			return nil, fmt.Errorf("invalid origin port")
		}
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("invalid origin port")
			}
			if n != 443 {
				host += ":" + strconv.Itoa(n)
			}
		}
		origin := "https://" + host
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	sort.Strings(out)
	return out, nil
}

// GET collection/key, PUT replace, DELETE clear. No CSP directives or CORS
// fields are accepted. Empty PUT and DELETE have the same durable effect.
func (s *Server) handleCallbackDashboardConnectOrigins(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !s.dashboardConnectAllowed(installID) {
		http.Error(w, "requires an active administrator-owned install with approved platform.dashboard.connect permission", http.StatusForbidden)
		return
	}
	key := ""
	if len(parts) > 1 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && parts[0] != "" {
		key, err = validateAppCORSRegistrationKey(parts[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.listDashboardConnectOrigins(installID, key)
		if err != nil {
			http.Error(w, "could not list dashboard connection destinations", 500)
			return
		}
		if key == "" {
			writeJSON(w, map[string]any{"registrations": rows})
			return
		}
		if len(rows) == 0 {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, rows[0])
	case http.MethodPut, http.MethodDelete:
		if key == "" {
			http.Error(w, "registration key required", 400)
			return
		}
		origins := []string{}
		if r.Method == http.MethodPut {
			var body struct {
				Origins []string `json:"origins"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				http.Error(w, "invalid origins payload", 400)
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				http.Error(w, "expected one origins payload", http.StatusBadRequest)
				return
			}
			origins, err = normalizeDashboardConnectOrigins(body.Origins)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		tx, err := s.store.db.Begin()
		if err != nil {
			http.Error(w, "could not save destinations", 500)
			return
		}
		defer tx.Rollback()
		_, err = tx.Exec(`DELETE FROM app_dashboard_connect_origins WHERE install_id=? AND registration_key=?`, installID, key)
		for _, origin := range origins {
			if err != nil {
				break
			}
			_, err = tx.Exec(`INSERT INTO app_dashboard_connect_origins(install_id,registration_key,origin) VALUES(?,?,?)`, installID, key, origin)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			http.Error(w, "could not save destinations", 500)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, sdk.DashboardConnectOriginRegistration{Key: key, Origins: origins})
	default:
		http.Error(w, "GET, PUT, or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listDashboardConnectOrigins(installID int64, key string) ([]sdk.DashboardConnectOriginRegistration, error) {
	rows, err := s.store.db.Query(`SELECT registration_key,origin FROM app_dashboard_connect_origins WHERE install_id=? AND (?='' OR registration_key=?) ORDER BY registration_key,origin`, installID, key, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sdk.DashboardConnectOriginRegistration{}
	for rows.Next() {
		var k, origin string
		if err := rows.Scan(&k, &origin); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Key != k {
			out = append(out, sdk.DashboardConnectOriginRegistration{Key: k, Origins: []string{}})
		}
		out[len(out)-1].Origins = append(out[len(out)-1].Origins, origin)
	}
	return out, rows.Err()
}

func (s *Server) dashboardConnectOrigins() ([]string, error) {
	// Read rows before checking permissions: the store can have a single DB
	// connection, so nested queries while rows are open could deadlock.
	rows, err := s.store.db.Query(`SELECT DISTINCT install_id,origin FROM app_dashboard_connect_origins`)
	if err != nil {
		return nil, err
	}
	byInstall := map[int64][]string{}
	for rows.Next() {
		var id int64
		var origin string
		if err := rows.Scan(&id, &origin); err != nil {
			rows.Close()
			return nil, err
		}
		byInstall[id] = append(byInstall[id], origin)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for id, origins := range byInstall {
		if !s.dashboardConnectAllowed(id) {
			continue
		}
		for _, origin := range origins {
			if !seen[origin] {
				seen[origin] = true
				out = append(out, origin)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
