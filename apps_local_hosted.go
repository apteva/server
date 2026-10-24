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
	return sup.trackHosted(spec, status.PID)
}
func (sup *LocalSupervisor) trackHosted(spec activationSpec, pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	p := &localProc{cmd: &exec.Cmd{Process: process}, port: spec.httpPort, done: make(chan struct{}), spec: spec.clone()}
	sup.mu.Lock()
	if sup.stopping {
		sup.mu.Unlock()
		return fmt.Errorf("local supervisor is stopping")
	}
	previous := sup.procs[spec.installID]
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
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		defer close(p.done)
		identitySeen := false
		for range ticker.C {
			matches := runtimeProcessMatches(pid, spec.binPath)
			if matches {
				identitySeen = true
			}
			if !processAlive(pid) || (identitySeen && !matches) {
				sup.mu.Lock()
				if sup.procs[spec.installID] == p {
					delete(sup.procs, spec.installID)
				}
				sup.mu.Unlock()
				return
			}
		}
	}()
	log.Printf("[APPS-LOCAL] stable host owns install=%d pid=%d port=%d", spec.installID, pid, spec.httpPort)
	return nil
}
