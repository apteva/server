//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import "golang.org/x/sys/unix"

func runtimeFilesystemFree(path string) (int64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * int64(s.Bsize), nil
}
