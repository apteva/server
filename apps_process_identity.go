package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Identity comes from the kernel, not argv (which the process can change).
// The birth token distinguishes different processes that reuse the same PID.
type runtimeProcessIdentity struct {
	pid        int
	birth      string
	executable string
	image      os.FileInfo
}

func (i runtimeProcessIdentity) matchesBinary(bin string) bool {
	if i.pid <= 0 || i.birth == "" || !filepath.IsAbs(bin) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return false
	}
	observed, err := filepath.EvalSymlinks(i.executable)
	if err != nil || resolved != observed {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular() && (i.image == nil || os.SameFile(i.image, info))
}

func (i runtimeProcessIdentity) sameProcess(other runtimeProcessIdentity) bool {
	return i.pid == other.pid && i.birth == other.birth && i.executable == other.executable &&
		(i.image == nil && other.image == nil || i.image != nil && other.image != nil && os.SameFile(i.image, other.image))
}

func runtimeProcessMatches(pid int, bin string) bool {
	identity, err := readRuntimeProcessIdentity(pid)
	return err == nil && identity.matchesBinary(bin)
}

// Native waiters subscribe to a particular process lifetime. They never reap a
// hosted process: cmd.Wait remains exclusively owned by the stable host.
type runtimeProcessExitWatch struct {
	wait  func(time.Duration) (bool, error)
	close func() error
}

func runtimeProcessGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func signalLocalProcHandle(p *localProc, sig syscall.Signal) error {
	if p.identity != nil {
		current, err := readRuntimeProcessIdentity(p.cmd.Process.Pid)
		if err != nil {
			return err
		}
		if !p.identity.sameProcess(current) {
			return fmt.Errorf("refusing to signal a different process lifetime or executable")
		}
	}
	return signalLocalProc(p.cmd.Process.Pid, sig)
}
