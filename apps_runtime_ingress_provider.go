package main

// Server-owned, backend-independent ingress orchestration. Providers receive
// normalized routes and an explicit upstream, never an app manifest or credentials.
import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const runtimeIngressContract = "apteva.ingress-provider/v1"

type runtimeIngressProviderRef struct {
	InstallID int64              `json:"app_install_id"`
	ID        string             `json:"id"`
	Target    runtimeResourceRef `json:"target"`
	Options   map[string]any     `json:"options"`
}
type runtimeIngressIntent struct {
	ExpectedDeploymentID string                    `json:"expected_deployment_id,omitempty"`
	ExpectedGeneration   int64                     `json:"expected_generation,omitempty"`
	Contract             string                    `json:"contract"`
	ControllerID         string                    `json:"controller_id"`
	OperationID          string                    `json:"operation_id"`
	InstallID            int64                     `json:"install_id"`
	DeploymentID         string                    `json:"deployment_id"`
	Generation           int64                     `json:"generation"`
	ProjectID            string                    `json:"project_id,omitempty"`
	Target               runtimeResourceRef        `json:"target"`
	Hostname             string                    `json:"hostname"`
	HostKey              string                    `json:"host_key,omitempty"`
	Upstream             string                    `json:"upstream"`
	Routes               []sdk.RuntimeIngressRoute `json:"routes"`
	Options              map[string]any            `json:"options"`
	PlanSHA256           string                    `json:"plan_sha256,omitempty"`
	MaintenanceWindow    bool                      `json:"maintenance_window,omitempty"`
}
type runtimeIngressPlan struct {
	PlanSHA256         string         `json:"plan_sha256"`
	ProviderPlanSHA256 string         `json:"provider_plan_sha256"`
	HostKey            string         `json:"host_key"`
	DeploymentID       string         `json:"deployment_id"`
	Generation         int64          `json:"generation"`
	Evidence           map[string]any `json:"evidence"`
}
type runtimeIngressAdoptionRequest struct {
	Hostname           string                    `json:"hostname"`
	Provider           runtimeIngressProviderRef `json:"provider"`
	PlanSHA256         string                    `json:"plan_sha256,omitempty"`
	ProviderPlanSHA256 string                    `json:"provider_plan_sha256,omitempty"`
	HostKey            string                    `json:"host_key,omitempty"`
	MaintenanceWindow  bool                      `json:"maintenance_window,omitempty"`
	IdempotencyKey     string                    `json:"idempotency_key,omitempty"`
}
type runtimeIngressOperation struct {
	Request          runtimeIngressAdoptionRequest `json:"request"`
	Intent           runtimeIngressIntent          `json:"intent"`
	MaintenanceUntil int64                         `json:"maintenance_until"`
	Receipt          map[string]any                `json:"receipt,omitempty"`
}
type runtimeIngressBinding struct {
	OriginalDeploymentID    string             `json:"original_deployment_id"`
	PreviousPlacement       runtimePlacement   `json:"previous_placement"`
	PreviousBrowserEndpoint string             `json:"previous_browser_endpoint"`
	PreviousEndpoints       map[string]string  `json:"previous_endpoints"`
	Spec                    runtimeIngressSpec `json:"spec"`
	HostKey                 string             `json:"host_key"`
	DeploymentID            string             `json:"deployment_id"`
	Generation              int64              `json:"generation"`
	Phase                   string             `json:"phase"`
	PlanSHA256              string             `json:"plan_sha256"`
	Receipt                 map[string]any     `json:"receipt,omitempty"`
}

