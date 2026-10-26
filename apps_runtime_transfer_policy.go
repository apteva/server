package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Legacy peers never implicitly gain permission to handle larger state.
const runtimeSnapshotLimit int64 = 512 << 20

func legacyRuntimeStatePolicy() runtimeStateTransferPolicy {
	return runtimeStateTransferPolicy{Format: "tar.gz", MaxArchiveBytes: runtimeSnapshotLimit, MaxExpandedBytes: runtimeSnapshotLimit, MaxEntries: 100000, TimeoutSeconds: 1200}
}
func serverRuntimeStatePolicy() (runtimeStateTransferPolicy, error) {
	p := runtimeStateTransferPolicy{Format: "tar.gz", MaxArchiveBytes: 8 << 30, MaxExpandedBytes: 8 << 30, MaxEntries: 1000000, TimeoutSeconds: 7200}
	for _, item := range []struct {
		key   string
		value *int64
	}{
		{"APTEVA_RUNTIME_STATE_MAX_ARCHIVE_BYTES", &p.MaxArchiveBytes},
		{"APTEVA_RUNTIME_STATE_MAX_EXPANDED_BYTES", &p.MaxExpandedBytes},
		{"APTEVA_RUNTIME_STATE_MAX_ENTRIES", &p.MaxEntries},
		{"APTEVA_RUNTIME_STATE_TIMEOUT_SECONDS", &p.TimeoutSeconds},
	} {
		if raw := os.Getenv(item.key); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return p, fmt.Errorf("invalid %s", item.key)
			}
			*item.value = n
		}
	}
	return p, validateRuntimeStatePolicy(p)
}
func validateRuntimeStatePolicy(p runtimeStateTransferPolicy) error {
	if p.Format != "tar.gz" || p.MaxArchiveBytes <= 0 || p.MaxArchiveBytes > 1<<40 || p.MaxExpandedBytes <= 0 || p.MaxExpandedBytes > 1<<40 || p.MaxEntries <= 0 || p.MaxEntries > 10000000 || p.TimeoutSeconds <= 0 || p.TimeoutSeconds > 86400 {
		return fmt.Errorf("invalid state transfer policy (tar.gz, positive limits, at most 1 TiB, 10000000 entries and 24 hours)")
	}
	return nil
}
func intersectRuntimeStatePolicy(a, b runtimeStateTransferPolicy) (runtimeStateTransferPolicy, error) {
	if err := validateRuntimeStatePolicy(a); err != nil {
		return a, err
	}
	if err := validateRuntimeStatePolicy(b); err != nil {
		return a, err
	}
	a.MaxArchiveBytes = min(a.MaxArchiveBytes, b.MaxArchiveBytes)
	a.MaxExpandedBytes = min(a.MaxExpandedBytes, b.MaxExpandedBytes)
	a.MaxEntries = min(a.MaxEntries, b.MaxEntries)
	a.TimeoutSeconds = min(a.TimeoutSeconds, b.TimeoutSeconds)
	return a, nil
}
func runtimeBackendStatePolicy(c runtimeBackendCaps) runtimeStateTransferPolicy {
	if c.StateTransfer != nil {
		return *c.StateTransfer
	}
	return legacyRuntimeStatePolicy()
}
func (m *appRuntimeManager) negotiateStatePolicy(old runtimeDeployment, p runtimePlacement, pinned *runtimeStateTransferPolicy) (runtimeStateTransferPolicy, error) {
	policy, err := serverRuntimeStatePolicy()
	if err != nil {
		return policy, err
	}
	for _, placement := range []runtimePlacement{old.Placement, p} {
		caps, err := m.placementCapabilities(placement)
		if err != nil {
			return policy, err
		}
		if !caps.Snapshots {
			return policy, fmt.Errorf("backend does not support state snapshots")
		}
		policy, err = intersectRuntimeStatePolicy(policy, runtimeBackendStatePolicy(caps))
		if err != nil {
			return policy, err
		}
	}
	if pinned != nil {
		effective, err := intersectRuntimeStatePolicy(policy, *pinned)
		if err != nil {
			return policy, err
		}
		if effective != *pinned {
			return policy, fmt.Errorf("backend or Server no longer supports the persisted state transfer policy")
		}
		return *pinned, nil
	}
	return policy, nil
}
func (m *appRuntimeManager) pinStatePolicy(old, d runtimeDeployment, op *runtimeOperation) error {
	p, err := m.negotiateStatePolicy(old, d.Placement, op.StateTransfer)
	if err != nil {
		return err
	}
	if op.StateTransfer == nil {
		// A previously journaled snapshot was created under the legacy limits.
		if op.SnapshotPath != "" {
			p, err = intersectRuntimeStatePolicy(p, legacyRuntimeStatePolicy())
			if err != nil {
				return err
			}
		}
		op.StateTransfer = &p
		return m.saveOperation(*op)
	}
	return nil
}
func operationRuntimeStatePolicy(op runtimeOperation) (runtimeStateTransferPolicy, error) {
	if op.StateTransfer != nil {
		return *op.StateTransfer, validateRuntimeStatePolicy(*op.StateTransfer)
	}
	return legacyRuntimeStatePolicy(), nil
}

