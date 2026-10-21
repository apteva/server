package main

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/url"
	"strings"
	"time"
)

// Only a backend's live contract can select a package pipeline. No provider
// names, resource-name guesses or trial writes to artifact endpoints are used.
func (m *appRuntimeManager) normalizePlacement(p *runtimePlacement, readOnly ...bool) error {
	readonly := len(readOnly) > 0 && readOnly[0]
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
		var candidates []runtimeBackendRegistration
		var err error
		if readonly {
			rows, e := m.s.store.db.Query(`SELECT record_json FROM app_runtime_backends WHERE (?=0 OR install_id=?)`, p.Backend.InstallID, p.Backend.InstallID)
			err = e
			if e == nil {
				for rows.Next() {
					var raw string
					var reg runtimeBackendRegistration
					if e = rows.Scan(&raw); e != nil {
						err = e
						break
					}
					if e = json.Unmarshal([]byte(raw), &reg); e != nil {
						err = e
						break
					}
					candidates = append(candidates, reg)
				}
				if e = rows.Err(); e != nil {
					err = e
				}
				rows.Close()
			}
		} else {
			candidates, err = m.discoverBackendsFor(ctx, p.Backend.InstallID)
		}
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
		if readonly {
			return fmt.Errorf("execution backend is not registered")
		}
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
		return runtimeReleaseTargetCompatibility(d.Manifest, d.Placement, b.registration, nil)
	}
	var project string
	if err := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, d.InstallID).Scan(&project); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	info, err := m.inspectTarget(ctx, d.Placement, project)
	if err != nil {
		return err
	}
	if _, _, err = runtimePackagePlatform(info.Platform); err != nil || !runtimeContains(info.Formats, "binary") {
		return fmt.Errorf("target does not support the prepared binary package platform/format")
	}
	if d.TargetInfo != nil && (d.TargetInfo.HostKey != info.HostKey || d.TargetInfo.Platform != info.Platform || d.TargetInfo.ABI != info.ABI) {
		return fmt.Errorf("deployment target identity/platform changed; create a new deployment")
	}
	if err = runtimeReleaseTargetCompatibility(d.Manifest, d.Placement, b.registration, &info); err != nil {
		return err
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
