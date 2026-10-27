//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHostedTrackingFourIdleAppsCPU(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	procs := make([]*localProc, 0, 4)
	for n := 0; n < 4; n++ {
		cmd, bin, _ := processTestChild(t)
		spec := activationSpec{installID: int64(800 + n), binPath: bin, httpPort: 32200 + n}
		if err := sup.trackHosted(spec, cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, sup.currentProc(spec.installID))
	}
	time.Sleep(50 * time.Millisecond)
	var before, after syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	start := time.Now()
	time.Sleep(time.Second)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	cpu := float64(after.Utime.Sec+after.Stime.Sec-before.Utime.Sec-before.Stime.Sec) +
		float64(after.Utime.Usec+after.Stime.Usec-before.Utime.Usec-before.Stime.Usec)/1e6
	t.Logf("four idle hosted apps: supervisor CPU %.3f ms over %s (%.3f%% of one core)", cpu*1000, time.Since(start), 100*cpu/time.Since(start).Seconds())
	sup.StopAll(time.Second)
	for _, p := range procs {
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Fatal("StopAll did not observe hosted process termination")
		}
	}
}

func TestHostedTrackingNoProcessHelpersAndControllerReattach(t *testing.T) {
	cmd, bin, _ := processTestChild(t)
	path := t.TempDir()
	marker := filepath.Join(path, "ps-invoked")
	if err := os.WriteFile(filepath.Join(path, "ps"), []byte("#!/bin/sh\nprintf x >> '"+marker+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path)
	for i := 0; i < 100; i++ {
		if !runtimeProcessMatches(cmd.Process.Pid, bin) {
			t.Fatal("kernel identity check failed")
		}
	}
	first, second := NewLocalSupervisor(t.TempDir()), NewLocalSupervisor(t.TempDir())
	spec := activationSpec{installID: 701, binPath: bin, httpPort: 32123}
	for _, sup := range []*LocalSupervisor{first, second} {
		if err := sup.trackHosted(spec, cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
	}
	p1, p2 := first.currentProc(701), second.currentProc(701)
	if err := second.trackHosted(spec, cmd.Process.Pid); err != nil || second.currentProc(701) != p2 {
		t.Fatal("duplicate reattachment created another tracker")
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("supervision spawned ps")
	}
	if err := second.Stop(701); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*localProc{p1, p2} {
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Fatal("reattached controller did not observe exit")
		}
	}
	if first.currentProc(701) != nil || second.currentProc(701) != nil {
		t.Fatal("dead hosted app retained in supervisor")
	}
}

func TestHostedTrackingRejectsForeignProcessAndPreservesReplacement(t *testing.T) {
	old, bin, _ := processTestChild(t)
	newer, _, _ := processTestChild(t)
	sup := NewLocalSupervisor(t.TempDir())
	spec := activationSpec{installID: 702, binPath: "/bin/sh", httpPort: 32124}
	if err := sup.trackHosted(spec, old.Process.Pid); err == nil || sup.currentProc(702) != nil {
		t.Fatal("foreign process accepted")
	}
	spec.binPath = bin
	if err := sup.trackHosted(spec, old.Process.Pid); err != nil {
		t.Fatal(err)
	}
	p := sup.currentProc(702)
	if err := sup.trackHosted(spec, newer.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = old.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("old process exit was not observed")
	}
	if sup.PID(702) != newer.Process.Pid {
		t.Fatal("old exit removed new deployment")
	}
	if err := sup.Stop(702); err != nil {
		t.Fatal(err)
	}
	sup.RetireOld(702, time.Second)
}

func TestHostedTrackingFallbackAndIdentityChange(t *testing.T) {
	cmd, _, _ := processTestChild(t)
	identity, err := readRuntimeProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	changed := identity
	changed.birth += "changed"
	done := make(chan struct{})
	go func() { waitHostedProcessExit(changed, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback did not reject reused process identity")
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("identity mismatch killed an unrelated process")
	}
	done = make(chan struct{})
	go func() { waitHostedProcessExit(identity, nil); close(done) }()
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback did not detect exit")
	}
}