func (m *appRuntimeManager) ingressProvider(ref runtimeIngressProviderRef, project string) (*remoteRuntimeBackend, error) {
	if ref.InstallID <= 0 || !runtimeID(ref.ID) || ref.Target.InstallID != ref.InstallID || ref.Target.Type == "" || ref.Target.ID == "" {
		return nil, fmt.Errorf("explicit namespaced ingress provider/target required")
	}
	endpoint := "/_apteva/ingress/providers/" + ref.ID
	var base, status, owner string
	if e := m.s.store.db.QueryRow(`SELECT sidecar_url_override,status,COALESCE(project_id,'') FROM app_installs WHERE id=?`, ref.InstallID).Scan(&base, &status, &owner); e != nil || status != "running" || owner != "" && owner != project {
		return nil, fmt.Errorf("ingress provider unavailable or outside installation project")
	}
	if e := runtimeSecureURL(base); e != nil {
		return nil, e
	}
	manifest, e := installManifest(m.s, ref.InstallID)
	if e != nil {
		return nil, e
	}
	found := false
	for _, route := range manifest.Provides.HTTPRoutes {
		if strings.TrimSuffix(route.Prefix, "/") == endpoint {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("provider must declare its protected ingress endpoint")
	}
	token, e := m.s.runtimeControlToken(ref.InstallID)
	if e != nil {
		return nil, e
	}
	b := &remoteRuntimeBackend{m, runtimeBackendRegistration{InstallID: ref.InstallID, ID: ref.ID, Endpoint: endpoint}, base, token}
	var caps struct {
		Contract             string `json:"contract"`
		ID                   string `json:"id"`
		Adoption             bool   `json:"adoption"`
		Rollback             bool   `json:"rollback"`
		RequiresHostIdentity bool   `json:"requires_host_identity"`
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	if e = b.request(ctx, "GET", "describe", nil, &caps); e != nil {
		return nil, e
	}
	if caps.Contract != runtimeIngressContract || caps.ID != ref.ID {
		return nil, fmt.Errorf("unsupported ingress provider contract")
	}
	b.registration.Caps.IngressIdentityRequired = caps.RequiresHostIdentity
	return b, nil
}
func (m *appRuntimeManager) ingressIntent(d runtimeDeployment, ref runtimeIngressProviderRef, host, operation string) (runtimeIngressIntent, error) {
	var project string
	if e := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, d.InstallID).Scan(&project); e != nil {
		return runtimeIngressIntent{}, e
	}
	if e := validateRuntimeIngressSpec(&runtimeIngressSpec{Hostname: host}); e != nil {
		return runtimeIngressIntent{}, e
	}
	if d.Manifest.Runtime.Ingress == nil {
		return runtimeIngressIntent{}, fmt.Errorf("approved release declares no public routes")
	}
	if e := sdk.ValidateRuntimeIngress(d.Manifest.Runtime.Ingress); e != nil {
		return runtimeIngressIntent{}, e
	}
	if d.Execution.IngressUpstream == "" {
		return runtimeIngressIntent{}, fmt.Errorf("execution backend must report ingress_upstream; control tunnels are not public upstreams")
	}
	if e := runtimeSecureURL(d.Execution.IngressUpstream); e != nil {
		return runtimeIngressIntent{}, e
	}
	options := map[string]any{}
	for key, value := range ref.Options {
		options[key] = value
	}
	options["hostname"] = host
	return runtimeIngressIntent{Contract: runtimeIngressContract, ControllerID: HashAPIKey("ingress-controller:" + string(m.s.secret)), OperationID: operation, InstallID: d.InstallID, DeploymentID: d.ID, Generation: d.Generation, ProjectID: project, Target: ref.Target, Hostname: host, HostKey: d.Execution.IngressHostKey, Upstream: d.Execution.IngressUpstream, Routes: d.Manifest.Runtime.Ingress.Routes, Options: options}, nil
}
func (m *appRuntimeManager) ingressActive(id int64, expected string, observe bool) (runtimeDeployment, error) {
	d, e := m.active(id)
	if e != nil || d.ID != expected && expected != "" || d.Status != "active" {
		return d, fmt.Errorf("active deployment changed")
	}
	var pending, status string
	var generation int64
	var allowed int
	if e = m.s.store.db.QueryRow(`SELECT runtime_pending_deployment,status,runtime_generation FROM app_installs WHERE id=? AND runtime_managed=1`, id).Scan(&pending, &status, &generation); e != nil || pending != "" || status != "running" || generation != d.Generation {
		return d, fmt.Errorf("installation is changing or not active")
	}
	if e = m.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_execution_owners WHERE deployment_id=? AND install_id=? AND generation=? AND granted=1 AND admission_confirmed=1`, d.ID, id, d.Generation).Scan(&allowed); e != nil || allowed != 1 {
		return d, fmt.Errorf("deployment does not own admitted execution")
	}
	if observe {
		b, e := m.backend(d)
		if e != nil {
			return d, e
		}
		req, e := m.request(d, runtimeOperation{ID: "ingress-inspect"}, "inspect")
		if e != nil {
			return d, e
		}
		out, e := b.Inspect(req)
		if e != nil {
			return d, e
		}
		out, e = runtimeMergeExecution(d.Execution, out)
		if e != nil {
			return d, e
		}
		health := m.runtimeAppHealthContext(m.ctx, d, out)
		if out.State != "running" || !out.Ready || !health.Healthy || !health.IdentityVerified || !health.Admitting {
			return d, fmt.Errorf("live deployment readiness and authenticated admission required")
		}
		d.Execution = out
	}
	return d, nil
}
func runtimeIngressPlanHash(d runtimeDeployment, r runtimeIngressAdoptionRequest, providerHash, key string) string {
	return runtimeFingerprint(struct {
		Deployment                      string
		Generation                      int64
		Manifest                        sdk.Manifest
		Placement                       runtimePlacement
		Execution                       runtimeExecution
		Provider                        runtimeIngressProviderRef
		Hostname, ProviderHash, HostKey string
	}{d.ID, d.Generation, d.Manifest, d.Placement, runtimeExecution{Handle: d.Execution.Handle, HostKey: d.Execution.HostKey, ReleaseDigest: d.Execution.ReleaseDigest, IngressUpstream: d.Execution.IngressUpstream}, r.Provider, r.Hostname, providerHash, key})
}
func (m *appRuntimeManager) inspectIngressAdoption(id int64, r runtimeIngressAdoptionRequest) (runtimeIngressPlan, error) {
	var out runtimeIngressPlan
	d, e := m.ingressActive(id, "", true)
	if e != nil {
		return out, e
	}
	intent, e := m.ingressIntent(d, r.Provider, r.Hostname, "ingress-inspect")
	if e != nil {
		return out, e
	}
	p, e := m.ingressProvider(r.Provider, intent.ProjectID)
	if e != nil {
		return out, e
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	if p.registration.Caps.IngressIdentityRequired && intent.HostKey == "" {
		return out, fmt.Errorf("backend must report ingress_host_key for this host-bound provider")
	}
	var evidence map[string]any
	if e = ingressProviderRequest(ctx, p, "inspect", &intent, &evidence); e != nil {
		return out, e
	}
	hash, _ := evidence["plan_sha256"].(string)
	key, _ := evidence["host_key"].(string)
	if !runtimeDigest(hash) || key == "" || len(key) > 512 {
		return out, fmt.Errorf("provider returned incomplete plan identity")
	}
	out = runtimeIngressPlan{PlanSHA256: runtimeIngressPlanHash(d, r, hash, key), ProviderPlanSHA256: hash, HostKey: key, DeploymentID: d.ID, Generation: d.Generation, Evidence: evidence}
	return out, nil
}
func (m *appRuntimeManager) ingressBinding(id int64) (runtimeIngressBinding, error) {
	var b runtimeIngressBinding
	var raw string
	e := m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_ingress_bindings WHERE install_id=?`, id).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &b)
	}
	return b, e
}
func (m *appRuntimeManager) beginIngress(id int64, action string, r runtimeIngressAdoptionRequest) (runtimeOperation, error) {
	var empty runtimeOperation
	if action != "adopt" && action != "rollback" || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256 || !runtimeDigest(r.PlanSHA256) || !runtimeDigest(r.ProviderPlanSHA256) || !r.MaintenanceWindow {
		return empty, fmt.Errorf("reviewed plan, fresh maintenance window and idempotency_key required")
	}
	fingerprint := runtimeFingerprint(struct {
		Action  string
		Request runtimeIngressAdoptionRequest
	}{action, r})
	var previousID, previousHash string
	e := m.s.store.db.QueryRow(`SELECT id,fingerprint FROM app_runtime_operations WHERE install_id=? AND idempotency_key=?`, id, r.IdempotencyKey).Scan(&previousID, &previousHash)
	if e == nil {
		if previousHash != fingerprint {
			return empty, fmt.Errorf("ingress retry conflicts with original request")
		}
		return m.operation(previousID)
	}
	if e != sql.ErrNoRows {
		return empty, e
	}
	d, e := m.ingressActive(id, "", true)
	if e != nil {
		return empty, e
	}
	if action == "adopt" && runtimeIngressPlanHash(d, r, r.ProviderPlanSHA256, r.HostKey) != r.PlanSHA256 {
		return empty, fmt.Errorf("reviewed plan no longer matches active deployment")
	}
	if action == "rollback" {
		saved, e := m.ingressBinding(id)
		if e != nil || saved.Spec.Provider == nil || saved.DeploymentID != d.ID || saved.OriginalDeploymentID != d.ID || saved.PlanSHA256 != r.PlanSHA256 || saved.Spec.Hostname != r.Hostname || runtimeJSON(*saved.Spec.Provider) != runtimeJSON(r.Provider) || saved.HostKey != r.HostKey {
			return empty, fmt.Errorf("rollback does not address the owned current ingress")
		}
	}
	op := runtimeOperation{ID: rand.Text(), InstallID: id, Kind: "ingress-" + action, Status: "queued", Phase: "queued", DeploymentID: d.ID, PreviousID: d.ID, IdempotencyKey: r.IdempotencyKey, Fingerprint: fingerprint, CreatedAt: runtimeTimestamp(), UpdatedAt: runtimeTimestamp()}
	intent, e := m.ingressIntent(d, r.Provider, r.Hostname, op.ID)
	if e != nil {
		return empty, e
	}
	intent.HostKey = r.HostKey
	intent.PlanSHA256 = r.ProviderPlanSHA256
	op.Ingress = &runtimeIngressOperation{Request: r, Intent: intent, MaintenanceUntil: time.Now().Add(time.Minute).Unix()}
	tx, e := m.s.store.db.Begin()
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	var active, pending string
	if e = tx.QueryRow(`SELECT runtime_active_deployment,runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&active, &pending); e != nil || active != d.ID || pending != "" {
		return empty, fmt.Errorf("deployment changed before ingress reservation")
	}
	if _, e = tx.Exec(`INSERT INTO app_runtime_ingress(install_id,hostname,record_json) VALUES(?,?,?) ON CONFLICT(install_id) DO NOTHING`, id, r.Hostname, runtimeJSON(runtimeIngressState{Spec: &runtimeIngressSpec{Hostname: r.Hostname, Provider: &r.Provider}})); e != nil {
		return empty, fmt.Errorf("hostname is already owned")
	}
	var hostname string
	if e = tx.QueryRow(`SELECT hostname FROM app_runtime_ingress WHERE install_id=?`, id).Scan(&hostname); e != nil || hostname != r.Hostname {
		return empty, fmt.Errorf("installation already owns another hostname")
	}
	if _, e = tx.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,idempotency_key,fingerprint,record_json) VALUES(?,?,?,'queued',?,?,?)`, op.ID, id, op.Kind, r.IdempotencyKey, fingerprint, runtimeJSON(op)); e != nil {
		return empty, fmt.Errorf("another deployment or ingress operation owns this installation")
	}
	binding := runtimeIngressBinding{OriginalDeploymentID: d.ID, PreviousPlacement: d.Placement, PreviousBrowserEndpoint: d.Execution.BrowserEndpoint, PreviousEndpoints: d.Execution.Endpoints, Spec: runtimeIngressSpec{Hostname: r.Hostname, Provider: &r.Provider}, HostKey: r.HostKey, DeploymentID: d.ID, Generation: d.Generation, Phase: "pending", PlanSHA256: r.PlanSHA256}
	var priorRaw string
	if err := tx.QueryRow(`SELECT record_json FROM app_runtime_ingress_bindings WHERE install_id=?`, id).Scan(&priorRaw); err == nil {
		var prior runtimeIngressBinding
		if json.Unmarshal([]byte(priorRaw), &prior) != nil {
			return empty, fmt.Errorf("invalid retained ingress state")
		}
		binding.OriginalDeploymentID = prior.OriginalDeploymentID
		binding.PreviousPlacement = prior.PreviousPlacement
		binding.PreviousBrowserEndpoint = prior.PreviousBrowserEndpoint
		binding.PreviousEndpoints = prior.PreviousEndpoints
	}
	if _, e = tx.Exec(`INSERT INTO app_runtime_ingress_bindings(install_id,record_json) VALUES(?,?) ON CONFLICT(install_id) DO UPDATE SET record_json=excluded.record_json`, id, runtimeJSON(binding)); e != nil {
		return empty, e
	}
	if e = tx.Commit(); e != nil {
		return empty, e
	}
	m.launch(op.ID)
	return op, nil
}
func providerReceiptMatches(receipt map[string]any, d runtimeDeployment, phase string) bool {
	gen, ok := receipt["generation"].(float64)
	return ok && int64(gen) == d.Generation && receipt["deployment_id"] == d.ID && receipt["phase"] == phase && receipt["source_matches"] == true
}
func (m *appRuntimeManager) runIngressOperation(op *runtimeOperation) error {
	if e := m.phase(op, "inspecting_ingress"); e != nil {
		return e
	}
	if op.Ingress == nil {
		return fmt.Errorf("missing durable ingress intent")
	}
	work := op.Ingress
	d, e := m.ingressActive(op.InstallID, op.DeploymentID, true)
	if e != nil {
		return e
	}
	if e = m.checkOwner(*op); e != nil {
		return e
	}
	provider, e := m.ingressProvider(work.Request.Provider, work.Intent.ProjectID)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
	defer cancel()
	action := strings.TrimPrefix(op.Kind, "ingress-")
	phase := "active"
	if action == "rollback" {
		phase = "released"
	}
	// Reconcile a lost response first. Expired maintenance attestations never
	// authorize a fresh cutover after a controller restart or delayed queue.
	var status map[string]any
	statusErr := ingressProviderRequest(ctx, provider, "status", &work.Intent, &status)
	done := statusErr == nil && providerReceiptMatches(status, d, phase)
	if !done {
		if time.Now().Unix() > work.MaintenanceUntil {
			return fmt.Errorf("maintenance window expired; inspect status and submit a fresh reviewed request")
		}
		if action == "adopt" {
			plan, e := m.inspectIngressAdoption(op.InstallID, work.Request)
			if e != nil {
				return e
			}
			if plan.PlanSHA256 != work.Request.PlanSHA256 || plan.HostKey != work.Intent.HostKey {
				return fmt.Errorf("proxy plan changed; inspect and review again")
			}
		}
		if e = m.checkOwner(*op); e != nil {
			return e
		}
		work.Intent.MaintenanceWindow = true
		var receipt map[string]any
		if e = ingressProviderRequest(ctx, provider, action, &work.Intent, &receipt); e != nil {
			return e
		}
		if e = ingressProviderRequest(ctx, provider, "status", &work.Intent, &status); e != nil || !providerReceiptMatches(status, d, phase) {
			return fmt.Errorf("proxy operation is unconfirmed; ownership retained for inspection")
		}
	}
	if e = m.phase(op, "verifying_ingress"); e != nil {
		return e
	}
	if phase == "active" {
		if e = m.verifyIngressHTTPS(ctx, work.Request.Hostname, d.ID); e != nil {
			if _, ownerErr := m.ingressActive(d.InstallID, d.ID, false); ownerErr == nil && time.Now().Unix() <= work.MaintenanceUntil {
				work.Intent.MaintenanceWindow = true
				var rollback map[string]any
				if rollbackErr := ingressProviderRequest(ctx, provider, "rollback", &work.Intent, &rollback); rollbackErr == nil {
					var confirmed map[string]any
					if ingressProviderRequest(ctx, provider, "status", &work.Intent, &confirmed) == nil && providerReceiptMatches(confirmed, d, "released") {
						return fmt.Errorf("public HTTPS verification failed; original proxy restored")
					}
				}
			}
			return fmt.Errorf("public HTTPS verification failed; inspect retained proxy ownership")
		}
	}
	if e = m.checkOwner(*op); e != nil {
		return e
	}
	if _, e = m.ingressActive(d.InstallID, d.ID, false); e != nil {
		return e
	}
	binding, e := m.ingressBinding(d.InstallID)
	if e != nil {
		return e
	}
	binding.Phase = phase
	binding.Receipt = status
	work.Receipt = status
	if phase == "active" {
		if d.Placement.Ingress != nil && d.Placement.Ingress.Provider == nil {
			d.LegacyIngressHostname = d.Placement.Ingress.Hostname
		}
		if d.Placement.Ingress == nil {
			d.Placement.Ingress = &runtimeIngressSpec{Hostname: work.Request.Hostname}
		}
		d.Placement.Ingress.Provider = &work.Request.Provider
		runtimeIngressEndpoints(&d)
	} else {
		d.Placement = binding.PreviousPlacement
		d.LegacyIngressHostname = ""
		d.Execution.BrowserEndpoint = binding.PreviousBrowserEndpoint
		d.Execution.Endpoints = binding.PreviousEndpoints
	}
	tx, e := m.s.store.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var actual string
	var lease int64
	if e = tx.QueryRow(`SELECT owner,lease_until FROM app_runtime_operations WHERE id=? AND status='running' AND cancel_requested=0`, op.ID).Scan(&actual, &lease); e != nil || actual != m.owner || lease < time.Now().Unix() {
		return fmt.Errorf("ingress operation lease lost before publication")
	}
	raw := runtimeJSON(runtimeStoredDeployment{runtimeDeployment: d, Credential: d.CredentialEncrypted, Environment: d.EnvironmentEncrypted, Bin: d.LocalBin, Port: d.LocalPort, PID: d.LocalPID})
	if _, e = tx.Exec(`UPDATE app_runtime_deployments SET record_json=? WHERE id=? AND install_id=? AND generation=?`, raw, d.ID, d.InstallID, d.Generation); e != nil {
		return e
	}
	result, e := tx.Exec(`UPDATE app_installs SET runtime_placement_json=? WHERE id=? AND runtime_active_deployment=? AND runtime_generation=? AND runtime_pending_deployment=''`, runtimeJSON(d.Placement), d.InstallID, d.ID, d.Generation)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("active deployment changed during ingress publication")
	}
	if _, e = tx.Exec(`UPDATE app_runtime_ingress_bindings SET record_json=? WHERE install_id=?`, runtimeJSON(binding), d.InstallID); e != nil {
		return e
	}
	return tx.Commit()
}
func runtimeIngressEndpoints(d *runtimeDeployment) {
	if d.Placement.Ingress == nil || d.Manifest.Runtime.Ingress == nil {
		return
	}
	base := "https://" + d.Placement.Ingress.Hostname
	d.Execution.Endpoints = map[string]string{}
	for name, path := range d.Manifest.Runtime.Ingress.Endpoints {
		d.Execution.Endpoints[name] = base + path
	}
	if path := d.Manifest.Runtime.Ingress.BrowserPath; path != "" {
		d.Execution.BrowserEndpoint = base + path
	}
}
func (m *appRuntimeManager) verifyIngressHTTPS(ctx context.Context, hostname, deployment string) error {
	req, e := http.NewRequestWithContext(ctx, "GET", "https://"+hostname+"/.well-known/apteva-runtime-ready", nil)
	if e != nil {
		return e
	}
	response, e := m.http.Do(req)
	if e != nil {
		return fmt.Errorf("trusted HTTPS ingress unavailable")
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 256))
	if e != nil || response.StatusCode != 200 || strings.TrimSpace(string(raw)) != deployment {
		return fmt.Errorf("public ingress does not identify selected deployment")
	}
	return nil
}
func (s *Server) handleIngressAdoption(w http.ResponseWriter, r *http.Request, id int64, action string) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectViewer); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var input runtimeIngressAdoptionRequest
	if runtimeDecode(r, &input) != nil {
		http.Error(w, "invalid ingress adoption request", 400)
		return
	}
	m := s.appRuntimeManager()
	if action == "inspect" {
		plan, e := m.inspectIngressAdoption(id, input)
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		writeJSON(w, plan)
		return
	}
	if action == "status" {
		binding, e := m.ingressBinding(id)
		if e != nil {
			http.Error(w, "owned ingress unavailable", 404)
			return
		}
		d, e := m.loadDeployment(binding.DeploymentID)
		if e != nil {
			http.Error(w, "deployment record unavailable", 409)
			return
		}
		intent, e := m.ingressIntent(d, *binding.Spec.Provider, binding.Spec.Hostname, "ingress-status")
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		intent.HostKey = binding.HostKey
		provider, e := m.ingressProvider(*binding.Spec.Provider, intent.ProjectID)
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		var receipt map[string]any
		if e = ingressProviderRequest(r.Context(), provider, "status", &intent, &receipt); e != nil {
			http.Error(w, "proxy outcome unavailable; ownership retained", 409)
			return
		}
		writeJSON(w, map[string]any{"binding": binding, "provider": receipt})
		return
	}
	op, e := m.beginIngress(id, action, input)
	if e != nil {
		http.Error(w, e.Error(), 409)
		return
	}
	writeJSONStatus(w, 202, op)
}

