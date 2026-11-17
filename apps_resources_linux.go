//go:build linux

package main

import (
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func localResourceCapabilities() *sdk.ResourceCapabilities {
	c := &sdk.ResourceCapabilities{Contract: sdk.ResourcePolicyVersion, ProcessControls: []string{}, ProcessApply: "unsupported", WorkloadApply: "cooperative", NetworkApply: "new_sockets"}
	if processResourceRoot() != "" && validateLocalProcessResources(sdk.ProcessResources{CPUWeight: 1}) == nil {
		c.ProcessControls = []string{"cpu_weight", "cpu_quota_percent", "memory_high_bytes", "memory_max_bytes", "io_weight"}
		c.ProcessApply = "next_deployment"
		c.Limitations = []string{"requires delegated cgroup v2 controllers; validated at launch", "inherited ancestor quotas still apply"}
	}
	return c
}
func validateLocalProcessResources(p sdk.ProcessResources) error {
	if err := (sdk.ResourcePolicy{Process: p}).Validate(); err != nil {
		return err
	}
	if err := validateHostResourcePolicy(sdk.ResourcePolicy{Process: p}); err != nil {
		return err
	}
	if p.Empty() {
		return nil
	}
	if p.Nice != nil {
		return fmt.Errorf("nice is not supported by local cgroup supervisor")
	}
	root := processResourceRoot()
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("process controls require an administrator-delegated APTEVA_CGROUP_ROOT (or legacy APTEVA_APP_CGROUP_ROOT)")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || uint64(fs.Type) != 0x63677270 {
		return fmt.Errorf("resource root must be a delegated cgroup v2 filesystem")
	}
	_, e := os.Stat(filepath.Join(root, "cgroup.controllers"))
	if e != nil {
		return fmt.Errorf("delegated cgroup v2 unavailable: %w", e)
	}
	if p.CPUWeight > 0 || p.CPUQuotaPercent > 0 {
		controllers, e := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
		if e != nil || !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " cpu ") {
			return fmt.Errorf("CPU controller is not delegated to workload groups")
		}
	}
	return nil
}
func configureProcessResources(cmd *exec.Cmd, owner processResourceOwner, p sdk.ProcessResources) (func(), error) {
	noop := func() {}
	prefix, err := owner.prefix()
	if err != nil {
		return noop, err
	}
	if err := validateLocalProcessResources(p); err != nil {
		return noop, err
	}
	if p.Empty() {
		return noop, nil
	}
	root := processResourceRoot()
	dir, err := os.MkdirTemp(root, prefix)
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
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	// CLONE_INTO_CGROUP places the child before application code executes.
	return func() { _ = fd.Close(); cleanup() }, nil
}

func inspectLocalProcessResources(pid int) map[string]string {
	return inspectProcessResources(pid, processResourceOwner{})
}
func inspectProcessResources(pid int, owner processResourceOwner) map[string]string {
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
		root := processResourceRoot()
		relative, err := filepath.Rel(root, dir)
		if root == "" || err != nil || relative == "." || strings.HasPrefix(relative, "..") || !ownedProcessResourceDir(dir, owner) {
			return map[string]string{"scope": "inherited_shared; no per-app attribution"}
		}
		out["scope"] = "dedicated"
		out["cgroup"] = dir
		for parent := filepath.Dir(dir); strings.HasPrefix(parent, "/sys/fs/cgroup"); parent = filepath.Dir(parent) {
			if data, e := os.ReadFile(filepath.Join(parent, "cpu.max")); e == nil {
				out["ancestor:"+parent+":cpu.max"] = strings.TrimSpace(string(data))
			}
			if parent == "/sys/fs/cgroup" {
				break
			}
		}
		for _, name := range []string{"cpu.weight", "cpu.max", "memory.high", "memory.max", "io.weight", "cpu.stat", "cpu.pressure", "memory.pressure", "io.pressure"} {
			if data, e := os.ReadFile(filepath.Join(dir, name)); e == nil && len(data) <= 2048 {
				out[name] = strings.TrimSpace(string(data))
			}
		}
	}
	return out
}

func ownedProcessResourceDir(dir string, owner processResourceOwner) bool {
	if owner.Kind == "" {
		return strings.HasPrefix(filepath.Base(dir), "app-")
	}
	prefix, e := owner.prefix()
	return e == nil && strings.HasPrefix(filepath.Base(dir), prefix)
}

// Reattachment never changes a surviving process's policy. Remove its group
// only after the exact process lifetime is gone, never during Server shutdown.
func adoptProcessResources(pid int, owner processResourceOwner) func() {
	identity, err := readRuntimeProcessIdentity(pid)
	if err != nil {
		return func() {}
	}
	observed := inspectProcessResources(pid, owner)
	dir := observed["cgroup"]
	if observed["scope"] != "dedicated" || dir == "" {
		return func() {}
	}
	return func() {
		current, e := readRuntimeProcessIdentity(pid)
		if e == nil && identity.sameProcess(current) {
			return
		}
		if e != nil && !runtimeProcessGone(e) {
			return
		}
		_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0600)
		_ = os.Remove(dir)
	}
}
