//go:build !linux

package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"os/exec"
)

func localResourceCapabilities() *sdk.ResourceCapabilities {
	return &sdk.ResourceCapabilities{Contract: sdk.ResourcePolicyVersion, ProcessControls: []string{}, ProcessApply: "unsupported", WorkloadApply: "cooperative", NetworkApply: "new_sockets", Limitations: []string{"Linux delegated cgroup v2 required for process controls"}}
}
func validateLocalProcessResources(p sdk.ProcessResources) error {
	if !p.Empty() {
		return fmt.Errorf("process resource enforcement unavailable on this host")
	}
	return nil
}
func configureLocalProcessResources(_ *exec.Cmd, env map[string]string) (func(), error) {
	if raw := env["APTEVA_RESOURCE_POLICY"]; raw != "" {
		var e sdk.ResourceEnvelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return func() {}, err
		}
		return func() {}, validateLocalProcessResources(e.Policy.Process)
	}
	return func() {}, nil
}
func inspectLocalProcessResources(int) map[string]string { return nil }
