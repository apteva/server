package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type appRuntimeManager struct {
	s       *Server
	ctx     context.Context
	cancel  context.CancelFunc
	owner   string
	http    *http.Client
	mu      sync.Mutex
	workers map[string]bool
	wg      sync.WaitGroup
}

func (s *Server) appRuntimeManager() *appRuntimeManager {
	s.runtimeOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.appRuntime = &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: rand.Text(), http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, workers: map[string]bool{}}
	})
	return s.appRuntime
}
func (m *appRuntimeManager) Close() { m.cancel(); m.wg.Wait(); m.http.CloseIdleConnections() }
func (m *appRuntimeManager) token(d runtimeDeployment) (string, error) {
	if d.CredentialEncrypted == "" {
		return m.s.appInstallToken(d.InstallID)
	}
	return Decrypt(m.s.secret, d.CredentialEncrypted)
}
func (m *appRuntimeManager) request(d runtimeDeployment, op runtimeOperation, action string) (runtimeBackendRequest, error) {
	var project string
	if err := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, d.InstallID).Scan(&project); err != nil {
		return runtimeBackendRequest{}, err
	}
	options := make(map[string]any, len(d.Placement.Options)+2)
	for k, v := range d.Placement.Options {
		options[k] = v
	}
	if d.Placement.Ingress != nil {
		options["ingress"] = map[string]any{"hostname": d.Placement.Ingress.Hostname}
		delete(options, "endpoints")
		delete(options, "browser_endpoint")
	}
	if !d.Placement.local() {
		if v, ok := options["gateway_url"]; ok && v != d.Placement.GatewayURL {
			return runtimeBackendRequest{}, fmt.Errorf("gateway option conflicts with placement gateway_url")
		}
		options["gateway_url"] = d.Placement.GatewayURL
	}
	req := runtimeBackendRequest{TargetInfo: d.TargetInfo, Contract: runtimeBackendContract, OperationID: op.ID, IdempotencyKey: op.ID + ":" + d.ID, DeploymentID: d.ID, InstallID: d.InstallID, Generation: d.Generation, ProjectID: project, Target: d.Placement.Target, Options: options, Manifest: d.Manifest, Execution: d.Execution, StateRef: d.Execution.StateRef, GatewayURL: d.Placement.GatewayURL}
	if d.Package != nil {
		if v, ok := options["artifact_id"]; ok && v != d.Package.Metadata.ID {
			return req, fmt.Errorf("artifact option conflicts with prepared package")
		}
		options["artifact_id"] = d.Package.Metadata.ID
		req.ArtifactPlatform = d.Package.Metadata.Platform
		req.Package = &runtimePackageReference{ID: d.Package.Metadata.ID, Platform: d.Package.Metadata.Platform, SHA256: d.Package.MetadataSHA256, ExecutableSHA256: d.Package.ExecutableSHA256, ArchiveSHA256: d.Package.ArchiveSHA256, ABI: d.Package.ABI, SystemLibraries: d.Package.SystemLibraries}
	}
	if action == "start" {
		raw, err := Decrypt(m.s.secret, d.EnvironmentEncrypted)
		if err != nil {
			return req, err
		}
		if err = json.Unmarshal([]byte(raw), &req.Environment); err != nil {
			return req, err
		}
		req.Environment["APTEVA_RUNTIME_ENDPOINTS"] = runtimeJSON(d.Execution.Endpoints)
	}
	return req, nil
}
func (m *appRuntimeManager) makeDeployment(id int64, p runtimePlacement, manifest *sdk.Manifest, replica int) (runtimeDeployment, error) {
	if err := p.validate(); err != nil {
		return runtimeDeployment{}, err
	}
	if err := validateAppRuntimeCompatibility(manifest, Version); err != nil {
		return runtimeDeployment{}, err
	}
	var gen int64
	if err := m.s.store.db.QueryRow(`SELECT runtime_generation+1 FROM app_installs WHERE id=?`, id).Scan(&gen); err != nil {
		return runtimeDeployment{}, err
	}
	d := runtimeDeployment{ID: rand.Text(), InstallID: id, Generation: gen, Replica: replica, Placement: p, Manifest: *manifest, Status: "preparing", CreatedAt: runtimeTimestamp(), AppCaps: m.appCapabilities(id)}
	if d.Placement.Type == "" {
		d.Placement.Type = "local"
	}
	token := rand.Text()
	encrypted, err := Encrypt(m.s.secret, token)
	if err != nil {
		return d, err
	}
	d.CredentialEncrypted = encrypted
	cfg, err := decryptInstallConfig(m.s, id)
	if err != nil {
		return d, err
	}
	var project string
	m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, id).Scan(&project)
	gateway := m.s.localGatewayURL()
	if !p.local() {
		gateway = p.GatewayURL
	}
	env := map[string]string{"APTEVA_APP_TOKEN": token, "APTEVA_OUTBOUND_TOKEN": token, "APTEVA_GATEWAY_URL": gateway, "APTEVA_PUBLIC_URL": m.s.publicBaseURL(), "APTEVA_INSTALL_ID": fmt.Sprint(id), "APTEVA_PROJECT_ID": project, "APTEVA_APP_CONFIG": runtimeJSON(cfg), "APTEVA_DEPLOYMENT_ID": d.ID, "APTEVA_DEPLOYMENT_GENERATION": fmt.Sprint(gen), "APTEVA_RUNTIME_MODE": "standby"}
	for k, v := range manifest.Runtime.Env {
		if v.From == "" && !strings.HasPrefix(k, "APTEVA_") {
			env[k] = v.Value
		}
	}
	d.EnvironmentEncrypted, err = Encrypt(m.s.secret, runtimeJSON(env))
	return d, err
}
func (m *appRuntimeManager) ensureExisting(id int64) error {
	var managed int
	var status, manifest, bin, endpoint string
	var port, pid int
	var generation int64
	err := m.s.store.db.QueryRow(`SELECT runtime_managed,status,COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),local_bin_path,local_port,local_pid,sidecar_url_override,runtime_generation FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).Scan(&managed, &status, &manifest, &bin, &port, &pid, &endpoint, &generation)
	if err != nil {
		return err
	}
	if managed == 1 {
		return nil
	}
	if m.s.localApps != nil {
		if !m.s.localApps.acquireInstall(id) {
			return fmt.Errorf("legacy install operation is still running")
		}
		defer m.s.localApps.releaseInstall(id)
	}
	if status != "running" && status != "pending" && status != "disabled" {
		return fmt.Errorf("installation is not ready for managed placement")
	}
	var sm sdk.Manifest
	if err = json.Unmarshal([]byte(manifest), &sm); err != nil {
		return err
	}
	if sm.Runtime.Kind == "static" && status == "running" {
		bin = strings.TrimPrefix(endpoint, "static://")
		if bin == endpoint {
			return fmt.Errorf("unmanaged remote static execution has no owner")
		}
	}
	var oldID string
	if status == "running" {
		if bin == "" || m.s.localApps == nil || (sm.Runtime.Kind != "static" && m.s.localApps.currentProc(id) == nil) {
			return fmt.Errorf("cannot adopt an unmanaged external process; register its owning backend first")
		}
		var existing string
		if e := m.s.store.db.QueryRow(`SELECT id FROM app_runtime_deployments WHERE install_id=? AND generation=? AND replica=0`, id, generation).Scan(&existing); e == nil {
			oldID = existing
		} else {
			oldID = rand.Text()
		}
		d := runtimeDeployment{ID: oldID, InstallID: id, Generation: generation, Manifest: sm, Status: "active", Placement: runtimePlacement{Type: "local"}, LocalBin: bin, LocalPort: port, LocalPID: pid, CreatedAt: runtimeTimestamp(), AppCaps: m.appCapabilities(id), Execution: runtimeExecution{Handle: oldID, State: "running", Ready: true, HostKey: "server:local", Endpoint: endpoint, StateRef: fmt.Sprintf("local:%d", id)}}
		p := m.s.localApps.currentProc(id)
		env := map[string]string{}
		if p != nil {
			env = p.spec.env
		}
		d.EnvironmentEncrypted, err = Encrypt(m.s.secret, runtimeJSON(env))
		if err != nil {
			return err
		}
		if err = m.saveDeployment(d); err != nil {
			return err
		}
	}
	_, err = m.s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1,runtime_active_deployment=?,runtime_placement_json='{"type":"local"}' WHERE id=? AND runtime_managed=0`, oldID, id)
	return err
}
func runtimeFingerprint(v any) string {
	b := sha256.Sum256([]byte(runtimeJSON(v)))
	return hex.EncodeToString(b[:])
}
func (m *appRuntimeManager) begin(id int64, kind, key string, p runtimePlacement, manifest *sdk.Manifest, desired ...int) (runtimeOperation, error) {
	if !runtimeID(key) {
		return runtimeOperation{}, fmt.Errorf("Idempotency-Key must be a bounded identifier")
	}
	replicas := 1
	if len(desired) > 0 {
		replicas = desired[0]
	}
	fingerprint := runtimeFingerprint(struct {
		Kind      string
		Placement runtimePlacement
		Manifest  *sdk.Manifest
		Replicas  int
	}{kind, p, manifest, replicas})
	var existing, fp string
	err := m.s.store.db.QueryRow(`SELECT id,fingerprint FROM app_runtime_operations WHERE install_id=? AND idempotency_key=?`, id, key).Scan(&existing, &fp)
	if err == nil {
		if fp != fingerprint {
			return runtimeOperation{}, fmt.Errorf("idempotency key reused with different request")
		}
		return m.operation(existing)
	}
	if err != sql.ErrNoRows {
		return runtimeOperation{}, err
	}
	if kind == "prepare" || kind == "upgrade" || kind == "restart" || kind == "deploy" {
		if err = m.normalizePlacement(&p); err != nil {
			return runtimeOperation{}, err
		}
		if err = p.validate(); err != nil {
			return runtimeOperation{}, err
		}
	}
	if !p.local() {
		if p.PoolID != "" {
			if err = m.resolvePool(id, &p); err != nil {
				return runtimeOperation{}, err
			}
		}
		if err = m.validatePlacementAccess(id, p); err != nil {
			return runtimeOperation{}, err
		}
		if _, err = m.registration(p.Backend); err != nil {
			return runtimeOperation{}, fmt.Errorf("execution backend is not registered")
		}
	}
	if err = m.ensureExisting(id); err != nil {
		return runtimeOperation{}, err
	}
	old, _ := m.active(id)
	d := runtimeDeployment{}
	if kind == "prepare" || kind == "upgrade" || kind == "restart" || kind == "deploy" {
		if manifest == nil {
			manifest, err = installManifest(m.s, id)
			if err != nil {
				return runtimeOperation{}, err
			}
		}
		d, err = m.makeDeployment(id, p, manifest, 0)
		if err != nil {
			return runtimeOperation{}, err
		}
	}
	if kind == "activate" || kind == "discard" {
		var pending string
		m.s.store.db.QueryRow(`SELECT runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&pending)
		d, err = m.loadDeployment(pending)
		if err != nil || (kind == "activate" && d.Status != "prepared") {
			return runtimeOperation{}, fmt.Errorf("no prepared deployment")
		}
	}
	op := runtimeOperation{ID: rand.Text(), InstallID: id, Kind: kind, Phase: "queued", Status: "queued", PreviousID: old.ID, DeploymentID: d.ID, IdempotencyKey: key, Fingerprint: fingerprint, CreatedAt: runtimeTimestamp(), UpdatedAt: runtimeTimestamp()}
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return op, err
	}
	defer tx.Rollback()
	if kind == "prepare" || kind == "upgrade" || kind == "restart" || kind == "deploy" {
		var pending string
		if err = tx.QueryRow(`SELECT runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&pending); err != nil {
			return op, err
		}
		if pending != "" {
			return op, fmt.Errorf("a candidate is already pending")
		}
		raw := runtimeJSON(runtimeStoredDeployment{runtimeDeployment: d, Credential: d.CredentialEncrypted, Environment: d.EnvironmentEncrypted})
		if _, err = tx.Exec(`INSERT INTO app_runtime_deployments(id,install_id,generation,replica,status,record_json) VALUES(?,?,?,?,?,?)`, d.ID, id, d.Generation, 0, d.Status, raw); err != nil {
			return op, err
		}
		if _, err = tx.Exec(`UPDATE app_installs SET runtime_pending_deployment=?,runtime_placement_json=?,runtime_generation=? WHERE id=?`, d.ID, runtimeJSON(p), d.Generation, id); err != nil {
			return op, err
		}
	}
	if _, err = tx.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,idempotency_key,fingerprint,record_json,desired_replicas) VALUES(?,?,?,?,?,?,?,?)`, op.ID, id, kind, "queued", key, fingerprint, runtimeJSON(op), replicas); err != nil {
		return op, fmt.Errorf("installation already has a deployment operation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return op, err
	}
	m.launch(op.ID)
	return op, nil
}
func (m *appRuntimeManager) validatePlacementAccess(id int64, p runtimePlacement) error {
	if id == p.Backend.InstallID {
		return fmt.Errorf("a backend cannot manage its own installation")
	}
	var project, bproject, status string
	if err := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, id).Scan(&project); err != nil {
		return err
	}
	if err := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,''),status FROM app_installs WHERE id=?`, p.Backend.InstallID).Scan(&bproject, &status); err != nil {
		return err
	}
	if status != "running" || (bproject != "" && bproject != project) {
		return fmt.Errorf("backend is unavailable in this project")
	}

	var tproject string
	if err := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, p.Target.InstallID).Scan(&tproject); err != nil {
		return fmt.Errorf("target owner is not installed")
	}
	if tproject != "" && tproject != project {
		return fmt.Errorf("target owner belongs to another project")
	}
	seen := map[int64]bool{id: true}
	cursor := p.Backend.InstallID
	for cursor > 0 {
		if seen[cursor] {
			return fmt.Errorf("runtime provider dependency cycle")
		}
		seen[cursor] = true
		var raw string
		if err := m.s.store.db.QueryRow(`SELECT runtime_placement_json FROM app_installs WHERE id=?`, cursor).Scan(&raw); err != nil {
			break
		}
		var q runtimePlacement
		if json.Unmarshal([]byte(raw), &q) != nil || q.local() {
			break
		}
		cursor = q.Backend.InstallID
	}
	return nil
}
func (m *appRuntimeManager) launch(id string) {
	if m.ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	if m.workers[id] {
		m.mu.Unlock()
		return
	}
	m.workers[id] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() { m.mu.Lock(); delete(m.workers, id); m.mu.Unlock() }()
		m.run(id)
	}()
}
func (m *appRuntimeManager) phase(op *runtimeOperation, p string) error {
	if p != "rolling_back" && p != "retiring_previous" {
		var cancelled int
		if err := m.s.store.db.QueryRow(`SELECT cancel_requested FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&cancelled); err != nil {
			return err
		}
		if cancelled != 0 {
			return errRuntimeCancelled
		}
	}
	op.Phase = p
	return m.saveOperation(*op)
}
func (m *appRuntimeManager) run(id string) {
	now := time.Now().Unix()
	res, err := m.s.store.db.Exec(`UPDATE app_runtime_operations SET owner=?,lease_until=?,status='running' WHERE id=? AND status IN ('queued','running') AND (owner='' OR lease_until<? OR owner=?)`, m.owner, now+45, id, now, m.owner)
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	op, err := m.operation(id)
	if err != nil {
		return
	}
	op.Status = "running"
	beat := make(chan struct{})
	defer close(beat)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-beat:
				return
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.s.store.db.Exec(`UPDATE app_runtime_operations SET lease_until=? WHERE id=? AND owner=?`, time.Now().Unix()+45, id, m.owner)
			}
		}
	}()
	switch op.Kind {
	case "prepare":
		err = m.prepare(&op)
	case "activate":
		err = m.activate(&op)
	case "stop":
		err = m.stopOperation(&op)
	case "delete":
		err = m.deleteOperation(&op)
	case "restart", "upgrade", "deploy":
		err = m.restartOperation(&op)
	case "scale":
		err = m.scaleOperation(&op)
	case "discard":
		err = m.discardOperation(&op)
	default:
		err = fmt.Errorf("unsupported runtime operation")
	}
	if m.ctx.Err() != nil {
		return
	}
	if err != nil {
		op.Status = "failed"
		if errors.Is(err, errRuntimeCancelled) {
			op.Status = "cancelled"
		}
		op.Error = err.Error()
		var currentStatus string
		m.s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, op.InstallID).Scan(&currentStatus)
		m.s.store.db.Exec(`UPDATE app_installs SET error_message=?,status_message=? WHERE id=?`, op.Error, "Deployment operation failed; inspect operation", op.InstallID)
	} else {
		op.Status = "succeeded"
		op.Phase = "complete"
	}
	_ = m.saveOperation(op)
}
func (m *appRuntimeManager) startReconciler() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			_, _ = m.discoverBackends(ctx)
			cancel()
			select {
			case <-m.ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.reconcile()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.reconcile()
				m.autoscale()
			}
		}
	}()
}
func (m *appRuntimeManager) checkOwner(op runtimeOperation) error {
	var owner string
	var lease int64
	if err := m.s.store.db.QueryRow(`SELECT owner,lease_until FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&owner, &lease); err != nil {
		return err
	}
	if owner != m.owner || lease < time.Now().Unix() || m.ctx.Err() != nil {
		return errors.New("deployment operation ownership lost")
	}
	return nil
}
