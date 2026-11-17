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

func TestAgentResourceCgroupBeforeExec(t *testing.T) {
	root := os.Getenv("APTEVA_RESOURCE_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("isolated delegated Linux cgroup required")
	}
	t.Setenv("APTEVA_CGROUP_ROOT", root)
	for _, weight := range []int{0, 200} {
		cmd := exec.Command("/bin/sh", "-c", `cat /proc/self/cgroup; sleep 0.05`)
		cleanup, err := configureProcessResources(cmd, processResourceOwner{"agent", "17"}, resolveProcessResources(sdk.ProcessResources{CPUWeight: weight}))
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.CombinedOutput()
		cleanup()
		if err != nil {
			t.Fatal(err, string(output))
		}
		if !strings.Contains(string(output), "agent-17-") {
			t.Fatal("core executed outside its dedicated group", string(output))
		}
	}
}
func TestProcessResourceOwnerPrefixRejectsOtherAgent(t *testing.T) {
	if ownedProcessResourceDir("/sys/fs/cgroup/agent-170-abc", processResourceOwner{"agent", "17"}) {
		t.Fatal("another agent attributed")
	}
	if ownedProcessResourceDir("/sys/fs/cgroup/app-17-abc", processResourceOwner{"agent", "17"}) {
		t.Fatal("app attributed to agent")
	}
}
