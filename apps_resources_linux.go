//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func localResourceCapabilities() *sdk.ResourceCapabilities {
	c := &sdk.ResourceCapabilities{Contract: sdk.ResourcePolicyVersion, ProcessControls: []string{}, ProcessApply: "unsupported", WorkloadApply: "cooperative", NetworkApply: "new_sockets"}
	if os.Getenv("APTEVA_APP_CGROUP_ROOT") != "" {
		c.ProcessControls = []string{"cpu_weight", "cpu_quota_percent", "memory_high_bytes", "memory_max_bytes", "io_weight"}
		c.ProcessApply = "next_deployment"
		c.Limitations = []string{"requires delegated cgroup v2 controllers; validated at launch", "inherited ancestor quotas still apply"}
	}
	return c
}
func validateLocalProcessResources(p sdk.ProcessResources) error {
	if e := (sdk.ResourcePolicy{Process: p}).Validate(); e != nil {
		return e
	}
	if p.Empty() {
		return nil
	}
	if p.Nice != nil {
		return fmt.Errorf("nice is not supported by local cgroup supervisor")
	}
	root := os.Getenv("APTEVA_APP_CGROUP_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("process controls require an administrator-delegated APTEVA_APP_CGROUP_ROOT")
	}
	_, e := os.Stat(filepath.Join(root, "cgroup.controllers"))
	if e != nil {
		return fmt.Errorf("delegated cgroup v2 unavailable: %w", e)
	}
	return nil
}
func configureLocalProcessResources(cmd *exec.Cmd, env map[string]string) (func(), error) {
	noop := func() {}
	raw := env["APTEVA_RESOURCE_POLICY"]
	if raw == "" {
		return noop, nil
	}
	var e sdk.ResourceEnvelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return noop, err
	}
	p := e.Policy.Process
	if err := validateLocalProcessResources(p); err != nil {
		return noop, err
	}
	if p.Empty() {
		return noop, nil
	}
	root := os.Getenv("APTEVA_APP_CGROUP_ROOT")
	dir, err := os.MkdirTemp(root, "app-"+e.InstallID+"-")
	if err != nil {
		return noop, err
	}
	cleanup := func() { _ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0600); _ = os.Remove(dir) }
	values := map[string]string{}
	if p.CPUWeight > 0 {
		values["cpu.weight"] = strconv.Itoa(p.CPUWeight)
	}
	if p.CPUQuotaPercent > 0 {
		values["cpu.max"] = fmt.Sprintf("%d 100000", p.CPUQuotaPercent*1000)
	}
	if p.MemoryHighBytes > 0 {
		values["memory.high"] = strconv.FormatInt(p.MemoryHighBytes, 10)
	}
	if p.MemoryMaxBytes > 0 {
		values["memory.max"] = strconv.FormatInt(p.MemoryMaxBytes, 10)
	}
	if p.IOWeight > 0 {
		values["io.weight"] = fmt.Sprintf("default %d", p.IOWeight)
	}
	for name, value := range values {
		path := filepath.Join(dir, name)
		if err = os.WriteFile(path, []byte(value), 0600); err != nil {
			cleanup()
			return noop, err
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil || strings.TrimSpace(string(got)) != value {
			cleanup()
			return noop, fmt.Errorf("cgroup %s readback failed", name)
		}
	}
	fd, err := os.Open(dir)
	if err != nil {
		cleanup()
		return noop, err
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	// CLONE_INTO_CGROUP places the child before application code executes.
	return func() { _ = fd.Close(); cleanup() }, nil
}

func inspectLocalProcessResources(pid int) map[string]string {
	out := map[string]string{}
	raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if e != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "0::/") {
			continue
		}
		rel := strings.TrimPrefix(line, "0::")
		if strings.Contains(rel, "..") {
			return out
		}
		dir := filepath.Join("/sys/fs/cgroup", rel)
		root := os.Getenv("APTEVA_APP_CGROUP_ROOT")
		relative, err := filepath.Rel(root, dir)
		if root == "" || err != nil || relative == "." || strings.HasPrefix(relative, "..") || !strings.HasPrefix(filepath.Base(dir), "app-") {
			return map[string]string{"scope": "inherited_shared; no per-app attribution"}
		}
		for _, name := range []string{"cpu.weight", "cpu.max", "memory.high", "memory.max", "io.weight", "cpu.stat", "cpu.pressure", "memory.pressure", "io.pressure"} {
			if data, e := os.ReadFile(filepath.Join(dir, name)); e == nil && len(data) <= 2048 {
				out[name] = strings.TrimSpace(string(data))
			}
		}
	}
	return out
}
