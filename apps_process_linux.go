//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func linuxProcessBirth(raw []byte, pid int) (string, error) {
	// comm may contain spaces and parentheses. Everything after its final ')'
	// starts at field 3; field 22 is the process start time in clock ticks.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 || !strings.HasPrefix(string(raw), strconv.Itoa(pid)+" (") {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("incomplete process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", os.ErrNotExist
	}
	if n, err := strconv.ParseUint(fields[19], 10, 64); err != nil || n == 0 {
		return "", fmt.Errorf("invalid process start time")
	}
	return fields[19], nil
}

func readRuntimeProcessIdentity(pid int) (runtimeProcessIdentity, error) {
	var identity runtimeProcessIdentity
	if pid <= 0 {
		return identity, os.ErrNotExist
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(root + "/stat")
	if err != nil {
		return identity, err
	}
	birth, err := linuxProcessBirth(stat, pid)
	if err != nil {
		return identity, err
	}
	executable, err := os.Readlink(root + "/exe")
	if err != nil {
		return identity, err
	}
	image, err := os.Stat(root + "/exe")
	if err != nil {
		return identity, err
	}
	// Re-read after resolving the executable so exit/PID reuse cannot combine
	// metadata from different process lifetimes into one identity.
	stat, err = os.ReadFile(root + "/stat")
	if err != nil {
		return identity, err
	}
	after, err := linuxProcessBirth(stat, pid)
	if err != nil || birth != after {
		return identity, fmt.Errorf("process changed during identity check")
	}
	return runtimeProcessIdentity{pid: pid, birth: birth, executable: executable, image: image}, nil
}

func watchRuntimeProcessExit(pid int) (*runtimeProcessExitWatch, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return &runtimeProcessExitWatch{
		close: func() error { return unix.Close(fd) },
		wait: func(timeout time.Duration) (bool, error) {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, err := unix.Poll(poll, int(timeout.Milliseconds()))
			if err == unix.EINTR {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if poll[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
				return false, fmt.Errorf("process exit descriptor failed")
			}
			return poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
		},
	}, nil
}
