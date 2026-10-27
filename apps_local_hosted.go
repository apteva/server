package main

import (
	"context"
	"fmt"
	"github.com/apteva/server/internal/host"
	"log"
	"os"
	"os/exec"
	"time"
)

// The stable host owns new sidecar processes and their rotating log streams.
// A management-slot exit therefore cannot close a sidecar's stdout pipe.
func (sup *LocalSupervisor) spawnHosted(spec activationSpec, cmd *exec.Cmd, logPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := host.UnixClient(os.Getenv("APTEVA_HOST_SOCKET"))
	defer client.CloseIdleConnections()
	var status host.ProcessStatus
	request := host.ProcessRequest{ID: generateToken(16), Args: cmd.Args, Env: cmd.Env, Directory: cmd.Dir, LogPath: logPath}
	var startErr error
	for attempt := 0; attempt < 3; attempt++ {
		startErr = host.Call(ctx, client, "POST", "/process/start", request, &status)
		if startErr == nil || ctx.Err() != nil {
			break
		}
		// A lost acknowledgement must reuse the same host-owned start identity.
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	if startErr != nil {
		return fmt.Errorf("stable host spawn: %w", startErr)
	}
	if status.PID < 1 || !status.Running {
		return fmt.Errorf("stable host did not publish a running process")
	}
	// Managed launches first write their PID through a shell and then exec the
	// prepared binary. Allow only that bounded startup transition, never treat
	// an arbitrary executable at the returned PID as an adopted app.
	deadline := time.Now().Add(2 * time.Second)
	for !runtimeProcessMatches(status.PID, spec.binPath) {
		if !processAlive(status.PID) || time.Now().After(deadline) {
			return fmt.Errorf("stable host process executable could not be verified")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sup.trackHosted(spec, status.PID)
}
func (sup *LocalSupervisor) trackHosted(spec activationSpec, pid int) error {
	identity, err := readRuntimeProcessIdentity(pid)
	if err != nil || !identity.matchesBinary(spec.binPath) {
		return fmt.Errorf("cannot verify hosted process executable/lifetime")
	}
	watch, watchErr := watchRuntimeProcessExit(pid)
	// Confirm the subscription and identity refer to the same process lifetime.
	after, err := readRuntimeProcessIdentity(pid)
	if err != nil || !identity.sameProcess(after) {
		if watch != nil {
			_ = watch.close()
		}
		return fmt.Errorf("hosted process changed before exit subscription")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		if watch != nil {
			_ = watch.close()
		}
		return err
	}
	cleanup := func() {
		_ = process.Release()
		if watch != nil {
			_ = watch.close()
		}
	}
	p := &localProc{cmd: &exec.Cmd{Process: process}, port: spec.httpPort, done: make(chan struct{}), spec: spec.clone(), identity: &identity}
	sup.mu.Lock()
	if sup.stopping {
		sup.mu.Unlock()
		cleanup()
		return fmt.Errorf("local supervisor is stopping")
	}
	previous := sup.procs[spec.installID]
	if previous != nil && previous.identity != nil && previous.identity.sameProcess(identity) {
		sup.mu.Unlock()
		cleanup()
		return nil
	}
	sup.procs[spec.installID] = p
	sup.mu.Unlock()
	if previous != nil {
		select {
		case <-previous.done:
		default:
			sup.pendingMu.Lock()
			sup.pending[spec.installID] = previous
			sup.pendingMu.Unlock()
		}
	}
	if watchErr != nil {
		log.Printf("[APPS-LOCAL] native exit notification unavailable install=%d; using 1s kernel identity checks: %v", spec.installID, watchErr)
	}
	go func() {
		// Keep Process.Pid immutable while captured handles can still be used
		// by Stop/Inspect. Go releases its internal pidfd when the handle is
		// garbage collected; our own exit descriptor is explicitly closed.
		defer close(p.done)
		waitHostedProcessExit(identity, watch)
		sup.mu.Lock()
		if sup.procs[spec.installID] == p {
			p.stoppedAt = time.Now()
			delete(sup.procs, spec.installID)
		}
		sup.mu.Unlock()
	}()
	log.Printf("[APPS-LOCAL] stable host owns install=%d pid=%d port=%d", spec.installID, pid, spec.httpPort)
	return nil
}

func waitHostedProcessExit(identity runtimeProcessIdentity, watch *runtimeProcessExitWatch) {
	defer func() {
		if watch != nil {
			_ = watch.close()
		}
	}()
	for {
		if watch != nil {
			// Exit notifications wake immediately. The slow audit also catches an
			// unexpected exec without periodically spawning helper processes.
			exited, err := watch.wait(30 * time.Second)
			if exited {
				return
			}
			if err != nil {
				_ = watch.close()
				watch = nil
				log.Printf("[APPS-LOCAL] process exit subscription failed pid=%d; using kernel identity checks: %v", identity.pid, err)
			}
		} else {
			// Older kernels/containers may not permit pidfds. This bounded
			// fallback still never executes ps or any other helper process.
			time.Sleep(time.Second)
		}
		current, err := readRuntimeProcessIdentity(identity.pid)
		if runtimeProcessGone(err) || err == nil && !identity.sameProcess(current) {
			return
		}
		// Permission/transient errors are unknown, not proof of termination.
	}
}
