package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
)

type legacyCORSEnvironmentState struct {
	Configured      bool     `json:"configured"`
	Mode            string   `json:"mode"`
	Origins         []string `json:"origins"`
	ReadOnly        bool     `json:"read_only"`
	RestartRequired bool     `json:"restart_required_to_change"`
}

// handleAdminCORSOrigins manages exact, server-wide origins that take effect
// immediately. Unlike app callback registrations, these origins authorize
// preflight across the entire /api surface. Every operation is platform-admin
// only and still relies on the ordinary authentication/authorization of the
// subsequent non-OPTIONS request.
//
//	GET    /admin/cors-origins
//	GET    /admin/cors-origins/:key
//	PUT    /admin/cors-origins/:key  {"origins":[...]}
//	DELETE /admin/cors-origins/:key
func (s *Server) handleAdminCORSOrigins(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.requirePlatformAdmin(w, r)
	if !ok {
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/admin/cors-origins")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		registrations, err := s.listPlatformCORSOriginRegistrations("")
		if err != nil {
			http.Error(w, "list CORS origins", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"registrations":      registrations,
			"legacy_environment": currentLegacyCORSEnvironmentState(),
		})
		return
	}
	if strings.Contains(rest, "/") {
		http.Error(w, "invalid CORS origin registration path", http.StatusNotFound)
		return
	}
	key, err := validateAppCORSRegistrationKey(rest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		registrations, err := s.listPlatformCORSOriginRegistrations(key)
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
		if err := s.replacePlatformCORSOrigins(adminID, key, origins); err != nil {
			http.Error(w, "save CORS origins", http.StatusInternalServerError)
			return
		}
		writeJSON(w, appCORSOriginRegistration{Key: key, Origins: origins})
	case http.MethodDelete:
		if _, err := s.store.db.Exec(`DELETE FROM platform_cors_origins WHERE registration_key=?`, key); err != nil {
			http.Error(w, "delete CORS origins", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PUT, or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) replacePlatformCORSOrigins(adminID int64, key string, origins []string) error {
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM platform_cors_origins WHERE registration_key=?`, key); err != nil {
		return err
	}
	for _, origin := range origins {
		if _, err := tx.Exec(`
			INSERT INTO platform_cors_origins(registration_key, origin, created_by)
			VALUES(?,?,?)`, key, origin, adminID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) listPlatformCORSOriginRegistrations(key string) ([]appCORSOriginRegistration, error) {
	query := `SELECT registration_key, origin FROM platform_cors_origins`
	args := []any{}
	if key != "" {
		query += ` WHERE registration_key=?`
		args = append(args, key)
	}
	query += ` ORDER BY registration_key, origin`
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string][]string{}
	order := []string{}
	for rows.Next() {
		var registrationKey, origin string
		if err := rows.Scan(&registrationKey, &origin); err != nil {
			return nil, err
		}
		if _, exists := byKey[registrationKey]; !exists {
			order = append(order, registrationKey)
		}
		byKey[registrationKey] = append(byKey[registrationKey], origin)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]appCORSOriginRegistration, 0, len(order))
	for _, registrationKey := range order {
		out = append(out, appCORSOriginRegistration{Key: registrationKey, Origins: byKey[registrationKey]})
	}
	return out, nil
}

func currentLegacyCORSEnvironmentState() legacyCORSEnvironmentState {
	cfg := newCORSConfig(os.Getenv("CORS_ORIGIN"))
	state := legacyCORSEnvironmentState{
		Mode:            "disabled",
		Origins:         []string{},
		ReadOnly:        true,
		RestartRequired: true,
	}
	if cfg == nil {
		return state
	}
	state.Configured = true
	state.Mode = cfg.mode
	if cfg.mode == "wildcard" {
		state.Origins = []string{"*"}
		return state
	}
	for origin := range cfg.origins {
		state.Origins = append(state.Origins, origin)
	}
	sort.Strings(state.Origins)
	return state
}