// Local metadata inspection and snapshot streaming share cancellation and
// ownership checks. Rollback ignores user cancellation so recovery can finish.
func (m *appRuntimeManager) stateTransferContext(op runtimeOperation, p runtimeStateTransferPolicy) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(m.ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.checkOwner(op); err != nil {
					cancel()
					return
				}
				if !runtimeRollingBack(op) {
					var n int
					if err := m.s.store.db.QueryRowContext(ctx, `SELECT cancel_requested FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&n); err != nil || n != 0 {
						cancel()
						return
					}
				}
			}
		}
	}()
	return ctx, cancel
}
func runtimeExistingFilesystemPath(path string) (string, error) {
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("state filesystem unavailable")
		}
		path = parent
	}
}
func runtimeStateDiskFree(path string) (int64, error) {
	existing, err := runtimeExistingFilesystemPath(path)
	if err != nil {
		return 0, err
	}
	return runtimeFilesystemFree(existing)
}

// Allow for tar headers, gzip overhead, filesystem allocation and staging.
// Existing data remains in place until atomic replacement; do not count it as free.
func runtimeStateDiskBudget(size runtimeStateSize) (archive, expanded int64) {
	archive = size.Bytes + size.Bytes/1000 + int64(size.Entries)*8192 + 1<<20
	expanded = size.Bytes + int64(size.Entries)*4096
	return
}
func runtimeRequireDisk(available *int64, required int64, where string) error {
	if available == nil || *available < 0 {
		return fmt.Errorf("%s state filesystem free space is unknown; backend must report state_available_bytes", where)
	}
	const reserve = 64 << 20
	if *available < required+reserve {
		return fmt.Errorf("%s needs %d additional bytes plus %d bytes reserve; %d bytes available", where, required, reserve, *available)
	}
	return nil
}

func inspectionPolicy() runtimeStateTransferPolicy {
	// Read-only inspection may measure more than a peer can transfer so the
	// operator sees the actual size and the negotiated blocker.
	return runtimeStateTransferPolicy{Format: "tar.gz", MaxArchiveBytes: 1 << 40, MaxExpandedBytes: 1 << 40, MaxEntries: 10000000, TimeoutSeconds: 86400}
}
func (m *appRuntimeManager) migrationDiskCheck(ctx context.Context, old runtimeDeployment, p runtimePlacement, size runtimeStateSize, sourceFree *int64, target *sdk.RuntimeTargetInfo) error {
	archive, expanded := runtimeStateDiskBudget(size)
	if m.s.localApps == nil {
		return fmt.Errorf("Server snapshot cache is unavailable")
	}
	cacheFree, err := runtimeStateDiskFree(m.s.localApps.cacheDir)
	if err != nil {
		return fmt.Errorf("Server snapshot cache free space could not be inspected")
	}
	// Local extraction and cached archives can share a filesystem. Conservatively
	// budget both there, including the restoration required for local rollback.
	cacheNeed := archive
	if old.Placement.local() || p.local() {
		cacheNeed += expanded
	}
	if err = runtimeRequireDisk(&cacheFree, cacheNeed, "Server snapshot cache"); err != nil {
		return err
	}
	if err = runtimeRequireDisk(sourceFree, expanded+archive, "Source/rollback"); err != nil {
		return err
	}
	var destinationFree *int64
	if p.local() {
		free, e := runtimeStateDiskFree(m.localStateDir(old))
		if e != nil {
			return fmt.Errorf("destination state filesystem free space could not be inspected")
		}
		destinationFree = &free
	} else {
		if target != nil {
			return runtimeRequireDisk(target.StateAvailableBytes, expanded+archive, "Destination")
		}
		var project string
		if err = m.s.store.db.QueryRowContext(ctx, `SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, old.InstallID).Scan(&project); err != nil {
			return err
		}
		info, e := m.inspectTarget(ctx, p, project)
		if e != nil {
			return fmt.Errorf("destination state capacity could not be verified: %w", e)
		}
		destinationFree = info.StateAvailableBytes
	}
	return runtimeRequireDisk(destinationFree, expanded+archive, "Destination")
}
