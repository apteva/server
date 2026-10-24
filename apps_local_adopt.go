package main

import (
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"time"
)

// Adopt tracks an already running sidecar after an exclusive hosted handoff.
// Never infer ownership from a PID alone and never kill a live process on a
// failed identity/health check. Its per-install data directory is unchanged.
func (sup *LocalSupervisor) Adopt(id int64, pid, port int, bin string, m *sdk.Manifest) error {
	if pid < 1 || port < 1 || !runtimeProcessMatches(pid, bin) {
		return fmt.Errorf("cannot verify existing sidecar process identity")
	}
	spec, err := newActivationSpec(id, m, bin, port, map[string]string{})
	if err != nil {
		return err
	}
	if err = sup.waitHealthy(id, port, spec.healthPath, 10*time.Second); err != nil {
		return err
	}
	return sup.trackHosted(spec, pid)
}
