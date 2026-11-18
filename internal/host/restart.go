package host

import (
	"context"
	"fmt"
	"time"
)

// restartMigration deploys a release that changes stored schema. The two
// versions cannot share the database, so this is a service restart onto the
// new version, run by the host: drain, stop the current server cleanly,
// record the new binary as committed, then boot it fresh so it applies its
// migrations. If the host itself stops part-way, its next start boots the new
// binary again and the migration continues.
//
// The previous binary is booted again only when the new server failed before
// it was activated, because migrations run after activation. An older binary
// is never booted against a database the new one may have migrated.
func (h *Host) restartMigration(c Candidate) {
	h.mu.Lock()
	old := h.active
	h.mu.Unlock()
	previous := Candidate{Binary: old.candidate.Binary, Version: old.candidate.Version}
	policyCtx, cancelPolicy := context.WithTimeout(h.ctx, 10*time.Second)
	policy := h.agentPolicy(policyCtx, c, old)
	cancelPolicy()
	c.ExistingSchema, c.SchemaHash, c.SecretHash = false, "", ""

	h.router.Pause()
	_ = h.phase("draining", "Waiting for active requests; this release migrates the database, so the server restarts")
	drainCtx, stopDrain := context.WithTimeout(h.ctx, h.cfg.DrainTimeout)
	err := h.router.Drain(drainCtx, old.backend)
	stopDrain()
	if err == nil {
		err = h.ctx.Err()
	}
	if err != nil {
		h.router.Resume()
		_ = h.phase("failed", "Existing work did not finish; the active version was retained")
		return
	}

	_ = h.phase("stopping", "Stopping the current server")
	h.stopSlot(old, false)
	h.stopProcesses()
	// From here on, any start of this host boots the new binary fresh, exactly
	// like a stopped service whose next start is the new version.
	if err = h.commitBinary(c); err != nil {
		h.restartPrevious(previous, fmt.Errorf("cannot record the new version: %w", err))
		return
	}

	_ = h.phase("migrating", "Starting the new server; it applies database migrations as it boots")
	candidate, activated, err := h.bootSlot(c)
	if err != nil && !activated {
		h.restartPrevious(previous, err)
		return
	}
	if err != nil {
		_ = h.phase("recovery_required", fmt.Sprintf("The new server failed after it began migrating the database: %v. The previous version was not started because it may not run on a migrated database; the next host start boots the new version again", err))
		return
	}
	if err = h.commitServing(candidate); err != nil {
		_ = h.phase("recovery_required", "New server is serving but the committed state could not be saved: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.StartupTimeout)
	defer cancel()
	_ = h.phase("succeeded", h.rollout(ctx, candidate, policy, "New server is serving after a restart migration; the database was migrated in place"))
}

// restartPrevious boots the previous binary after the new one failed before
// activation, so the database is as the previous server left it.
func (h *Host) restartPrevious(previous Candidate, cause error) {
	_ = h.phase("rolling_back", "The new server did not start; starting the previous version")
	h.stopProcesses()
	err := h.commitBinary(previous)
	var s *slot
	if err == nil {
		s, _, err = h.bootSlot(previous)
	}
	if err == nil {
		err = h.commitServing(s)
	}
	if err != nil {
		_ = h.phase("recovery_required", fmt.Sprintf("new server failed: %v; previous version failed to start: %v", cause, err))
		return
	}
	_ = h.phase("rolled_back", fmt.Sprintf("Previous version restarted; the new server failed before it migrated the database: %v", cause))
}

// commitBinary records c as the binary every later start boots, with no slot
// to adopt, as after a clean stop.
func (h *Host) commitBinary(c Candidate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Binary = c.Binary
	h.state.Version = c.Version
	h.state.CleanStop = true
	h.state.ActiveSlot = nil
	h.state.PendingSlot = nil
	return h.saveLocked()
}

// bootSlot starts c as a fresh server and waits until it is healthy. The
// router stays paused. activated reports whether the server was released
// past its standby gate, after which it may have run migrations.
func (h *Host) bootSlot(c Candidate) (s *slot, activated bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*h.cfg.StartupTimeout)
	defer cancel()
	if s, err = h.startSlot(ctx, c); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	h.state.PendingSlot = recordSlot(s)
	err = h.saveLocked()
	h.mu.Unlock()
	if err == nil {
		activated = true
		err = h.activate(ctx, s)
	}
	if err != nil {
		h.stopSlot(s, false)
		return nil, activated, err
	}
	return s, true, nil
}

// commitServing makes s the active server and opens the router to it.
func (h *Host) commitServing(s *slot) error {
	h.mu.Lock()
	h.active = s
	h.state.ActiveSlot = recordSlot(s)
	h.state.PendingSlot = nil
	h.state.CleanStop = false
	err := h.saveLocked()
	h.mu.Unlock()
	h.router.Publish(s.backend)
	return err
}
