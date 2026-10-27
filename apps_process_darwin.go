//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func readRuntimeProcessIdentity(pid int) (runtimeProcessIdentity, error) {
	var identity runtimeProcessIdentity
	if pid <= 0 {
		return identity, os.ErrNotExist
	}
	before, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return identity, darwinProcessError(pid, err)
	}
	if before.Proc.P_pid != int32(pid) || before.Proc.P_stat == 5 { // SZOMB
		return identity, os.ErrNotExist
	}
	// KERN_PROCARGS2 begins with argc, then the kernel's executable path,
	// followed by argv/environment. Never retain or log the latter.
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return identity, darwinProcessError(pid, err)
	}
	if len(raw) < 5 {
		return identity, fmt.Errorf("missing process executable path")
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end <= 0 {
		return identity, fmt.Errorf("invalid process executable path")
	}
	executable := string(raw[4 : 4+end])
	after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return identity, darwinProcessError(pid, err)
	}
	if after.Proc.P_pid != int32(pid) || after.Proc.P_stat == 5 {
		return identity, os.ErrNotExist
	}
	if after.Proc.P_starttime != before.Proc.P_starttime {
		return identity, fmt.Errorf("process changed during identity check")
	}
	birth := strconv.FormatInt(before.Proc.P_starttime.Sec, 10) + ":" + strconv.FormatInt(int64(before.Proc.P_starttime.Usec), 10)
	return runtimeProcessIdentity{pid: pid, birth: birth, executable: executable}, nil
}

func darwinProcessError(pid int, err error) error {
	// x/sys reports EIO when kern.proc.pid returns an empty result for a
	// reaped PID. Permission and transient failures must remain unknown.
	if unix.Kill(pid, 0) == unix.ESRCH {
		return os.ErrNotExist
	}
	return err
}

func watchRuntimeProcessExit(pid int) (*runtimeProcessExitWatch, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	event := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err = unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &runtimeProcessExitWatch{
		close: func() error { return unix.Close(fd) },
		wait: func(timeout time.Duration) (bool, error) {
			var events [1]unix.Kevent_t
			ts := unix.NsecToTimespec(timeout.Nanoseconds())
			n, err := unix.Kevent(fd, nil, events[:], &ts)
			if err == unix.EINTR {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if n == 0 {
				return false, nil
			}
			if events[0].Flags&unix.EV_ERROR != 0 {
				return false, fmt.Errorf("process exit subscription failed")
			}
			return events[0].Fflags&unix.NOTE_EXIT != 0, nil
		},
	}, nil
}
