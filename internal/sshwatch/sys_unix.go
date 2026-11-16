//go:build linux || darwin

package sshwatch

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// errLocked reports that another process holds the collector lock.
var errLocked = errors.New("collector lock is held by another process")

// tryLock takes a non-blocking exclusive flock. flock locks belong to the open
// file description, so two collectors in one process also exclude each other,
// and the kernel releases the lock when the holder dies. The file is opened
// read-only: flock needs no write access, so a lock file created by a root
// apteva-host still works for a dedicated collector user with
// CAP_DAC_READ_SEARCH.
func tryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func fileIdentity(fi os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

func dirWritable(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	return unix.Access(dir, unix.W_OK|unix.X_OK) == nil
}
