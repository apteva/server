//go:build linux

package main

import (
	sdk "github.com/apteva/app-sdk"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestLocalResourceEnforcementNeedsDelegation(t *testing.T) {
	t.Setenv("APTEVA_APP_CGROUP_ROOT", "")
	if validateLocalProcessResources(sdk.ProcessResources{CPUWeight: 200}) == nil {
		t.Fatal("claimed enforcement without delegation")
	}
	if err := validateLocalProcessResources(sdk.ProcessResources{}); err != nil {
		t.Fatal(err)
	}
}
func TestLocalResourceCgroupBeforeExec(t *testing.T) {
	root := os.Getenv("APTEVA_RESOURCE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("opt-in isolated delegated cgroup v2 subtree required")
	}
	t.Setenv("APTEVA_APP_CGROUP_ROOT", root)
	cmd := exec.Command("/bin/sh", "-c", "cat /proc/self/cgroup; sleep 0.05")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := map[string]string{"APTEVA_RESOURCE_POLICY": runtimeJSON(sdk.ResourceEnvelope{InstallID: "17", Policy: sdk.ResourcePolicy{Process: sdk.ProcessResources{CPUWeight: 200}}})}
	cleanup, e := configureLocalProcessResources(cmd, env)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	output, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatal(e, string(output))
	}
	if !strings.Contains(string(output), "app-17-") {
		t.Fatalf("child executed outside its group: %s", output)
	}
}
