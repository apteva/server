//go:build !linux

package main

import (
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"os/exec"
)

func localResourceCapabilities() *sdk.ResourceCapabilities {
	return &sdk.ResourceCapabilities{Contract: sdk.ResourcePolicyVersion, ProcessControls: []string{}, ProcessApply: "unsupported", WorkloadApply: "cooperative", NetworkApply: "new_sockets", Limitations: []string{"Linux delegated cgroup v2 required for process controls"}}
}
func validateLocalProcessResources(p sdk.ProcessResources) error {
	if err := (sdk.ResourcePolicy{Process: p}).Validate(); err != nil {
		return err
	}
	if err := validateHostResourcePolicy(sdk.ResourcePolicy{Process: p}); err != nil {
		return err
	}
	if !p.Empty() {
		return fmt.Errorf("process resource enforcement unavailable on this host")
	}
	return nil
}
func configureProcessResources(_ *exec.Cmd, owner processResourceOwner, p sdk.ProcessResources) (func(), error) {
	if _, err := owner.prefix(); err != nil {
		return func() {}, err
	}
	return func() {}, validateLocalProcessResources(p)
}
func inspectLocalProcessResources(int) map[string]string                  { return nil }
func inspectProcessResources(int, processResourceOwner) map[string]string { return nil }
func adoptProcessResources(int, processResourceOwner) func()              { return func() {} }
