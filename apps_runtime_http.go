package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func runtimeDecode(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, runtimeResponseLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid runtime request")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("runtime request must contain one JSON value")
	}
	return nil
}
func (s *Server) registerAppDeploymentRoutes(mux *http.ServeMux) {
	s.registerRuntimeConsoleRoutes(mux)
	mux.HandleFunc("/apps/execution-backends/discover", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		found, err := s.appRuntimeManager().discoverBackends(ctx)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		writeJSON(w, map[string]any{"backends": found})
	}))
	mux.HandleFunc("/apps/execution-backends", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		m := s.appRuntimeManager()
		switch r.Method {
		case http.MethodPost:
			var reg runtimeBackendRegistration
			if runtimeDecode(r, &reg) != nil {
				http.Error(w, "invalid backend registration", 400)
				return
			}
			if err := m.registerBackend(reg); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			stored, _ := m.registration(runtimeBackendRef{reg.InstallID, reg.ID})
			writeJSON(w, stored)
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			_, err := m.discoverBackends(ctx)
			cancel()
			if err != nil {
				http.Error(w, "backend discovery unavailable", 503)
				return
			}
			rows, err := s.store.db.Query(`SELECT record_json FROM app_runtime_backends ORDER BY install_id,backend_id`)
			if err != nil {
				http.Error(w, "backend catalog unavailable", 503)
				return
			}
			defer rows.Close()
			out := []runtimeBackendRegistration{}
			for rows.Next() {
				var raw string
				var reg runtimeBackendRegistration
				if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &reg) == nil {
					out = append(out, reg)
				}
			}
			rows.Close()
			for i := range out {
				out[i].DisplayName = m.backendName(runtimeBackendRef{out[i].InstallID, out[i].ID})
			}
			writeJSON(w, map[string]any{"contract": runtimeBackendContract, "backends": out, "local": (&localRuntimeBackend{}).Describe()})
		case http.MethodDelete:
			id, _ := strconv.ParseInt(r.URL.Query().Get("app_install_id"), 10, 64)
			if s.runtimeProviderInUse(id) {
				http.Error(w, "backend owns deployment resources", 409)
				return
			}
			s.store.db.Exec(`DELETE FROM app_runtime_backends WHERE install_id=? AND backend_id=?`, id, r.URL.Query().Get("backend_id"))
			writeJSON(w, map[string]bool{"deleted": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/apps/execution-pools", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		m := s.appRuntimeManager()
		if r.Method == http.MethodPost {
			var p runtimePool
			if runtimeDecode(r, &p) != nil || !runtimeID(p.ID) || len(p.Targets) == 0 || len(p.Targets) > 128 {
				http.Error(w, "invalid execution pool", 400)
				return
			}
			if _, err := m.registration(p.Backend); err != nil {
				http.Error(w, "backend not registered", 400)
				return
			}
			for _, target := range p.Targets {
				var project string
				err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, target.InstallID).Scan(&project)
				if err != nil || project != "" && project != p.ProjectID || target.Type == "" || target.ID == "" {
					http.Error(w, "invalid pool target owner", 400)
					return
				}
			}
			_, err := s.store.db.Exec(`INSERT INTO app_runtime_pools(id,record_json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET record_json=excluded.record_json`, p.ID, runtimeJSON(p))
			if err != nil {
				http.Error(w, "pool persistence failed", 500)
				return
			}
			writeJSON(w, p)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		rows, err := s.store.db.Query(`SELECT record_json FROM app_runtime_pools`)
		if err != nil {
			http.Error(w, "pool catalog unavailable", 503)
			return
		}
		defer rows.Close()
		out := []runtimePool{}
		for rows.Next() {
			var raw string
			var p runtimePool
			if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &p) == nil {
				out = append(out, p)
			}
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("/apps/callback/runtime/browser-authorize", s.authMiddleware(s.handleRuntimeBrowserAuthorize))
	mux.HandleFunc("/apps/callback/runtime/browser-origin", s.authMiddleware(s.handleRuntimeBrowserOrigin))
	mux.HandleFunc("/apps/callback/runtime/ownership", s.authMiddleware(s.handleRuntimeOwnership))
}
func (s *Server) handleAppDeployment(w http.ResponseWriter, r *http.Request, id int64) {
	m := s.appRuntimeManager()
	suffix := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/apps/installs/"), "/", 2)
	if len(suffix) != 2 {
		http.Error(w, "not found", 404)
		return
	}
	path := suffix[1]
	if path == "deployment/resources/runtime" {
		s.handleRuntimeResourceExecution(w, r, id)
		return
	}
	if path == "deployment/resources" {
		s.handleRuntimeResources(w, r, id)
		return
	}
	if path == "deployment/browser-origins" {
		s.handleRuntimeBrowserOrigins(w, r, id)
		return
	}
	if path == "deployment/browser-credentials/revoke" {
		s.handleRuntimeBrowserRevoke(w, r, id)
		return
	}
	if path == "deployment/browser-credentials" {
		s.handleRuntimeBrowserCredentials(w, r, id)
		return
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectViewer); !ok {
		return
	}
	if strings.HasPrefix(path, "deployment/operations/") && strings.HasSuffix(path, "/retry") && r.Method == http.MethodPost {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "deployment/operations/"), "/retry")
		if err := m.retryOperation(ref, id); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		op, _ := m.operation(ref)
		writeJSONStatus(w, 202, op)
		return
	}
	if strings.HasPrefix(path, "deployment/operations/") && strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost {
		if _, ok := s.requirePlatformAdmin(w, r); !ok {
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "deployment/operations/"), "/cancel")
		if err := m.cancelOperation(ref, id); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		writeJSON(w, map[string]bool{"cancel_requested": true})
		return
	}
	if r.Method == http.MethodGet {
		if strings.HasPrefix(path, "deployment/operations/") {
			op, err := m.operation(strings.TrimPrefix(path, "deployment/operations/"))
			if err != nil || op.InstallID != id {
				http.Error(w, "operation not found", 404)
				return
			}
			writeJSON(w, op)
			return
		}
		if path == "deployment/logs" {
			s.handleRuntimeLogs(w, r, id)
			return
		}
		if path == "placement" || path == "deployment" || path == "deployment/endpoints" {
			ds, err := m.deployments(id)
			if err != nil {
				http.Error(w, "deployment state unavailable", 503)
				return
			}
			active, _ := m.active(id)
			if path == "deployment/endpoints" {
				var status string
				s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status)
				if active.ID == "" || active.Status != "active" || status != "running" {
					http.Error(w, "no active endpoint", 503)
					return
				}
				writeJSON(w, map[string]any{"install_id": id, "deployment_generation": active.Generation, "proxy_base": fmt.Sprintf("%s/api/apps/%s/_install/%d", s.publicBaseURL(), active.Manifest.Name, id), "browser_api_base": active.Execution.BrowserEndpoint, "endpoints": active.Execution.Endpoints})
				return
			}
			var pending, raw string
			s.store.db.QueryRow(`SELECT runtime_pending_deployment,runtime_placement_json FROM app_installs WHERE id=?`, id).Scan(&pending, &raw)
			var p runtimePlacement
			json.Unmarshal([]byte(raw), &p)
			writeJSON(w, map[string]any{"placement": p, "active_deployment_id": active.ID, "pending_deployment_id": pending, "deployments": ds, "summary": m.deploymentSummary(id), "operations": m.consoleOperations(id), "can_scale": active.ID != "" && m.replicaEligible(active) == nil})
			return
		}
		http.Error(w, "not found", 404)
		return
	}
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if path == "deployment/scale" {
		var body struct {
			Replicas int `json:"replicas"`
		}
		if runtimeDecode(r, &body) != nil {
			http.Error(w, "invalid replica request", 400)
			return
		}
		if err := m.scale(id, body.Replicas); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		writeJSONStatus(w, 202, map[string]bool{"queued": true})
		return
	}
	if path == "deployment/autoscaling" {
		var p runtimeScalingPolicy
		if runtimeDecode(r, &p) != nil || p.Min < 1 || p.Max < p.Min || p.Max > 32 || p.TargetLoad <= 0 || p.CooldownSeconds < 30 {
			http.Error(w, "invalid scaling policy", 400)
			return
		}
		d, err := m.active(id)
		if err != nil || m.replicaEligible(d) != nil {
			http.Error(w, "installation is not replica eligible", 409)
			return
		}
		p.InstallID = id
		p.LastScaled = 0
		_, err = s.store.db.Exec(`INSERT INTO app_runtime_scaling(install_id,record_json) VALUES(?,?) ON CONFLICT(install_id) DO UPDATE SET record_json=excluded.record_json`, id, runtimeJSON(p))
		if err != nil {
			http.Error(w, "scaling policy persistence failed", 500)
			return
		}
		writeJSON(w, p)
		return
	}
	kind := strings.TrimPrefix(path, "deployment/")
	if kind != "deploy" && kind != "prepare" && kind != "activate" && kind != "stop" && kind != "delete" && kind != "restart" && kind != "discard" {
		http.Error(w, "not found", 404)
		return
	}
	var body struct {
		Placement runtimePlacement `json:"placement"`
	}
	if kind == "prepare" || kind == "deploy" {
		if runtimeDecode(r, &body) != nil {
			http.Error(w, "invalid placement", 400)
			return
		}
	} else {
		body.Placement = runtimePlacement{Type: "local"}
		if kind == "restart" {
			if current, err := m.active(id); err == nil {
				body.Placement = current.Placement
			} else {
				var raw string
				if err = m.s.store.db.QueryRow(`SELECT runtime_placement_json FROM app_installs WHERE id=?`, id).Scan(&raw); err != nil || raw == "" || json.Unmarshal([]byte(raw), &body.Placement) != nil {
					http.Error(w, "installation has no persisted execution placement", 409)
					return
				}
			}
		}
	}
	op, err := m.begin(id, kind, r.Header.Get("Idempotency-Key"), body.Placement, nil)
	if err != nil {
		status := 409
		if err == sql.ErrNoRows {
			status = 404
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, op)
}
func (s *Server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request, id int64) {
	m := s.appRuntimeManager()
	ref := r.URL.Query().Get("deployment_id")
	if ref == "" {
		d, err := m.active(id)
		if err != nil {
			http.Error(w, "deployment not found", 404)
			return
		}
		ref = d.ID
	}
	d, err := m.loadDeployment(ref)
	if err != nil || d.InstallID != id {
		http.Error(w, "deployment not found", 404)
		return
	}
	if d.Placement.local() {
		http.Error(w, "local logs use the existing install log endpoint", 409)
		return
	}
	b, err := m.backend(d)
	if err != nil {
		http.Error(w, "backend unavailable", 503)
		return
	}
	remote := b.(*remoteRuntimeBackend)
	req, _ := m.request(d, runtimeOperation{ID: "logs"}, "logs")
	req.Options = map[string]any{"cursor": r.URL.Query().Get("cursor"), "limit": 100}
	var out struct {
		Lines  []string `json:"lines"`
		Cursor string   `json:"cursor"`
	}
	if err = remote.request(r.Context(), http.MethodPost, "logs", &req, &out); err != nil {
		http.Error(w, "backend logs unavailable", 503)
		return
	}
	if len(out.Lines) > 100 {
		out.Lines = out.Lines[:100]
	}
	secrets := []string{remote.token}
	if token, e := m.token(d); e == nil {
		secrets = append(secrets, token)
	}
	for i, line := range out.Lines {
		for _, secret := range secrets {
			if len(secret) > 8 {
				line = strings.ReplaceAll(line, secret, "[redacted]")
			}
		}
		out.Lines[i] = line
	}
	writeJSON(w, out)
}
