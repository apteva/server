package main

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func runtimeBackendCompatibility(manifest sdk.Manifest, caps runtimeBackendCaps) error {
	if !runtimeContains(caps.RuntimeKinds, manifest.Runtime.Kind) {
		return fmt.Errorf("backend does not support this app runtime kind")
	}
	if manifest.Runtime.Resources.CPULimit < 0 || manifest.Runtime.Resources.MemoryLimit < 0 {
		return fmt.Errorf("negative resource limit")
	}
	if (manifest.Runtime.Resources.CPULimit > 0 || manifest.Runtime.Resources.MemoryLimit > 0) && !caps.ResourceControls {
		return fmt.Errorf("backend cannot enforce declared resource limits")
	}
	for _, port := range manifest.Runtime.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if !runtimeContains(caps.Protocols, protocol) {
			return fmt.Errorf("backend does not support declared %s listeners", protocol)
		}
	}
	return nil
}
func runtimeReleaseTargetCompatibility(manifest sdk.Manifest, p runtimePlacement, reg runtimeBackendRegistration, info *sdk.RuntimeTargetInfo) error {
	if err := runtimeBackendCompatibility(manifest, reg.Caps); err != nil {
		return err
	}
	delivery := reg.PackageDelivery
	if delivery == nil {
		delivery = reg.Caps.PackageDelivery
	}
	if delivery != nil {
		if err := validateRuntimePackageDelivery(delivery, reg.Caps); err != nil {
			return err
		}
	}
	platform := ""
	if p.Package != nil {
		platform = p.Package.Platform
	}
	if info != nil {
		if platform != "" && platform != info.Platform {
			return fmt.Errorf("requested package platform differs from the inspected target")
		}
		platform = info.Platform
	}
	if platform != "" && !runtimeContains(reg.Caps.Platforms, platform) {
		return fmt.Errorf("backend does not support the selected target platform")
	}
	if delivery != nil && platform == "" {
		return fmt.Errorf("artifact-independent target inspection or an explicit package platform is required before building")
	}
	if delivery != nil {
		_, archive := manifest.Runtime.Artifacts[platform]
		if len(manifest.Runtime.Artifacts) > 0 && !archive {
			return fmt.Errorf("release has no verified artifact for the target platform")
		}
		if !archive && manifest.Runtime.Source == nil {
			return fmt.Errorf("binary package needs source or a checksummed platform archive")
		}
		spec := runtimePackageSpec{Platform: platform}
		if p.Package != nil {
			spec = *p.Package
			spec.Platform = platform
		}
		if manifest.Runtime.Package != nil && manifest.Runtime.Package.CGO {
			spec.CGO = true
		}
		if err := validateRuntimePackageSpec(spec); err != nil {
			return err
		}
		if _, ok := manifest.Runtime.Artifacts[platform]; ok && !delivery.VerifiedArchives {
			return fmt.Errorf("backend cannot verify this release's archive format as an executable package")
		}
		if info != nil && !runtimeContains(info.Formats, "binary") {
			return fmt.Errorf("target does not support prepared binary packages")
		}
	}
	abi := ""
	libs := []string{}
	if manifest.Runtime.Package != nil {
		abi = manifest.Runtime.Package.ABI
		libs = manifest.Runtime.Package.SystemLibraries
	}
	if p.Package != nil {
		if abi != "" && p.Package.ABI != "" && abi != p.Package.ABI {
			return fmt.Errorf("package ABI conflicts with the release declaration")
		}
		if p.Package.ABI != "" {
			abi = p.Package.ABI
		}
		libs = append(append([]string{}, libs...), p.Package.SystemLibraries...)
	}
	if (abi != "" || len(libs) > 0) && info == nil {
		return fmt.Errorf("target ABI/library inspection is required to verify this release")
	}
	if info != nil {
		pkg := runtimePreparedPackage{Metadata: runtimePackageMetadata{Platform: platform}, ABI: abi, SystemLibraries: libs}
		if err := verifyRuntimeTargetRequirements(info, &pkg); err != nil {
			return err
		}
	}
	return nil
}
func (m *appRuntimeManager) preflight(ctx context.Context, id int64, project string, manifest sdk.Manifest, p runtimePlacement) (runtimePlacement, []runtimeCompatibilityCheck) {
	checks := []runtimeCompatibilityCheck{runtimeCheck("App release", "app_release_incompatible", validateAppRuntimeCompatibility(&manifest, Version))}
	if err := m.normalizePlacement(&p, true); err != nil {
		return p, append(checks, runtimeCheck("Placement", "invalid_placement", err))
	}
	checks = append(checks, runtimeCheck("Placement", "", nil))
	if id > 0 && !p.local() {
		if err := m.validatePlacementAccess(id, p); err != nil {
			return p, append(checks, runtimeCheck("Placement access", "placement_access_denied", err))
		}
	}
	var reg runtimeBackendRegistration
	if p.local() {
		reg.Caps = (&localRuntimeBackend{m: m}).Describe()
	} else {
		var err error
		reg, err = m.registration(p.Backend)
		if err != nil {
			return p, append(checks, runtimeCheck("Backend", "backend_unavailable", fmt.Errorf("execution backend is not registered")))
		}
		var owner, status string
		if err = m.s.store.db.QueryRow(`SELECT COALESCE(project_id,''),status FROM app_installs WHERE id=?`, reg.InstallID).Scan(&owner, &status); err != nil || status != "running" || owner != "" && owner != project || p.Target.InstallID != p.Backend.InstallID {
			return p, append(checks, runtimeCheck("Placement access", "placement_access_denied", fmt.Errorf("backend and target must be available in this project")))
		}
	}
	checks = append(checks, runtimeCheck("Backend compatibility", "backend_incompatible", runtimeBackendCompatibility(manifest, reg.Caps)))
	if manifest.Runtime.DatabaseUpgrade == "requires_restore" {
		checks = append(checks, runtimeCheck("Database upgrade", "database_restore_required", fmt.Errorf("automatic activation requires an explicit database restore plan")))
	}
	if p.Ingress != nil && (!reg.Caps.ManagedIngress || manifest.Runtime.Ingress == nil) {
		checks = append(checks, runtimeCheck("HTTPS", "ingress_incompatible", fmt.Errorf("app and backend must declare managed public routes")))
	}
	var target *sdk.RuntimeTargetInfo
	if !p.local() && reg.Caps.TargetInspection {
		timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
		info, err := m.inspectTarget(timeout, p, project)
		cancel()
		if err != nil {
			code, message := runtimeInspectionFailure(err)
			checks = append(checks, runtimeCheck("Target inspection", code, fmt.Errorf("%s", message)))
		} else {
			target = &info
			checks = append(checks, runtimeCompatibilityCheck{Name: "Target platform", Status: "passed", Message: info.Platform})
			for _, diagnostic := range info.Checks {
				c := runtimeCompatibilityCheck{Name: "Target " + diagnostic.Name, Status: diagnostic.Status, Code: "target_" + diagnostic.Name}
				if diagnostic.Status != "passed" {
					c.Message = "Backend reports " + diagnostic.Name + " " + diagnostic.Status
				}
				checks = append(checks, c)
			}
		}
	}
	checks = append(checks, runtimeCheck("Package requirements", "package_incompatible", runtimeReleaseTargetCompatibility(manifest, p, reg, target)))
	if id > 0 {
		source, err := m.existingSource(id)
		if err != nil {
			checks = append(checks, runtimeCheck("Source installation", "source_unverified", err))
		} else if source.Exists {
			migration := m.migrationChecks(ctx, source.Deployment, manifest, p, nil, false)
			checks = append(checks, migration...)
		}
	}
	return p, checks
}
