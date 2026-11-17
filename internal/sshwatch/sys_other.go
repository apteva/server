//go:build !linux && !darwin

package sshwatch

import (
	"errors"
	"os"
)

var errLocked = errors.New("collector lock is held by another process")

func tryLock(string) (func(), error) {
	return nil, errors.New("the SSH collector requires Linux or macOS")
}

func freeSpace(string) (uint64, error) {
	return 0, errors.New("free space is unavailable on this platform")
}

func fileIdentity(os.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }

func dirWritable(string) bool { return false }
