package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func runtimeDigest(s string) bool { v, e := hex.DecodeString(s); return e == nil && len(v) == 32 }
func (m *appRuntimeManager) prepare(op *runtimeOperation) error {
	d, err := m.loadDeployment(op.DeploymentID)
	if err != nil {
		return err
	}
	if d.Status == "prepared" {
		return nil
	}
	b, err := m.backend(d)
	if err != nil {
		return err
	}
	caps := b.Describe()
	if op.RollbackID == "" {
		source, e := m.existingSource(d.InstallID)
		if e != nil {
			return e
		}
		if source.Exists && !sameRuntimeTarget(source.Deployment, d) && runtimePersistentState(source.Deployment) {
			if err = m.pinStatePolicy(source.Deployment, d, op); err != nil {
				return err
			}
		}
		if err = m.checkInstallationMigration(m.ctx, d.InstallID, d.Manifest, d.Placement); err != nil {
			return err
		}
	}
	if err = runtimeBackendCompatibility(d.Manifest, caps); err != nil {
		return err
	}
	if err = m.prepareIngress(&d, b); err != nil {
		return err
	}
	if err = m.phase(op, "preparing"); err != nil {
		return err
	}
	if err = m.prepareDeploymentPackage(&d, op, b); err != nil {
		return err
	}
	req, err := m.request(d, *op, "prepare")
	if err != nil {
		return err
	}
	if old, e := m.loadDeployment(op.PreviousID); e == nil && sameRuntimeTarget(old, d) {
		req.StateRef = old.Execution.StateRef
	}
	if err = m.checkOwner(*op); err != nil {
		return err
	}
	// An exclusive backend may reserve the same service/state directory during
	// Prepare. Finish package validation first, then confirm the previous writer
	// stopped before asking the backend to prepare its replacement.
	if op.Kind != "prepare" && op.RollbackID == "" && d.Manifest.Runtime.DatabaseUpgrade == "requires_restore" {
		return fmt.Errorf("automatic activation blocked by database_upgrade requires_restore")
	}
	if op.Kind != "prepare" && op.RollbackID == "" {
		if old, e := m.loadDeployment(op.PreviousID); e == nil && sameRuntimeTarget(old, d) && (op.Recovery || runtimeExclusive(old, d) || d.Component != nil) && old.Status != "stopped" {
			if err = m.phase(op, "draining"); err != nil {
				return err
			}
			if err = m.drain(old, *op); err != nil {
				if !op.Recovery {
					_ = m.appLifecycle(old, "activate")
				}
				return err
			}
			if err = m.phase(op, "stopping_previous"); err != nil {
				_ = m.appLifecycle(old, "activate")
				return err
			}
			if err = m.s.markExclusiveActivationUnavailable(d.InstallID); err != nil {
				return err
			}
			if err = m.stopDeployment(&old, *op); err != nil {
				return err
			}
			if err = m.phase(op, "preparing"); err != nil {
				return err
			}
		}
	}
	execution, err := b.Prepare(req)
	if err != nil {
		return err
	}
	if saved, e := m.loadDeployment(d.ID); e == nil {
		d.LocalBin = saved.LocalBin
		d.LocalPort = saved.LocalPort
		d.LocalPID = saved.LocalPID
	}
	if d.TargetInfo != nil && execution.HostKey != d.TargetInfo.HostKey {
		return fmt.Errorf("prepared host differs from inspected target")
	}
	if execution.Handle == "" || execution.HostKey == "" {
		return fmt.Errorf("backend preparation must return a stable handle and reservation domain")
	}
	// Async preparation is inspectable and idempotent by deployment ID.
	deadline := time.Now().Add(30 * time.Minute)
	for execution.State == "preparing" || execution.State == "pending" {
		d.Execution = execution
		d.Status = "preparing"
		if err = m.saveDeployment(d); err != nil {
			return err
		}
		if err = m.phase(op, "preparing"); err != nil {
			return err
		}
		if err = m.checkOwner(*op); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("preparation deadline exceeded")
		}
		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		case <-time.After(time.Second):
		}
		inspectReq, e := m.request(d, *op, "inspect")
		if e != nil {
			return e
		}
		observed, e := b.Inspect(inspectReq)
		if e != nil {
			return e
		}
		execution, e = runtimeMergeExecution(execution, observed)
		if e != nil {
			return e
		}
	}
	if execution.State != "prepared" {
		return fmt.Errorf("backend package is not prepared")
	}
	if execution.Handle == "" || execution.HostKey == "" || !runtimeDigest(execution.ReleaseDigest) {
		return fmt.Errorf("backend preparation must return handle, host key and exact release digest")
	}
	if !d.Placement.local() {
		if len(execution.HostKey) > 512 {
			return fmt.Errorf("invalid backend physical host identity")
		}
		if d.Package != nil {
			if execution.Platform != d.Package.Metadata.Platform || execution.ReleaseDigest != d.Package.ExecutableSHA256 || execution.AppCapabilities != d.Package.Metadata.AppCapabilities {
				return fmt.Errorf("backend prepared a different package platform, executable or lifecycle declaration")
			}
		} else if len(d.Manifest.Runtime.Artifacts) > 0 {
			artifact, ok := d.Manifest.Runtime.Artifacts[execution.Platform]
			if !ok || artifact.SHA256 != execution.ReleaseDigest {
				return fmt.Errorf("backend did not prepare the matching checksummed artifact")
			}
		}
	}
	for _, p := range d.Manifest.Runtime.Ports {
		if p.HostPort == 0 {
			continue
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		found := false
		for _, allocated := range execution.Ports {
			if allocated.Protocol == proto && allocated.Start <= p.HostPort && max(allocated.End, allocated.Start) >= p.HostPort {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("backend omitted declared fixed listener")
		}
	}
	if runtimePersistentState(d) && execution.StateRef == "" {
		return fmt.Errorf("backend must provide a persistent state reference")
	}
	if req.StateRef != "" && execution.StateRef != req.StateRef {
		return fmt.Errorf("backend did not preserve the existing state reference")
	}
	d.Execution = execution
	if execution.AppCapabilities != (runtimeAppCaps{}) {
		if runtimeCapabilitiesKnown(d) && d.AppCaps != execution.AppCapabilities {
			return fmt.Errorf("backend lifecycle capabilities differ from the approved release")
		}
		d.AppCaps, d.AppCapsKnown = execution.AppCapabilities, true
	}
	d.Status = "preparing"
	if err = m.saveDeployment(d); err != nil {
		return err
	}
	if err = m.reservePorts(d); err != nil {
		return err
	}
	d.Status = "prepared"
	return m.saveDeployment(d)
}
func sameRuntimeTarget(a, b runtimeDeployment) bool {
	return a.Placement.local() && b.Placement.local() || (!a.Placement.local() && !b.Placement.local() && a.Placement.Backend == b.Placement.Backend && a.Placement.Target == b.Placement.Target)
}
func runtimeExclusive(a, b runtimeDeployment) bool {
	if a.Manifest.Runtime.ActivationStrategy == "exclusive" || b.Manifest.Runtime.ActivationStrategy == "exclusive" {
		return true
	}
	for _, d := range []runtimeDeployment{a, b} {
		for _, p := range d.Manifest.Runtime.Ports {
			if p.HostPort > 0 {
				return true
			}
		}
		for _, p := range d.Execution.Ports {
			if p.Exclusive {
				return true
			}
		}
	}
	return false
}
func (m *appRuntimeManager) stopDeployment(d *runtimeDeployment, op runtimeOperation) error {
	b, err := m.backend(*d)
	if err != nil {
		return err
	}
	req, err := m.request(*d, op, "stop")
	if err != nil {
		return err
	}
	if err = m.checkOwner(op); err != nil {
		return err
	}
	out, err := b.Stop(req)
	if err != nil || !out.StoppedConfirmed {
		if !b.Describe().Fencing {
			return fmt.Errorf("previous execution could not be confirmed stopped; replacement refused")
		}
		out, err = b.Fence(req)
	}
	if err != nil || !out.StoppedConfirmed {
		return fmt.Errorf("previous execution is neither stopped nor fenced")
	}
	d.Status = "stopped"
	d.Execution.State = "stopped"
	d.Execution.StoppedConfirmed = true
	d.Execution.Ready = false
	return m.saveDeployment(*d)
}
func (m *appRuntimeManager) startDeployment(d *runtimeDeployment, op runtimeOperation) error {
	b, err := m.backend(*d)
	if err != nil {
		return err
	}
	req, err := m.request(*d, op, "start")
	if err != nil {
		return err
	}
	// Persist before the side effect: inspection by deployment ID recovers a
	// start whose response or subsequent persistence was lost.
	d.Status = "starting"
	if err = m.saveDeployment(*d); err != nil {
		return err
	}
	if err = m.checkOwner(op); err != nil {
		return err
	}
	out, err := b.Start(req)
	if err != nil {
		return err
	}
	if out.Handle != "" {
		out, err = runtimeMergeExecution(d.Execution, out)
		if err != nil {
			return err
		}
		d.Execution = out
	}
	d.Status = "starting"
	if err = m.saveDeployment(*d); err != nil {
		return err
	}
	return m.waitReady(d, op)
}
func (m *appRuntimeManager) waitReady(d *runtimeDeployment, op runtimeOperation) error {
	budget := 30 * time.Second
	if d.Manifest.Runtime.StartupTimeoutSeconds > 0 {
		budget = time.Duration(d.Manifest.Runtime.StartupTimeoutSeconds) * time.Second
	}
	end := time.Now().Add(budget)
	for {
		if err := m.checkOwner(op); err != nil {
			return err
		}
		b, err := m.backend(*d)
		if err != nil {
			return err
		}
		req, err := m.request(*d, op, "inspect")
		if err != nil {
			return err
		}
		out, err := b.Inspect(req)
		if err == nil && out.Ready && d.Manifest.Runtime.Kind == "static" && d.Placement.local() {
			d.Execution = out
			d.Status = "ready"
			return m.saveDeployment(*d)
		}
		if err == nil && out.Ready && out.State == "running" {
			if err = runtimeSecureURL(out.Endpoint); err != nil {
				return err
			}
			for name, endpoint := range out.Endpoints {
				if !runtimeID(name) {
					return fmt.Errorf("invalid endpoint name")
				}
				if err = runtimePublicEndpoint(endpoint); err != nil {
					return err
				}
			}
			if out.BrowserEndpoint != "" {
				if err = runtimeSecureURL(out.BrowserEndpoint); err != nil {
					return err
				}
			}
			token, e := m.token(*d)
			if e != nil {
				return e
			}
			r, _ := http.NewRequestWithContext(m.ctx, http.MethodGet, runtimeEndpoint(out.Endpoint, d.Manifest.Runtime.HealthCheck), nil)
			if d.Manifest.Runtime.HealthCheck == "" {
				r, _ = http.NewRequestWithContext(m.ctx, http.MethodGet, runtimeEndpoint(out.Endpoint, "/health"), nil)
			}
			r.Header.Set("Authorization", "Bearer "+token)
			resp, e := m.http.Do(r)
			if e == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					if err := m.verifyRuntimeCapabilities(d, out.Endpoint, token); err != nil {
						return err
					}
					if err := m.verifyComponentWorker(*d, out.Endpoint, token); err != nil {
						return err
					}
					if d.Package != nil {
						if err := m.verifyPackagedAppCapabilities(*d, out.Endpoint, token); err != nil {
							return err
						}
					}
					out, err = runtimeMergeExecution(d.Execution, out)
					if err != nil {
						return err
					}
					d.Execution = out
					d.Status = "ready"
					return m.saveDeployment(*d)
				}
			}
		}
		if err == nil && (out.State == "failed" || out.State == "stopped") {
			return fmt.Errorf("candidate exited before readiness")
		}
		if time.Now().After(end) {
			return fmt.Errorf("runtime readiness deadline exceeded")
		}
		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func (m *appRuntimeManager) activate(op *runtimeOperation) error {
	if op.RollbackID != "" {
		old, err := m.loadDeployment(op.PreviousID)
		if err != nil {
			return err
		}
		failed, err := m.loadDeployment(op.FailedID)
		if err != nil {
			return err
		}
		return m.rollback(op, &old, &failed, errors.New("recovering interrupted activation"))
	}

	d, err := m.loadDeployment(op.DeploymentID)
	if err != nil {
		return err
	}
	old, oldErr := m.loadDeployment(op.PreviousID)
	if oldErr != nil && oldErr != sql.ErrNoRows && op.PreviousID != "" {
		return oldErr
	}
	// Recovery must not be blocked by forward-migration preflight checks.
	if op.Phase == "rolling_back" {
		return m.rollback(op, &old, &d, errors.New("recovering interrupted activation"))
	}
	if d.Status == "active" {
		current, e := m.active(d.InstallID)
		if e == nil && current.ID == d.ID {
			return m.retirePrevious(op, old)
		}
	}
	if d.Manifest.Runtime.DatabaseUpgrade == "requires_restore" {
		return fmt.Errorf("automatic activation blocked by database_upgrade requires_restore")
	}
	if old.ID != "" && old.Placement.local() {
		present, e := runtimeStatePresent(m.localStateDir(old))
		if e != nil {
			return e
		}
		if present && !old.PersistentData {
			old.PersistentData = true
			if err = m.saveDeployment(old); err != nil {
				return err
			}
		}
	}
	migration := old.ID != "" && (!sameRuntimeTarget(old, d) || runtimePersistentState(old) && old.Execution.StateRef != d.Execution.StateRef)
	if migration {
		if runtimePersistentState(old) {
			if err = m.pinStatePolicy(old, d, op); err != nil {
				return err
			}
		}
		if err = firstRuntimeBlock(m.migrationChecks(m.ctx, old, d.Manifest, d.Placement, &d.AppCaps, true, runtimeMigrationCheckOptions{Policy: op.StateTransfer})); err != nil {
			return err
		}
	}
	exclusive := op.Recovery || migration || runtimeExclusive(old, d) || d.Component != nil
	if old.ID != "" && exclusive && old.Status != "stopped" {
		if err = m.phase(op, "draining"); err != nil {
			return err
		}
		if err = m.drain(old, *op); err != nil {
			return err
		}
		if err = m.phase(op, "stopping_previous"); err != nil {
			_ = m.appLifecycle(old, "activate")
			return err
		}
		if err = m.s.markExclusiveActivationUnavailable(d.InstallID); err != nil {
			return err
		}
		if err = m.stopDeployment(&old, *op); err != nil {
			if !op.Recovery && m.waitReady(&old, *op) == nil {
				old.Status = "active"
				_ = m.saveDeployment(old)
				_ = m.appLifecycle(old, "activate")
				_ = m.restoreOldRoute(old)
			}
			return err
		}
	}
	if migration && runtimePersistentState(old) {
		if err = m.phase(op, "transferring_state"); err != nil {
			return m.rollback(op, &old, &d, err)
		}
		if err = m.transferState(&old, &d, op); err != nil {
			return m.rollback(op, &old, &d, err)
		}
	}
	if old.ID == "" || old.Status == "stopped" {
		if _, err = m.s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, d.InstallID); err != nil {
			return err
		}
	}
	if err = m.phase(op, "starting_candidate"); err != nil {
		return m.rollback(op, &old, &d, err)
	}
	if migration && runtimePersistentState(old) && !op.CandidateStarted {
		op.CandidateStarted = true
		if err = m.saveOperation(*op); err != nil {
			return m.rollback(op, &old, &d, err)
		}
	}
	if d.Status == "starting" || d.Status == "ready" {
		b, e := m.backend(d)
		if e != nil {
			return e
		}
		req, _ := m.request(d, *op, "inspect")
		out, e := b.Inspect(req)
		if e == nil && out.State == "running" {
			merged, e := runtimeMergeExecution(d.Execution, out)
			if e != nil {
				return m.rollback(op, &old, &d, e)
			}
			d.Execution = merged
			err = m.waitReady(&d, *op)
		} else {
			err = m.startDeployment(&d, *op)
		}
	} else {
		err = m.startDeployment(&d, *op)
	}
	if err != nil {
		return m.rollback(op, &old, &d, err)
	}
	if err = m.phase(op, "committing"); err != nil {
		return m.rollback(op, &old, &d, err)
	}
	if err = m.checkOwner(*op); err != nil {
		return err
	}
	if err = m.activateAndConfirm(d, *op); err != nil {
		return m.rollback(op, &old, &d, err)
	}
	if err = m.publishIngress(d, *op); err != nil {
		return m.rollback(op, &old, &d, err)
	}
	if err = m.commitActive(d); err != nil {
		current, e := m.active(d.InstallID)
		if e == nil && current.ID == d.ID {
			return fmt.Errorf("deployment committed; post-activation registration requires reconciliation")
		}
		return m.rollback(op, &old, &d, err)
	}
	return m.retirePrevious(op, old)
}
func (m *appRuntimeManager) drain(d runtimeDeployment, op runtimeOperation) error {
	if !d.AppCaps.Drain {
		return nil
	}
	// A fenced predecessor has already stopped media. Its database may still
	// contain unfinished carrier calls after the outage; these cannot become a
	// 330-second graceful-drain prerequisite for verified recovery. Fresh exact
	// identity/admission evidence is required; Stop still must be confirmed.
	if op.Recovery && d.ID == op.PreviousID {
		health := m.runtimeAppHealth(d, d.Execution)
		if health.Admission == "fenced" && health.Recovery == "restart" {
			return nil
		}
		return fmt.Errorf("fenced recovery no longer verified; previous execution retained")
	}
	ctx, cancel := m.stateTransferContext(op, runtimeStateTransferPolicy{TimeoutSeconds: 330})
	defer cancel()
	var last error
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("app drain interrupted (%v): %w", last, ctx.Err())
		}
		if err := m.checkOwner(op); err != nil {
			return err
		}
		// A lost acknowledgement or recovered SSH tunnel can change both process
		// state and control address. Never infer a successful drain from a timeout.
		if b, err := m.backend(d); err == nil {
			if req, err := m.request(d, op, "inspect"); err == nil {
				inspect := b.Inspect
				if contextual, ok := b.(interface {
					inspectContext(context.Context, runtimeBackendRequest) (runtimeExecution, error)
				}); ok {
					inspect = func(req runtimeBackendRequest) (runtimeExecution, error) { return contextual.inspectContext(ctx, req) }
				}
				if out, err := inspect(req); err == nil {
					merged, err := runtimeMergeExecution(d.Execution, out)
					if err != nil {
						return err
					}
					if out.StoppedConfirmed && out.State == "stopped" {
						return nil
					}
					d.Execution = merged
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = m.appLifecycleContext(ctx, d, "drain")
		if last == nil {
			return nil
		}
		// Keep progress visible while distinguishing real active work from a broken
		// management path. No automatic stop of an unconfirmed draining execution.
		phase := "drain_unreachable"
		if errors.Is(last, errRuntimeDraining) {
			phase = "draining"
		}
		if err := m.phase(&op, phase); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("app drain interrupted (%v): %w", last, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
func (m *appRuntimeManager) commitActive(d runtimeDeployment) error {
	d.Status = "active"
	if d.Placement.local() {
		ports, e := fixedRuntimePorts(&d.Manifest)
		if e != nil {
			return e
		}
		if e = m.s.localApps.RestoreCommittedFixedPorts(d.InstallID, d.Manifest.Name, ports); e != nil {
			return e
		}
	}
	if err := m.saveDeployment(d); err != nil {
		return err
	}
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The active pointer and install routing change in one database transaction.
	if _, err = tx.Exec(`UPDATE app_runtime_deployments SET status='retiring',record_json=json_set(record_json,'$.status','retiring') WHERE install_id=? AND generation!=? AND status IN ('active','ready')`, d.InstallID, d.Generation); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE app_installs SET runtime_active_deployment=?,runtime_pending_deployment='',runtime_placement_json=?,runtime_generation=?,status='running',version=?,manifest_json=?,pending_manifest_json='',sidecar_url_override=?,local_pid=0,local_bin_path=?,local_port=?,status_message='',error_message='',mcp_activation_revision=mcp_activation_revision+1 WHERE id=?`, d.ID, runtimeJSON(d.Placement), d.Generation, d.Manifest.Version, runtimeJSON(d.Manifest), d.Execution.Endpoint, d.LocalBin, d.LocalPort, d.InstallID)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if m.s.installedApps != nil {
		m.s.LoadInstalledApps()
		m.s.RemountStaticApps()
	}
	if err = m.s.registerAppMCPAfterActivation(d.InstallID, appMCPSurfaceSnapshot{}); err != nil {
		return fmt.Errorf("deployment active; MCP registration requires reconciliation")
	}
	return nil
}
func (m *appRuntimeManager) restoreOldRoute(old runtimeDeployment) error {
	if old.ID == "" {
		return nil
	}
	_, err := m.s.store.db.Exec(`UPDATE app_installs SET status='running',sidecar_url_override=?,runtime_active_deployment=?,runtime_pending_deployment='',runtime_placement_json=?,status_message='upgrade failed; previous deployment verified',mcp_activation_revision=mcp_activation_revision+1 WHERE id=?`, old.Execution.Endpoint, old.ID, runtimeJSON(old.Placement), old.InstallID)
	if err == nil && m.s.installedApps != nil {
		m.s.LoadInstalledApps()
	}
	return err
}
func (m *appRuntimeManager) rollback(op *runtimeOperation, old, candidate *runtimeDeployment, cause error) error {
	if err := m.phase(op, "rolling_back"); err != nil {
		return err
	}
	// Upgrade older operation journals conservatively before touching a possible
	// writer. A stopped candidate with a verified restore may have already run.
	if !op.CandidateStarted && op.SnapshotPath != "" && (candidate.Status == "starting" || candidate.Status == "ready" || candidate.Status == "active" || candidate.Status == "stopped" && candidate.Execution.SnapshotSHA256 != "") {
		op.CandidateStarted = true
		if err := m.saveOperation(*op); err != nil {
			return err
		}
	}
	// Unknown start outcomes are inspected/stopped by deployment ID, never
	// assumed absent just because Start returned an error.
	if candidate.Status == "starting" || candidate.Status == "ready" || candidate.Status == "active" {
		if candidate.Execution.State == "running" && candidate.Execution.Ready {
			if err := m.drain(*candidate, *op); err != nil {
				return fmt.Errorf("candidate drain failed; rollback refused: %w", err)
			}
		}
		if err := m.stopDeployment(candidate, *op); err != nil {
			return fmt.Errorf("activation failed; candidate stop unconfirmed and rollback refused")
		}
	}
	if old.ID == "" {
		return cause
	}
	if old.Status == "stopped" && op.CandidateStarted && (op.SnapshotSourceID == "" || op.SnapshotSourceID == old.ID) {
		if candidate.Status != "stopped" || !candidate.Execution.StoppedConfirmed {
			return fmt.Errorf("rollback requires confirmed candidate shutdown")
		}
		if candidate.Manifest.Runtime.DatabaseUpgrade == "requires_restore" || old.Manifest.Runtime.DatabaseUpgrade == "requires_restore" {
			return fmt.Errorf("rollback requires an explicit database restore plan; newer state retained")
		}
		if err := m.captureStateSnapshot(*candidate, op); err != nil {
			return fmt.Errorf("rollback could not preserve latest candidate state: %w", err)
		}
	}
	if op.PreviousStopped && old.Status == "stopped" {
		_, err := m.s.store.db.Exec(`UPDATE app_installs SET status='disabled',runtime_active_deployment=?,runtime_pending_deployment='',runtime_placement_json=? WHERE id=?`, old.ID, runtimeJSON(old.Placement), old.InstallID)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w; previous installation remains stopped with its data retained", cause)
	}

	if old.Status == "stopped" {
		if !old.Placement.local() {
			return m.rollbackRemoteDeployment(op, *old, *candidate, cause)
		}
		if op.SnapshotPath != "" {
			if err := m.restoreSnapshot(old, *op); err != nil {
				return fmt.Errorf("rollback state restoration failed")
			}
		}
		if err := m.startDeployment(old, *op); err != nil {
			return fmt.Errorf("activation failed; previous deployment readiness failed")
		}
		if err := m.activateAndConfirm(*old, *op); err != nil {
			return fmt.Errorf("previous deployment could not resume admission")
		}
	} else {
		if err := m.waitReady(old, *op); err != nil {
			return fmt.Errorf("activation failed; previous deployment is not ready")
		}
	}
	old.Status = "active"
	if err := m.saveDeployment(*old); err != nil {
		return err
	}
	if err := m.restoreOldRoute(*old); err != nil {
		return err
	}
	m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, candidate.ID)
	return fmt.Errorf("%w; previous deployment restored and verified; database changes are not automatically undone", cause)
}
func (m *appRuntimeManager) stopOperation(op *runtimeOperation) error {
	ds, err := m.deployments(op.InstallID)
	if err != nil {
		return err
	}
	if _, err = m.s.store.db.Exec(`UPDATE app_installs SET status='pending',status_message='Stopping managed executions' WHERE id=?`, op.InstallID); err != nil {
		return err
	}
	if m.s.installedApps != nil {
		m.s.installedApps.Remove(op.InstallID)
		m.s.RemountStaticApps()
	}
	for _, d := range ds {
		if d.Status != "active" && d.Status != "ready" && d.Status != "starting" && d.Status != "retiring" {
			continue
		}
		// Recover a lost stop acknowledgement before attempting to drain a dead
		// app endpoint. Only the owning backend can confirm that no process remains.
		alreadyStopped := false
		if backend, e := m.backend(d); e == nil {
			if req, e := m.request(d, *op, "inspect"); e == nil {
				if out, e := backend.Inspect(req); e == nil && out.StoppedConfirmed && out.State == "stopped" {
					alreadyStopped = true
				}
			}
		}
		if !alreadyStopped {
			if err = m.drain(d, *op); err != nil {
				return err
			}
		}
		if err = m.stopDeployment(&d, *op); err != nil {
			return err
		}
	}
	_, err = m.s.store.db.Exec(`UPDATE app_installs SET status='disabled',status_message='Stopped managed deployment' WHERE id=?`, op.InstallID)
	return err
}

func (m *appRuntimeManager) deleteOperation(op *runtimeOperation) error {
	if err := m.stopOperation(op); err != nil {
		return err
	}
	ds, err := m.deployments(op.InstallID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.Status == "deleted" {
			continue
		}
		if d.Status != "stopped" && d.Status != "prepared" && d.Status != "preparing" {
			return fmt.Errorf("deployment resources are not safe to delete")
		}
		b, err := m.backend(d)
		if err != nil {
			return err
		}
		req, _ := m.request(d, *op, "delete")
		if err = b.Delete(req); err != nil {
			return err
		}
		m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, d.ID)
		d.Status = "deleted"
		if err = m.saveDeployment(d); err != nil {
			return err
		}
	}
	if err = m.cleanupIngress(op.InstallID); err != nil {
		return err
	}
	// Deleted executions cannot remain the active/pending predecessor of a future
	// deployment. The placement and backend-owned persistent data are retained.
	_, err = m.s.store.db.Exec(`UPDATE app_installs SET runtime_active_deployment='',runtime_pending_deployment='',sidecar_url_override='',local_pid=0,local_port=0 WHERE id=?`, op.InstallID)
	return err
}
func (m *appRuntimeManager) deployments(id int64) ([]runtimeDeployment, error) {
	rows, err := m.s.store.db.Query(`SELECT id FROM app_runtime_deployments WHERE install_id=? ORDER BY generation,replica`, id)
	if err != nil {
		return nil, err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	var out []runtimeDeployment
	for _, ref := range refs {
		d, e := m.loadDeployment(ref)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, nil
}

func runtimePersistentState(d runtimeDeployment) bool {
	return d.PersistentData || len(d.Manifest.Runtime.Storage) > 0 || (d.Manifest.DB != nil && (d.Manifest.DB.Driver == "" || d.Manifest.DB.Driver == "sqlite"))
}
func (m *appRuntimeManager) retirePrevious(op *runtimeOperation, old runtimeDeployment) error {
	if old.ID == "" {
		return nil
	}
	ds, err := m.deployments(old.InstallID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.Generation != old.Generation || d.Status == "deleted" {
			continue
		}
		if d.Status != "stopped" {
			if err = m.phase(op, "retiring_previous"); err != nil {
				return err
			}
			if err = m.drain(d, *op); err != nil {
				return fmt.Errorf("candidate active; previous drain requires attention")
			}
			if err = m.stopDeployment(&d, *op); err != nil {
				return fmt.Errorf("candidate active; previous stop requires attention")
			}
		}
		m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, d.ID)
	}
	return nil
}

func runtimeMergeExecution(previous, next runtimeExecution) (runtimeExecution, error) {
	if next.Handle != "" && previous.Handle != "" && next.Handle != previous.Handle {
		return next, fmt.Errorf("execution handle changed outside preparation")
	}
	if next.HostKey != "" && previous.HostKey != "" && next.HostKey != previous.HostKey {
		return next, fmt.Errorf("execution host changed outside placement")
	}
	if next.ReleaseDigest != "" && previous.ReleaseDigest != "" && next.ReleaseDigest != previous.ReleaseDigest {
		return next, fmt.Errorf("running release differs from prepared digest")
	}
	if next.Handle == "" {
		next.Handle = previous.Handle
	}
	if next.HostKey == "" {
		next.HostKey = previous.HostKey
	}
	if next.ReleaseDigest == "" {
		next.ReleaseDigest = previous.ReleaseDigest
	}
	if next.Platform == "" {
		next.Platform = previous.Platform
	}
	if next.StateRef == "" {
		next.StateRef = previous.StateRef
	}
	if next.SnapshotSHA256 == "" {
		next.SnapshotSHA256 = previous.SnapshotSHA256
	}
	if next.Ports == nil {
		next.Ports = previous.Ports
	}
	return next, nil
}