func (m *appRuntimeManager) prepareProviderIngress(d *runtimeDeployment, b runtimeBackend) error {
	if !b.Describe().IngressUpstream {
		return fmt.Errorf("backend must advertise ingress_upstream before managed proxy deployment")
	}
	ref := d.Placement.Ingress.Provider
	binding, e := m.ingressBinding(d.InstallID)
	if e == nil && (binding.Phase == "active" || binding.Phase == "prepared" || binding.Phase == "verifying") {
		if binding.Spec.Provider == nil || binding.Spec.Hostname != d.Placement.Ingress.Hostname || binding.Spec.Provider.InstallID != ref.InstallID || binding.Spec.Provider.ID != ref.ID || runtimeJSON(binding.Spec.Provider.Target) != runtimeJSON(ref.Target) {
			return fmt.Errorf("ingress provider/target changed; explicit adoption required")
		}
		d.Placement.Ingress.Provider = binding.Spec.Provider
		runtimeIngressEndpoints(d)
		return m.saveDeployment(*d)
	}
	if e != sql.ErrNoRows {
		return fmt.Errorf("retained ingress ownership must be reconciled before preparation")
	}
	var project string
	if e = m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, d.InstallID).Scan(&project); e != nil {
		return e
	}
	provider, e := m.ingressProvider(*ref, project)
	if e != nil {
		return e
	}
	options := map[string]any{}
	for key, value := range ref.Options {
		options[key] = value
	}
	options["hostname"] = d.Placement.Ingress.Hostname
	intent := runtimeIngressIntent{Contract: runtimeIngressContract, ControllerID: HashAPIKey("ingress-controller:" + string(m.s.secret)), OperationID: "ingress-prepare-" + d.ID, InstallID: d.InstallID, DeploymentID: d.ID, Generation: d.Generation, ProjectID: project, Target: ref.Target, Hostname: d.Placement.Ingress.Hostname, Routes: d.Manifest.Runtime.Ingress.Routes, Options: options}
	var receipt map[string]any
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
	defer cancel()
	if e = ingressProviderRequest(ctx, provider, "prepare", &intent, &receipt); e != nil {
		return e
	}
	key, _ := receipt["host_key"].(string)
	resolved, ok := receipt["options"].(map[string]any)
	if receipt["phase"] != "prepared" || key == "" || !ok {
		return fmt.Errorf("ingress preparation unconfirmed")
	}
	ref.Options = resolved
	binding = runtimeIngressBinding{Spec: *d.Placement.Ingress, HostKey: key, Phase: "prepared", OriginalDeploymentID: d.ID, PreviousPlacement: d.Placement, DeploymentID: d.ID, Generation: d.Generation}
	if _, e = m.s.store.db.Exec(`INSERT INTO app_runtime_ingress_bindings(install_id,record_json) VALUES(?,?)`, d.InstallID, runtimeJSON(binding)); e != nil {
		return e
	}
	runtimeIngressEndpoints(d)
	return m.saveDeployment(*d)
}

