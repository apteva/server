package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type localRuntimeBackend struct {
	m *appRuntimeManager
	d runtimeDeployment
}

func (b *localRuntimeBackend) Describe() runtimeBackendCaps {
	policy, _ := serverRuntimeStatePolicy()
	return runtimeBackendCaps{StateTransfer: &policy, Contract: runtimeBackendContract, ID: "local", RuntimeKinds: []string{"source", "service", "static"}, Platforms: []string{localPlatform()}, Formats: []string{"artifact", "source", "binary"}, Protocols: []string{"tcp", "udp"}, Snapshots: true, ConfirmedStop: true}
}
func (b *localRuntimeBackend) Prepare(req runtimeBackendRequest) (runtimeExecution, error) {
	if req.Manifest.Runtime.Kind == "static" {
		return b.prepareStatic(req)
	}
	if b.m.s.localApps == nil {
		return runtimeExecution{}, fmt.Errorf("local supervisor unavailable")
	}
	sup := b.m.s.localApps
	release := sup.acquireRuntimeBuildSlot(&req.Manifest)
	defer release()
	unlock := sup.lockApp(req.Manifest.Name, req.Manifest.Version)
	defer unlock()
	var path string
	var err error
	if hasLocalArtifact(&req.Manifest) || req.Manifest.Runtime.Source != nil {
		path, err = sup.BuildFromSourceBinary(&req.Manifest, func(string) {})
	} else {
		raw, ok := req.Manifest.Runtime.Binaries[localPlatform()]
		if !ok {
			return runtimeExecution{}, fmt.Errorf("no local binary delivery")
		}
		path, err = sup.fetchBinary(req.Manifest.Name, req.Manifest.Version, raw)
	}
	if err != nil {
		return runtimeExecution{}, err
	}
	ports, e := fixedRuntimePorts(&req.Manifest)
	if e != nil {
		return runtimeExecution{}, e
	}
	if e = sup.RestorePendingFixedPorts(req.InstallID, req.Manifest.Name, ports); e != nil {
		return runtimeExecution{}, e
	}
	digest, err := artifactFileHash(path)
	if err != nil {
		return runtimeExecution{}, err
	}
	if a, ok := req.Manifest.Runtime.Artifacts[localPlatform()]; ok {
		digest = a.SHA256
	}
	port, err := freePort()
	if err != nil {
		return runtimeExecution{}, err
	}
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	d.LocalBin = path
	d.LocalPort = port
	if err = b.m.saveDeployment(d); err != nil {
		return runtimeExecution{}, err
	}
	out := runtimeExecution{AppCapabilities: d.AppCaps, Handle: d.ID, State: "prepared", HostKey: "server:local", StateRef: fmt.Sprintf("local:%d", d.InstallID), ReleaseDigest: digest, Platform: localPlatform()}
	for _, p := range req.Manifest.Runtime.Ports {
		if p.HostPort > 0 {
			protocol := p.Protocol
			if protocol == "" {
				protocol = "tcp"
			}
			out.Ports = append(out.Ports, runtimePortBinding{protocol, p.HostPort, p.HostPort, true})
		}
	}
	return out, nil
}
func (b *localRuntimeBackend) Start(req runtimeBackendRequest) (runtimeExecution, error) {
	if req.Manifest.Runtime.Kind == "static" {
		return b.inspectStatic(req)
	}
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	if d.LocalBin == "" {
		return runtimeExecution{}, fmt.Errorf("local binary is not prepared")
	}
	if _, err = os.Stat(d.LocalBin); err != nil {
		return runtimeExecution{}, fmt.Errorf("exact prepared binary is missing; no implicit rebuild")
	}
	sup := b.m.s.localApps
	if p := sup.currentProc(d.InstallID); p != nil && p.spec.env["APTEVA_DEPLOYMENT_ID"] == d.ID {
		return b.Inspect(req)
	}
	env := req.Environment
	if env == nil {
		raw, e := Decrypt(b.m.s.secret, d.EnvironmentEncrypted)
		if e != nil {
			return runtimeExecution{}, e
		}
		if e = json.Unmarshal([]byte(raw), &env); e != nil {
			return runtimeExecution{}, e
		}
	}
	pidPath := b.m.localPIDPath(d)
	if err = os.MkdirAll(filepath.Dir(pidPath), 0700); err != nil {
		return runtimeExecution{}, err
	}
	env["APTEVA_RUNTIME_PID_FILE"] = pidPath
	if env["APTEVA_DEPLOYMENT_ID"] == "" {
		env["APTEVA_DEPLOYMENT_ID"] = d.ID
	}
	if req.Manifest.Runtime.Source != nil {
		root := filepath.Join(sup.cacheDir, req.Manifest.Name, req.Manifest.Version, "src", req.Manifest.Runtime.Source.Entry)
		env["APTEVA_UI_DIR"] = filepath.Join(root, "ui")
		if req.Manifest.DB != nil {
			env["APTEVA_MIGRATIONS_DIR"] = filepath.Join(root, req.Manifest.DB.Migrations)
		}
	}
	applyArtifactResourceEnv(&req.Manifest, d.LocalBin, env)
	spec, err := newActivationSpec(d.InstallID, &req.Manifest, d.LocalBin, d.LocalPort, env)
	if err != nil {
		return runtimeExecution{}, err
	}
	if err = sup.spawn(spec); err != nil {
		d.Status = "prepared"
		_ = b.m.saveDeployment(d)
		return runtimeExecution{}, err
	}
	d.LocalPID = sup.PID(d.InstallID)
	if err = b.m.saveDeployment(d); err != nil {
		return runtimeExecution{}, err
	}
	return b.Inspect(req)
}
func (b *localRuntimeBackend) Inspect(req runtimeBackendRequest) (runtimeExecution, error) {
	if req.Manifest.Runtime.Kind == "static" {
		return b.inspectStatic(req)
	}
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	out := d.Execution
	p := b.m.s.localApps.currentProc(d.InstallID)
	if p == nil || p.spec.env["APTEVA_DEPLOYMENT_ID"] != "" && p.spec.env["APTEVA_DEPLOYMENT_ID"] != d.ID {
		// Blue-green predecessors are still tracked in pending until retired.
		b.m.s.localApps.pendingMu.Lock()
		p = b.m.s.localApps.pending[d.InstallID]
		b.m.s.localApps.pendingMu.Unlock()
		if p == nil || p.spec.binPath != d.LocalBin {
			pid, e := b.m.localRecordedPID(d)
			if e == nil && processAlive(pid) {
				if !runtimeProcessMatches(pid, d.LocalBin) {
					return out, fmt.Errorf("managed process identity cannot be confirmed")
				}
				out.State = "running"
				out.Endpoint = localSidecarURL(int64(d.LocalPort))
				out.Ready = true
				out.StoppedConfirmed = false
				return out, nil
			}
			if os.IsNotExist(e) && d.Status == "starting" {
				return out, fmt.Errorf("managed start outcome is uncertain; PID record not yet observed")
			}
			out.State = "stopped"
			out.StoppedConfirmed = true
			out.Ready = false
			return out, nil
		}
	}
	out.State = "running"
	out.Endpoint = localSidecarURL(int64(p.port))
	out.Ready = waitFixedTCPPorts(p.spec.probeHost, p.spec.fixedPorts, time.Second) == nil
	return out, nil
}
func (b *localRuntimeBackend) Stop(req runtimeBackendRequest) (runtimeExecution, error) {
	if req.Manifest.Runtime.Kind == "static" {
		out := req.Execution
		out.State = "stopped"
		out.Ready = false
		out.StoppedConfirmed = true
		return out, nil
	}
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	sup := b.m.s.localApps
	p := sup.currentProc(d.InstallID)
	if p != nil && p.spec.binPath == d.LocalBin && p.port == d.LocalPort {
		if err = sup.Stop(d.InstallID); err != nil {
			return runtimeExecution{}, err
		}
	} else {
		sup.pendingMu.Lock()
		p = sup.pending[d.InstallID]
		sup.pendingMu.Unlock()
		if p != nil && p.spec.binPath == d.LocalBin && p.port == d.LocalPort {
			if err = terminateProc(p, 5*time.Second); err != nil {
				return runtimeExecution{}, err
			}
			sup.pendingMu.Lock()
			if sup.pending[d.InstallID] == p {
				delete(sup.pending, d.InstallID)
			}
			sup.pendingMu.Unlock()
		}
	}
	pid, e := b.m.localRecordedPID(d)
	if e == nil && processAlive(pid) {
		if !runtimeProcessMatches(pid, d.LocalBin) {
			return runtimeExecution{}, fmt.Errorf("refusing to stop an unrelated process")
		}
		if err = killOrphan(pid); err != nil {
			return runtimeExecution{}, err
		}
	}
	if os.IsNotExist(e) && d.Status == "starting" {
		return runtimeExecution{}, fmt.Errorf("cannot confirm historical start termination")
	}
	out := d.Execution
	out.State = "stopped"
	out.Ready = false
	out.StoppedConfirmed = true
	return out, nil
}
func (b *localRuntimeBackend) Fence(runtimeBackendRequest) (runtimeExecution, error) {
	return runtimeExecution{}, fmt.Errorf("local fencing is unavailable")
}
func (b *localRuntimeBackend) Delete(req runtimeBackendRequest) error {
	out, err := b.Inspect(req)
	if err != nil {
		return err
	}
	if !out.StoppedConfirmed && out.State == "running" {
		return fmt.Errorf("local execution still running")
	}
	return nil
} // persistent state is retained unless explicitly removed by installation teardown.
func (m *appRuntimeManager) localStateDir(d runtimeDeployment) string {
	return filepath.Join(m.s.localApps.cacheDir, d.Manifest.Name, "data", strconv.FormatInt(d.InstallID, 10))
}

func (m *appRuntimeManager) localPIDPath(d runtimeDeployment) string {
	return filepath.Join(m.s.localApps.cacheDir, ".runtime-pids", d.ID+".pid")
}
func (m *appRuntimeManager) localRecordedPID(d runtimeDeployment) (int, error) {
	raw, err := os.ReadFile(m.localPIDPath(d))
	if os.IsNotExist(err) && d.LocalPID > 0 {
		return d.LocalPID, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid < 1 {
		return 0, fmt.Errorf("invalid managed pid record")
	}
	return pid, nil
}
func runtimeProcessMatches(pid int, bin string) bool {
	if pid <= 0 || bin == "" {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return err == nil && strings.TrimSpace(string(out)) == bin
}
