package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"os"
	"os/exec"
)

type processResourceOwner struct {
	Kind string
	ID   string
}

func (o processResourceOwner) prefix() (string, error) {
	if (o.Kind != "app" && o.Kind != "agent") || !runtimeID(o.ID) {
		return "", fmt.Errorf("invalid process resource owner")
	}
	return o.Kind + "-" + o.ID + "-", nil
}
func processResourceRoot() string {
	if root := os.Getenv("APTEVA_CGROUP_ROOT"); root != "" {
		return root
	}
	return os.Getenv("APTEVA_APP_CGROUP_ROOT")
}

// Generic-root opt-in gives ordinary apps and agents their own baseline group.
// The legacy app-only root retains its existing explicit-policy behavior.
func resolveProcessResources(p sdk.ProcessResources) sdk.ProcessResources {
	if os.Getenv("APTEVA_CGROUP_ROOT") != "" && p.CPUWeight == 0 {
		p.CPUWeight = 100
	}
	return p
}
func configureLocalProcessResources(cmd *exec.Cmd, env map[string]string) (func(), error) {
	var e sdk.ResourceEnvelope
	if raw := env["APTEVA_RESOURCE_POLICY"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return func() {}, err
		}
	}
	if e.InstallID == "" {
		e.InstallID = env["APTEVA_INSTALL_ID"]
	}
	// Legacy apps without an identity and without a policy remain unchanged.
	if e.InstallID == "" && e.Policy.Process.Empty() && os.Getenv("APTEVA_CGROUP_ROOT") == "" {
		return func() {}, nil
	}
	return configureProcessResources(cmd, processResourceOwner{"app", e.InstallID}, resolveProcessResources(e.Policy.Process))
}
