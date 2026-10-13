package main

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Only a backend's live contract can select a package pipeline. No provider
// names, resource-name guesses or trial writes to artifact endpoints are used.
func (m *appRuntimeManager) normalizePlacement(p *runtimePlacement) error {
	if p.local() {
		return p.validate()
	}
	if p.GatewayURL == "" {
		p.GatewayURL = strings.TrimRight(m.s.publicBaseURL(), "/")
		u, err := url.Parse(p.GatewayURL)
		if err != nil || u.Scheme != "https" {
			return fmt.Errorf("configure this Server's reachable public address before remote deployment")
		}
	}
	if p.Backend.InstallID == 0 {
		p.Backend.InstallID = p.Target.InstallID
	}
	if p.Backend.ID == "" {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		candidates, err := m.discoverBackendsFor(ctx, p.Backend.InstallID)
		if err != nil {
			return err
		}
		var compatible []runtimeBackendRegistration
		for _, c := range candidates {
			var schema struct {
				Properties map[string]struct {
					Const string `json:"const"`
				} `json:"properties"`
			}
			if json.Unmarshal(c.Caps.TargetSchema, &schema) == nil && schema.Properties["resource_type"].Const == p.Target.Type {
				compatible = append(compatible, c)
			}
		}
		if len(compatible) != 1 {
			return fmt.Errorf("target must have one compatible execution backend, or select backend_id explicitly")
		}
		p.Backend.ID = compatible[0].ID
	}
	if _, err := m.registration(p.Backend); err != nil {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		if _, err = m.discoverBackendsFor(ctx, p.Backend.InstallID); err != nil {
			return err
		}
	}
	return p.validate()
}
func (m *appRuntimeManager) inspectPackageTarget(d *runtimeDeployment, op *runtimeOperation, b *remoteRuntimeBackend) error {
	if !b.Describe().TargetInspection {
		if d.Placement.Package == nil || d.Placement.Package.Platform == "" {
			return fmt.Errorf("backend must advertise artifact-independent target inspection for automatic deployment")
		}
		return nil
	}
	req, err := m.request(*d, *op, "inspect-target")
	if err != nil {
		return err
	}
	var info sdk.RuntimeTargetInfo
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	if err = b.request(ctx, http.MethodPost, "inspect-target", &req, &info); err != nil {
		return err
	}
	if _, _, err = runtimePackagePlatform(info.Platform); err != nil || info.HostKey == "" || len(info.HostKey) > 512 || !runtimeContains(info.Formats, "binary") || len(info.SystemLibraries) > 2048 {
		return fmt.Errorf("backend returned invalid target metadata")
	}
	if info.ABI != "" && info.ABI != "gnu" && info.ABI != "musl" {
		return fmt.Errorf("backend returned unsupported target ABI")
	}
	for _, lib := range info.SystemLibraries {
		if !runtimeSystemLibraryName(lib) {
			return fmt.Errorf("backend returned invalid library inventory")
		}
	}
	if d.TargetInfo != nil && (d.TargetInfo.HostKey != info.HostKey || d.TargetInfo.Platform != info.Platform || d.TargetInfo.ABI != info.ABI) {
		return fmt.Errorf("deployment target identity/platform changed; create a new deployment")
	}
	if release := d.Manifest.Runtime.Package; release != nil && release.ABI != "" && release.ABI != info.ABI {
		return fmt.Errorf("release ABI is incompatible with selected target")
	}
	if d.Placement.Package == nil {
		d.Placement.Package = &runtimePackageSpec{}
	}
	spec := d.Placement.Package
	if spec.Platform != "" && spec.Platform != info.Platform || spec.ABI != "" && spec.ABI != info.ABI {
		return fmt.Errorf("requested package platform/ABI does not match selected target")
	}
	spec.Platform = info.Platform
	if spec.ABI == "" {
		spec.ABI = info.ABI
	}
	d.TargetInfo = &info
	if err = m.checkOwner(*op); err != nil {
		return err
	}
	return m.saveDeployment(*d)
}
func verifyRuntimeTargetRequirements(info *sdk.RuntimeTargetInfo, p *runtimePreparedPackage) error {
	if info == nil || p == nil {
		return nil
	}
	if p.Metadata.Platform != info.Platform || p.ABI != "" && p.ABI != info.ABI {
		return fmt.Errorf("prepared package is incompatible with target platform/ABI")
	}
	for _, lib := range p.SystemLibraries {
		if !runtimeContains(info.SystemLibraries, lib) {
			return fmt.Errorf("target lacks required runtime library %s", lib)
		}
	}
	return nil
}
