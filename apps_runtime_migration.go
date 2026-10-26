package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/apteva/app-sdk"
)

type runtimeCompatibilityCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func runtimeCheck(name, code string, err error) runtimeCompatibilityCheck {
	c := runtimeCompatibilityCheck{Name: name, Code: code, Status: "passed"}
	if err != nil {
		c.Status = "blocked"
		c.Message = err.Error()
	}
	return c
}
func firstRuntimeBlock(checks []runtimeCompatibilityCheck) error {
	for _, c := range checks {
		if c.Status != "passed" {
			return fmt.Errorf("%s: %s", c.Name, c.Message)
		}
	}
	return nil
}

type runtimeExistingSource struct {
	Deployment  runtimeDeployment
	Exists      bool
	Legacy      bool
	Environment map[string]string
}

// Reading legacy provenance never adopts the process, writes a deployment or
// changes installation routing. ensureExisting reuses it only for an operation.
func (m *appRuntimeManager) existingSource(id int64) (runtimeExistingSource, error) {
	var out runtimeExistingSource
	var managed int
	var status, raw, endpoint, bin string
	var port, pid int
	var generation int64
	err := m.s.store.db.QueryRow(`SELECT runtime_managed,status,COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),local_bin_path,local_port,local_pid,sidecar_url_override,runtime_generation FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).Scan(&managed, &status, &raw, &bin, &port, &pid, &endpoint, &generation)
	if err != nil {
		return out, err
	}
	if managed != 0 {
		d, err := m.active(id)
		if err == sql.ErrNoRows {
			return out, nil
		}
		if err == nil && d.Placement.local() && m.s.localApps != nil {
			present, e := runtimeStatePresent(m.localStateDir(d))
			if e != nil {
				return out, e
			}
			d.PersistentData = d.PersistentData || present
		}
		out.Deployment = d
		out.Exists = err == nil
		return out, err
	}
	if status != "running" && status != "pending" && status != "disabled" {
		return out, fmt.Errorf("installation is not ready for managed placement")
	}
	var manifest sdk.Manifest
	if err = json.Unmarshal([]byte(raw), &manifest); err != nil {
		return out, err
	}
	d := runtimeDeployment{InstallID: id, Generation: generation, Manifest: manifest, Status: "stopped", Placement: runtimePlacement{Type: "local"}, LocalBin: bin, LocalPort: port, LocalPID: pid, Execution: runtimeExecution{State: "stopped", StoppedConfirmed: true, HostKey: "server:local", Endpoint: endpoint, StateRef: fmt.Sprintf("local:%d", id)}}
	if manifest.Runtime.Package != nil && manifest.Runtime.Package.AppCapabilities != nil {
		d.AppCaps = runtimeAppCaps(*manifest.Runtime.Package.AppCapabilities)
	}
	if m.s.localApps == nil {
		return out, fmt.Errorf("local supervisor is unavailable for source inspection")
	}
	present, err := runtimeStatePresent(m.localStateDir(d))
	if err != nil {
		return out, err
	}
	d.PersistentData = present
	out.Legacy = true
	out.Environment = map[string]string{}
	if status == "running" {
		if manifest.Runtime.Kind == "static" {
			d.LocalBin = trimStaticRuntimeEndpoint(endpoint)
			if d.LocalBin == "" {
				return out, fmt.Errorf("unmanaged remote static execution has no owner")
			}
		} else {
			proc := m.s.localApps.currentProc(id)
			if bin == "" || proc == nil || proc.spec.binPath != bin || proc.port != port {
				return out, fmt.Errorf("cannot adopt an unmanaged external process; register its owning backend first")
			}
			for k, v := range proc.spec.env {
				out.Environment[k] = v
			}
		}
		d.Status = "active"
		d.Execution.State = "running"
		d.Execution.Ready = true
		d.Execution.StoppedConfirmed = false
		d.AppCaps, d.AppCapsKnown = m.appCapabilities(id)
		out.Exists = true
	} else {
		out.Exists = d.PersistentData || len(manifest.Runtime.Storage) > 0
	}
	out.Deployment = d
	return out, nil
}
func trimStaticRuntimeEndpoint(endpoint string) string {
	const prefix = "static://"
	if len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix {
		return endpoint[len(prefix):]
	}
	return ""
}
func declaredRuntimeCapabilities(manifest sdk.Manifest, p runtimePlacement, old runtimeDeployment) (runtimeAppCaps, bool) {
	if manifest.Runtime.Package != nil && manifest.Runtime.Package.AppCapabilities != nil {
		return runtimeAppCaps(*manifest.Runtime.Package.AppCapabilities), true
	}
	if p.Package != nil && p.Package.AppCapabilities != nil {
		return *p.Package.AppCapabilities, true
	}
	if runtimeCapabilitiesKnown(old) && runtimeFingerprint(manifest) == runtimeFingerprint(old.Manifest) {
		return old.AppCaps, true
	}
	return runtimeAppCaps{}, false
}
func (m *appRuntimeManager) placementCapabilities(p runtimePlacement) (runtimeBackendCaps, error) {
	if p.local() {
		return (&localRuntimeBackend{m: m}).Describe(), nil
	}
	reg, err := m.registration(p.Backend)
	return reg.Caps, err
}

type runtimeMigrationCheckOptions struct {
	Policy *runtimeStateTransferPolicy
	Target *sdk.RuntimeTargetInfo
}

// These checks are used before preparation and again before draining. Backend
// apps supply execution facts; the Server retains migration sequencing/rollback.
func (m *appRuntimeManager) migrationChecks(ctx context.Context, old runtimeDeployment, manifest sdk.Manifest, p runtimePlacement, candidateCaps *runtimeAppCaps, force bool, options ...runtimeMigrationCheckOptions) []runtimeCompatibilityCheck {
	if !force && sameRuntimeTarget(old, runtimeDeployment{Placement: p}) {
		return nil
	}
	persistent := runtimePersistentState(old)
	var bytes *int64
	var entries *int64
	var sourceFree *int64
	var stateErr error
	if old.Placement.local() {
		if m.s.localApps == nil {
			stateErr = fmt.Errorf("local source state cannot be inspected")
		} else {
			stats, err := inspectRuntimeStateContext(ctx, m.localStateDir(old), inspectionPolicy())
			stateErr = err
			if err == nil {
				bytes = &stats.Bytes
				n := int64(stats.Entries)
				entries = &n
				if free, e := runtimeStateDiskFree(m.localStateDir(old)); e == nil {
					sourceFree = &free
				}
				persistent = persistent || stats.Entries > 0
			}
		}
	}
	if !persistent && stateErr == nil {
		return nil
	}
	checks := []runtimeCompatibilityCheck{}
	if old.Placement.local() {
		checks = append(checks, runtimeCheck("Source recovery", "source_binary_unavailable", localRuntimeRollbackBinary(old)))
	}
	sourceCaps, sourceErr := m.placementCapabilities(old.Placement)
	destinationCaps, destinationErr := m.placementCapabilities(p)
	sourceSnapshot := sourceErr
	if sourceSnapshot == nil && !sourceCaps.Snapshots {
		sourceSnapshot = fmt.Errorf("source backend does not support state snapshots")
	}
	destinationSnapshot := destinationErr
	if destinationSnapshot == nil && !destinationCaps.Snapshots {
		destinationSnapshot = fmt.Errorf("destination backend does not support state snapshots; existing installation data cannot be moved")
	}
	checks = append(checks, runtimeCheck("Source state transfer", "source_snapshots_required", sourceSnapshot), runtimeCheck("Destination state transfer", "destination_snapshots_required", destinationSnapshot))
	if old.Placement.local() && len(old.Manifest.Runtime.Storage) > 0 || p.local() && len(manifest.Runtime.Storage) > 0 {
		checks = append(checks, runtimeCheck("Storage mapping", "storage_mapping_unsupported", fmt.Errorf("local persistent mounts require an explicit storage transfer backend")))
	}
	if !old.Placement.local() && sourceSnapshot == nil {
		b, err := m.backend(old)
		if err == nil {
			req, e := m.request(old, runtimeOperation{ID: "migration-preflight"}, "inspect")
			err = e
			if err == nil {
				var observed runtimeExecution
				remote := b.(*remoteRuntimeBackend)
				observed, err = remote.inspectContext(ctx, req)
				if err == nil {
					_, err = runtimeMergeExecution(old.Execution, observed)
					bytes = observed.StateBytes
					entries = observed.StateEntries
					sourceFree = observed.StateAvailableBytes
				}
			}
		}
		if err != nil {
			stateErr = fmt.Errorf("source state metadata could not be verified")
		}
	}
	if stateErr == nil && bytes == nil && sourceSnapshot != nil {
		stateErr = fmt.Errorf("state size cannot be verified without source snapshot support")
	}
	if stateErr == nil && bytes == nil {
		stateErr = fmt.Errorf("source backend must report uncompressed state_bytes before migration; transfer size is unknown")
	}
	var opts runtimeMigrationCheckOptions
	if len(options) > 0 {
		opts = options[0]
	}
	policy, policyErr := m.negotiateStatePolicy(old, p, opts.Policy)
	checks = append(checks, runtimeCheck("State transfer policy", "state_transfer_policy_incompatible", policyErr))
	if stateErr == nil && entries == nil {
		stateErr = fmt.Errorf("source backend must report state_entries before migration")
	}
	if stateErr == nil && policyErr == nil && (*bytes < 0 || *bytes > policy.MaxExpandedBytes || *entries < 0 || *entries > policy.MaxEntries) {
		stateErr = fmt.Errorf("persistent state exceeds negotiated limits: %d bytes / %d entries; allowed %d bytes / %d entries", *bytes, *entries, policy.MaxExpandedBytes, policy.MaxEntries)
	}
	checks = append(checks, runtimeCheck("State size", "state_size_unverified", stateErr))
	if stateErr == nil && policyErr == nil {
		checks[len(checks)-1].Message = fmt.Sprintf("%d uncompressed bytes, %d entries; allowed %d bytes, %d entries (rechecked after shutdown)", *bytes, *entries, policy.MaxExpandedBytes, policy.MaxEntries)
		diskErr := m.migrationDiskCheck(ctx, old, p, runtimeStateSize{Bytes: *bytes, Entries: int(*entries)}, sourceFree, opts.Target)
		checks = append(checks, runtimeCheck("State transfer disk space", "state_disk_insufficient", diskErr))
	}
	oldLifecycle := error(nil)
	if old.Status != "stopped" && (!old.AppCaps.PassiveStart || !old.AppCaps.Drain) {
		oldLifecycle = fmt.Errorf("source app must support drain and passive-start for consistent transfer and recovery")
	}
	checks = append(checks, runtimeCheck("Source lifecycle", "source_lifecycle_required", oldLifecycle))
	next, known := declaredRuntimeCapabilities(manifest, p, old)
	if candidateCaps != nil {
		next = *candidateCaps
		known = true
	}
	nextLifecycle := error(nil)
	if !known {
		nextLifecycle = fmt.Errorf("declare lifecycle capabilities for the exact destination release in runtime.package")
	} else if !next.PassiveStart || !next.Drain {
		nextLifecycle = fmt.Errorf("destination app must support passive-start and drain")
	}
	checks = append(checks, runtimeCheck("Destination lifecycle", "destination_lifecycle_required", nextLifecycle))
	return checks
}
func (m *appRuntimeManager) checkInstallationMigration(ctx context.Context, id int64, manifest sdk.Manifest, p runtimePlacement) error {
	source, err := m.existingSource(id)
	if err != nil {
		return err
	}
	if !source.Exists {
		return nil
	}
	return firstRuntimeBlock(m.migrationChecks(ctx, source.Deployment, manifest, p, nil, false))
}

// Kept separate from package-building: checking local source ownership and
// data must never fetch/build a release or prepare a destination.
func localRuntimeRollbackBinary(d runtimeDeployment) error {
	if d.LocalBin == "" {
		return fmt.Errorf("the previous local binary is unavailable for verified rollback")
	}
	info, err := os.Stat(d.LocalBin)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("the previous local binary is unavailable for verified rollback")
	}
	return nil
}
