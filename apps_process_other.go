//go:build !linux && !darwin

package main

import "errors"

func readRuntimeProcessIdentity(int) (runtimeProcessIdentity, error) {
	return runtimeProcessIdentity{}, errors.ErrUnsupported
}

func watchRuntimeProcessExit(int) (*runtimeProcessExitWatch, error) {
	return nil, errors.ErrUnsupported
}
