package main

import (
	"io"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLocalSupervisorStopAllKillsRunningAndPendingSidecars(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	running := startStubbornLocalProc(t)
	pending := startStubbornLocalProc(t)

	sup.mu.Lock()
	sup.procs[1] = running
	sup.mu.Unlock()
	sup.pendingMu.Lock()
	sup.pending[1] = pending
	sup.pendingMu.Unlock()

	sup.StopAll(100 * time.Millisecond)

	waitLocalProcDone(t, running)
	waitLocalProcDone(t, pending)

	sup.mu.Lock()
	procCount := len(sup.procs)
	sup.mu.Unlock()
	sup.pendingMu.Lock()
	pendingCount := len(sup.pending)
	sup.pendingMu.Unlock()
	if procCount != 0 || pendingCount != 0 {
		t.Fatalf("StopAll left tracked sidecars: procs=%d pending=%d", procCount, pendingCount)
	}
}

func startStubbornLocalProc(t *testing.T) *localProc {
	t.Helper()
	cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 60; done")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stubborn proc: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	p := &localProc{cmd: cmd, done: done}
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			signalLocalProc(cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})
	return p
}

func waitLocalProcDone(t *testing.T, p *localProc) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("process pid=%d did not exit", p.cmd.Process.Pid)
	}
}
