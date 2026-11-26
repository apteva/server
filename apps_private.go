package main

// Private installations are ordinary installs of an app, made through the
// usual install flow, that serve only the apps an administrator allows. They
// keep their own data, config and connections like any second installation,
// and they never appear where people or agents find apps:
//
//   - never the default, so no lookup by name reaches them (the database
//     refuses it, see migrateAppInstallations);
//   - absent from /api/apps, the registry's lists, agent tool bridges,
//     agent attachments and default apps;
//   - the app proxy serves only their public routes (provider webhooks);
//   - their events reach only their allowed apps;
//   - the Helper and agents cannot manage them by id.
//
// The allowed apps call them by install id (sdk.WithAppInstallation, or
// install_id in a Functions call). A private installation may in turn call
// only private installations that allow it, so data never flows between it
// and the project's own apps.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) isPrivateInstall(id int64) bool {
	private, _, err := s.privateInstallCallers(id)
	return err == nil && private
}

// privateInstallCallers reports whether the install is private and which
// installs may call it.
func (s *Server) privateInstallCallers(id int64) (bool, []int64, error) {
	var private bool
	var raw string
	err := s.store.dbQueryRow(`SELECT private,COALESCE(private_callers_json,'[]') FROM app_installs WHERE id=?`, id).Scan(&private, &raw)
	if err != nil {
		return false, nil, err
	}
	callers := []int64{}
	if private && json.Unmarshal([]byte(raw), &callers) != nil {
		return true, nil, errors.New("private installation callers unreadable")
	}
	return private, callers, nil
}

// privateAppCallError applies the call rule between two installs. A call
// that touches no private installation is unaffected.
func (s *Server) privateAppCallError(callerID, targetID int64) error {
	targetPrivate, callers, err := s.privateInstallCallers(targetID)
	if err != nil {
		return errors.New("target installation unavailable")
	}
	if targetPrivate {
		for _, id := range callers {
			if id == callerID {
				return nil
			}
		}
		return fmt.Errorf("private installation %d does not allow calls from installation %d", targetID, callerID)
	}
	if s.isPrivateInstall(callerID) {
		return errors.New("a private installation may call only private installations that allow it")
	}
	return nil
}

// requestFromAgentGateway is true for calls the Helper and agent tools make
// through the platform gateway (instance secret from loopback).
func requestFromAgentGateway(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("X-Apteva-MCP-User-ID")) != "" &&
		(r.Header.Get("X-Agent-Secret") != "" || r.Header.Get("X-Instance-Secret") != "")
}

// hidePrivateInstallFromGateway answers 404 when the Helper or an agent
// addresses a private installation by id.
func (s *Server) hidePrivateInstallFromGateway(w http.ResponseWriter, r *http.Request, id int64) bool {
	if requestFromAgentGateway(r) && s.isPrivateInstall(id) {
		http.Error(w, "app install not found", http.StatusNotFound)
		return true
	}
	return false
}

type privateInstallCaller struct {
	InstallID int64  `json:"install_id"`
	App       string `json:"app"`
	Name      string `json:"installation_name"`
	ProjectID string `json:"project_id"`
}

type privateInstallView struct {
	InstallID     int64                  `json:"install_id"`
	App           string                 `json:"app"`
	DisplayName   string                 `json:"display_name"`
	Icon          string                 `json:"icon,omitempty"`
	ProjectID     string                 `json:"project_id"`
	Key           string                 `json:"installation_key"`
	Name          string                 `json:"installation_name"`
	Status        string                 `json:"status"`
	StatusMessage string                 `json:"status_message,omitempty"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
	Version       string                 `json:"version"`
	Callers       []privateInstallCaller `json:"callers"`
}

// GET /api/apps/private
// PUT /api/apps/private/<id>/callers {"caller_install_ids":[...]}
func (s *Server) handlePrivateInstalls(w http.ResponseWriter, r *http.Request) {
	if requestFromAgentGateway(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apps/private"), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		views, err := s.listPrivateInstalls()
		if err != nil {
			http.Error(w, "private installations unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, views)
		return
	}
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 || len(parts) != 2 || parts[1] != "callers" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Callers []int64 `json:"caller_install_ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	view, status, err := s.setPrivateInstallCallers(id, body.Callers)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, view)
}

func (s *Server) setPrivateInstallCallers(id int64, requested []int64) (*privateInstallView, int, error) {
	private, _, err := s.privateInstallCallers(id)
	if err != nil {
		return nil, http.StatusNotFound, errors.New("installation not found")
	}
	if !private {
		return nil, http.StatusConflict, errors.New("installation is not private")
	}
	callers := []int64{}
	seen := map[int64]bool{}
	for _, caller := range requested {
		if caller <= 0 || caller == id {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid caller installation %d", caller)
		}
		if seen[caller] {
			continue
		}
		seen[caller] = true
		var exists int
		if s.store.dbQueryRow(`SELECT 1 FROM app_installs WHERE id=? AND owner_install_id=0`, caller).Scan(&exists) != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("caller installation %d not found", caller)
		}
		callers = append(callers, caller)
	}
	raw, _ := json.Marshal(callers)
	if _, err = s.store.dbExec(`UPDATE app_installs SET private_callers_json=? WHERE id=? AND private=1`, string(raw), id); err != nil {
		return nil, http.StatusInternalServerError, errors.New("save callers failed")
	}
	views, err := s.listPrivateInstalls(id)
	if err != nil || len(views) != 1 {
		return nil, http.StatusInternalServerError, errors.New("read installation failed")
	}
	return &views[0], 0, nil
}

func (s *Server) listPrivateInstalls(only ...int64) ([]privateInstallView, error) {
	q := `SELECT i.id,a.name,COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),COALESCE(i.project_id,''),
	             i.installation_key,i.installation_name,i.status,COALESCE(i.status_message,''),COALESCE(i.error_message,''),
	             COALESCE(i.version,''),COALESCE(i.private_callers_json,'[]')
	      FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.private=1`
	args := []any{}
	if len(only) == 1 {
		q += ` AND i.id=?`
		args = append(args, only[0])
	}
	rows, err := s.store.dbQuery(q+` ORDER BY a.name,i.id`, args...)
	if err != nil {
		return nil, err
	}
	views := []privateInstallView{}
	callerIDs := [][]int64{}
	for rows.Next() {
		var v privateInstallView
		var manifestJSON, callersJSON string
		if err = rows.Scan(&v.InstallID, &v.App, &manifestJSON, &v.ProjectID, &v.Key, &v.Name, &v.Status, &v.StatusMessage,
			&v.ErrorMessage, &v.Version, &callersJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var manifest struct {
			DisplayName string `json:"display_name"`
			Icon        string `json:"icon"`
		}
		_ = json.Unmarshal([]byte(manifestJSON), &manifest)
		v.DisplayName = manifest.DisplayName
		if v.DisplayName == "" {
			v.DisplayName = v.App
		}
		v.Icon = manifest.Icon
		ids := []int64{}
		_ = json.Unmarshal([]byte(callersJSON), &ids)
		views = append(views, v)
		callerIDs = append(callerIDs, ids)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range views {
		views[i].Callers = []privateInstallCaller{}
		for _, id := range callerIDs[i] {
			c := privateInstallCaller{InstallID: id}
			if s.store.dbQueryRow(`SELECT a.name,i.installation_name,COALESCE(i.project_id,'') FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).
				Scan(&c.App, &c.Name, &c.ProjectID) != nil {
				c.App = "removed"
			}
			views[i].Callers = append(views[i].Callers, c)
		}
	}
	return views, nil
}