func (m *appRuntimeManager) publishProviderIngress(d runtimeDeployment, op runtimeOperation) error {
	if e := m.requirePublication(d, op); e != nil {
		return e
	}
	binding, e := m.ingressBinding(d.InstallID)
	if e != nil || binding.Phase != "active" && binding.Phase != "prepared" && binding.Phase != "verifying" || binding.Spec.Provider == nil {
		return fmt.Errorf("owned ingress provider unavailable")
	}
	if binding.Spec.Hostname != d.Placement.Ingress.Hostname || runtimeJSON(binding.Spec.Provider) != runtimeJSON(d.Placement.Ingress.Provider) {
		return fmt.Errorf("ingress provider changed; explicit adoption required")
	}
	intent, e := m.ingressIntent(d, *binding.Spec.Provider, binding.Spec.Hostname, op.ID)
	if e != nil {
		return e
	}
	intent.HostKey = binding.HostKey
	provider, e := m.ingressProvider(*binding.Spec.Provider, intent.ProjectID)
	if e != nil {
		return e
	}
	if provider.registration.Caps.IngressIdentityRequired && (d.Execution.IngressHostKey == "" || d.Execution.IngressHostKey != binding.HostKey) {
		return fmt.Errorf("upstream and ingress target host identities differ")
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	var current map[string]any
	if err := ingressProviderRequest(ctx, provider, "status", &intent, &current); err == nil && current["phase"] == "active" && current["source_matches"] == true {
		actualID, _ := current["deployment_id"].(string)
		gen, _ := current["generation"].(float64)
		if actualID != binding.DeploymentID {
			actual, err := m.loadDeployment(actualID)
			allowed := actualID == op.DeploymentID || actualID == op.FailedID || actualID == op.PreviousID || actualID == op.RollbackID
			if err != nil || actual.InstallID != d.InstallID || actual.Generation != int64(gen) || !allowed {
				return fmt.Errorf("proxy owner changed outside this deployment operation")
			}
			binding.DeploymentID = actualID
			binding.Generation = int64(gen)
			binding.Phase = "active"
			if _, err = m.s.store.db.Exec(`UPDATE app_runtime_ingress_bindings SET record_json=? WHERE install_id=?`, runtimeJSON(binding), d.InstallID); err != nil {
				return err
			}
		}
	}
	intent.ExpectedDeploymentID = binding.DeploymentID
	intent.ExpectedGeneration = binding.Generation
	// Nonexclusive cutovers need a separate drain window before replacing a proxy.
	// Retiring a still-serving upstream immediately after a reload is unsafe for WS.
	if binding.Phase == "active" && binding.DeploymentID != d.ID {
		previous, e := m.loadDeployment(binding.DeploymentID)
		if e != nil {
			return fmt.Errorf("previous ingress deployment ownership unavailable")
		}
		if previous.Status != "stopped" || !previous.Execution.StoppedConfirmed {
			if binding.DeploymentID != op.PreviousID && binding.DeploymentID != op.FailedID && binding.DeploymentID != op.DeploymentID {
				return fmt.Errorf("proxy predecessor is outside this operation")
			}
			if e = m.phase(&op, "draining_ingress_predecessor"); e != nil {
				return e
			}
			if e = m.drain(previous, op); e != nil {
				return e
			}
			if e = m.stopDeployment(&previous, op); e != nil {
				return e
			}
		}
	}
	var plan map[string]any
	if e = ingressProviderRequest(ctx, provider, "inspect", &intent, &plan); e != nil {
		return e
	}
	hash, _ := plan["plan_sha256"].(string)
	if !runtimeDigest(hash) || plan["host_key"] != binding.HostKey {
		return fmt.Errorf("ingress target/plan changed")
	}
	intent.PlanSHA256 = hash
	intent.MaintenanceWindow = true
	if e = m.requirePublication(d, op); e != nil {
		return e
	}
	var receipt map[string]any
	if e = ingressProviderRequest(ctx, provider, "publish", &intent, &receipt); e != nil {
		return e
	}
	if e = ingressProviderRequest(ctx, provider, "status", &intent, &receipt); e != nil || !providerReceiptMatches(receipt, d, "active") {
		return fmt.Errorf("provider publication unconfirmed")
	}
	binding.DeploymentID = d.ID
	binding.Generation = d.Generation
	binding.Receipt = receipt
	binding.Phase = "verifying"
	if _, e = m.s.store.db.Exec(`UPDATE app_runtime_ingress_bindings SET record_json=? WHERE install_id=?`, runtimeJSON(binding), d.InstallID); e != nil {
		return e
	}
	if e = m.verifyIngressHTTPS(ctx, binding.Spec.Hostname, d.ID); e != nil {
		return e
	}
	if e = m.requirePublication(d, op); e != nil {
		return e
	}
	binding.Phase = "active"
	binding.DeploymentID = d.ID
	binding.Generation = d.Generation
	binding.Receipt = receipt
	_, e = m.s.store.db.Exec(`UPDATE app_runtime_ingress_bindings SET record_json=? WHERE install_id=?`, runtimeJSON(binding), d.InstallID)
	return e
}
func (m *appRuntimeManager) unpublishProviderIngress(d runtimeDeployment, op runtimeOperation) error {
	if d.Placement.Ingress == nil || d.Placement.Ingress.Provider == nil {
		return nil
	}
	if e := m.checkOwner(op); e != nil {
		return e
	}
	binding, e := m.ingressBinding(d.InstallID)
	if e != nil || binding.Spec.Provider == nil {
		return fmt.Errorf("retained ingress ownership unavailable")
	}
	intent, e := m.ingressIntent(d, *binding.Spec.Provider, binding.Spec.Hostname, op.ID)
	if e != nil {
		return e
	}
	intent.HostKey = binding.HostKey
	provider, e := m.ingressProvider(*binding.Spec.Provider, intent.ProjectID)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	var receipt map[string]any
	return ingressProviderRequest(ctx, provider, "unpublish", &intent, &receipt)
}

// Deleting an installation releases only an already-unpublished provider resource.
// The adapter leaves a harmless 404 vhost and does not stop a shared proxy service.
func (m *appRuntimeManager) releaseProviderIngress(id int64) error {
	binding, e := m.ingressBinding(id)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	if binding.Phase == "released" {
		_, e = m.s.store.db.Exec(`DELETE FROM app_runtime_ingress_bindings WHERE install_id=?`, id)
		return e
	}
	d, e := m.loadDeployment(binding.DeploymentID)
	if e != nil || !d.Execution.StoppedConfirmed || d.Status != "stopped" && d.Status != "deleted" {
		return fmt.Errorf("confirmed stopped ingress owner required before release")
	}
	intent, e := m.ingressIntent(d, *binding.Spec.Provider, binding.Spec.Hostname, "ingress-release")
	if e != nil {
		return e
	}
	intent.HostKey = binding.HostKey
	provider, e := m.ingressProvider(*binding.Spec.Provider, intent.ProjectID)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	var receipt map[string]any
	if e = ingressProviderRequest(ctx, provider, "release", &intent, &receipt); e != nil {
		return e
	}
	if receipt["phase"] != "released" || receipt["unpublished"] != true {
		return fmt.Errorf("ingress release unconfirmed")
	}
	_, e = m.s.store.db.Exec(`DELETE FROM app_runtime_ingress_bindings WHERE install_id=?`, id)
	return e
}

func ingressProviderRequest(ctx context.Context, p *remoteRuntimeBackend, action string, in, out any) error {
	e := p.request(ctx, "POST", action, in, out)
	var problem *runtimeBackendHTTPError
	if errors.As(e, &problem) && regexp.MustCompile(`^[a-z_]{1,128}$`).MatchString(problem.Code) {
		return fmt.Errorf("ingress %s: %s", action, problem.Code)
	}
	return e
}
