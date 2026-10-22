//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package main

import "fmt"

func runtimeFilesystemFree(string) (int64, error) {
	return 0, fmt.Errorf("state filesystem capacity inspection is unsupported on this platform")
}
