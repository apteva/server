package main

import (
	"fmt"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// runtimeHostSetup is the operator's consent for a backend to change a shared
// target host (install OS packages) before preparing a release on it.
type runtimeHostSetup struct {
	ApprovedProfiles []string `json:"approved_profiles,omitempty"`
}

func runtimeRequiredHostProfiles(manifest sdk.Manifest) []string {
	if manifest.Runtime.Package == nil {
		return nil
	}
	return manifest.Runtime.Package.HostProfiles
}
func (p runtimePlacement) approvedHostProfiles() []string {
	if p.HostSetup == nil {
		return nil
	}
	return p.HostSetup.ApprovedProfiles
}

// runtimeHostSetupStatus resolves the release's required host profiles against
// what the backend reported for the target. install lists the approved
// profiles the backend will install; unverified lists profiles a backend that
// predates host profiles could not report, which never block on their own.
func runtimeHostSetupStatus(manifest sdk.Manifest, p runtimePlacement, caps runtimeBackendCaps, info *sdk.RuntimeTargetInfo) (install, unverified []string, err error) {
	required := runtimeRequiredHostProfiles(manifest)
	if len(required) == 0 || p.local() {
		return nil, nil, nil
	}
	approved := p.approvedHostProfiles()
	var unsupported, pending []string
	for _, name := range required {
		state := ""
		if info != nil {
			state = info.HostProfiles[name]
		}
		switch {
		case state == sdk.HostProfilePresent:
		case state == sdk.HostProfileInstallable && runtimeContains(approved, name):
			install = append(install, name)
		case state == sdk.HostProfileInstallable:
			pending = append(pending, name)
		case state == sdk.HostProfileUnsupported, state == "" && len(caps.HostProfiles) > 0 && !runtimeContains(caps.HostProfiles, name):
			unsupported = append(unsupported, name)
		default:
			unverified = append(unverified, name)
		}
	}
	if len(unsupported) > 0 {
		return install, unverified, fmt.Errorf("target cannot provide host profile %s", strings.Join(unsupported, ", "))
	}
	if len(pending) > 0 {
		return install, unverified, fmt.Errorf("target needs host setup %s; approve it with placement.host_setup.approved_profiles", strings.Join(pending, ", "))
	}
	return install, unverified, nil
}
func runtimeHostSetupCheck(manifest sdk.Manifest, p runtimePlacement, caps runtimeBackendCaps, info *sdk.RuntimeTargetInfo) runtimeCompatibilityCheck {
	install, unverified, err := runtimeHostSetupStatus(manifest, p, caps, info)
	c := runtimeCheck("Host setup", "host_setup_required", err)
	if err != nil {
		if strings.HasPrefix(err.Error(), "target cannot") {
			c.Code = "host_profile_unsupported"
		}
		return c
	}
	var notes []string
	if len(install) > 0 {
		notes = append(notes, "installs "+strings.Join(install, ", ")+" on the target")
	}
	if len(unverified) > 0 {
		notes = append(notes, "backend does not report "+strings.Join(unverified, ", ")+"; not verified")
	}
	c.Message = strings.Join(notes, "; ")
	return c
}
func validateRuntimeHostProfileStates(states map[string]string) error {
	if len(states) > 16 {
		return fmt.Errorf("too many host profiles")
	}
	for name, state := range states {
		if sdk.ValidateHostProfiles([]string{name}) != nil {
			return fmt.Errorf("invalid host profile name")
		}
		if state != sdk.HostProfilePresent && state != sdk.HostProfileInstallable && state != sdk.HostProfileUnsupported {
			return fmt.Errorf("invalid host profile state")
		}
	}
	return nil
}
