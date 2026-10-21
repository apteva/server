package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/apteva/app-sdk"
)

// Target inspection is authenticated but independent of an execution. Do not
// add installation/deployment/generation or credentials to this read-only body.
type runtimeTargetInspectionRequest struct {
	Contract   string             `json:"contract"`
	Target     runtimeResourceRef `json:"target"`
	ProjectID  string             `json:"project_id"`
	GatewayURL string             `json:"gateway_url"`
	Options    map[string]any     `json:"options,omitempty"`
}

type runtimeBackendHTTPError struct {
	Action string
	Status int
	Code   string
}

func (e *runtimeBackendHTTPError) Error() string {
	return fmt.Sprintf("backend %s returned HTTP %d", e.Action, e.Status)
}

type runtimeInspectionProtocolError struct{ message string }

func (e *runtimeInspectionProtocolError) Error() string { return e.message }

type runtimeTargetDiagnosticError struct{ name, status string }

func (e *runtimeTargetDiagnosticError) Error() string { return "Target " + e.name + " " + e.status }

// Codes, never arbitrary backend error text, cross the operator boundary.
func runtimeInspectionFailure(err error) (string, string) {
	var protocol *runtimeInspectionProtocolError
	if errors.As(err, &protocol) {
		return "inspection_protocol_incompatible", "Backend returned an invalid target-inspection response; update its target-inspection support"
	}
	var diagnostic *runtimeTargetDiagnosticError
	if errors.As(err, &diagnostic) {
		return "target_" + diagnostic.name + "_" + diagnostic.status, diagnostic.Error()
	}
	var response *runtimeBackendHTTPError
	if errors.As(err, &response) {
		switch response.Code {
		case "ssh_unreachable":
			return "ssh_unreachable", "Target SSH is unavailable"
		case "systemd_unavailable":
			return "systemd_unavailable", "Target systemd is unavailable"
		case "gateway_unreachable":
			return "gateway_unreachable", "Target cannot reach the management gateway"
		case "capacity_insufficient":
			return "capacity_insufficient", "Target has insufficient capacity"
		case "invalid_target":
			return "invalid_target", "Backend rejected the selected target"
		}
		switch response.Status {
		case 400, 404, 405, 422, 501:
			return "inspection_protocol_incompatible", fmt.Sprintf("Backend does not accept the artifact-independent inspect-target contract (HTTP %d); update its target-inspection support. This does not establish an SSH or gateway failure", response.Status)
		case 401, 403:
			return "backend_authentication_failed", "Backend rejected target-inspection authentication or authorization"
		}
		return "backend_inspection_failed", fmt.Sprintf("Backend target inspection failed (HTTP %d); no SSH, systemd or gateway diagnosis was returned", response.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "inspection_timeout", "Target inspection did not complete within the request deadline"
	}
	return "backend_unreachable", "Target-inspection backend is unavailable; target connectivity has not been established"
}

func (m *appRuntimeManager) inspectTarget(ctx context.Context, p runtimePlacement, project string) (sdk.RuntimeTargetInfo, error) {
	var info sdk.RuntimeTargetInfo
	reg, err := m.registration(p.Backend)
	if err != nil {
		return info, err
	}
	if !reg.Caps.TargetInspection {
		return info, fmt.Errorf("backend does not advertise artifact-independent target inspection")
	}
	var base, status string
	if err = m.s.store.db.QueryRow(`SELECT sidecar_url_override,status FROM app_installs WHERE id=?`, reg.InstallID).Scan(&base, &status); err != nil {
		return info, err
	}
	if status != "running" || runtimeSecureURL(base) != nil {
		return info, fmt.Errorf("backend unavailable")
	}
	token, err := m.s.runtimeControlToken(reg.InstallID)
	if err != nil {
		return info, err
	}
	backend := remoteRuntimeBackend{m, reg, base, token}
	options := make(map[string]any, len(p.Options)+1)
	for key, value := range p.Options {
		options[key] = value
	}
	delete(options, "artifact_id")
	options["gateway_url"] = p.GatewayURL
	input := runtimeTargetInspectionRequest{Contract: runtimeBackendContract, ProjectID: project, Target: p.Target, GatewayURL: p.GatewayURL, Options: options}
	if err = backend.request(ctx, http.MethodPost, "inspect-target", input, &info); err != nil {
		return info, err
	}
	if err = validateRuntimeTargetInfo(info, reg.Caps); err != nil {
		return info, &runtimeInspectionProtocolError{message: err.Error()}
	}
	for _, check := range info.Checks {
		if check.Status != "passed" {
			return info, &runtimeTargetDiagnosticError{name: check.Name, status: check.Status}
		}
	}
	return info, nil
}
func validateRuntimeTargetInfo(info sdk.RuntimeTargetInfo, caps runtimeBackendCaps) error {
	if info.HostKey == "" || len(info.HostKey) > 512 || !runtimeContains(caps.Platforms, info.Platform) || len(info.Formats) == 0 || len(info.SystemLibraries) > 2048 || len(info.Checks) > 4 {
		return fmt.Errorf("invalid target platform/identity metadata")
	}
	for _, format := range info.Formats {
		if !runtimeContains(caps.Formats, format) {
			return fmt.Errorf("target returned an unadvertised delivery format")
		}
	}
	if info.ABI != "" && info.ABI != "gnu" && info.ABI != "musl" {
		return fmt.Errorf("unsupported target ABI")
	}
	for _, lib := range info.SystemLibraries {
		if !runtimeSystemLibraryName(lib) {
			return fmt.Errorf("invalid target library inventory")
		}
	}
	seen := map[string]bool{}
	for _, check := range info.Checks {
		if seen[check.Name] {
			return fmt.Errorf("duplicate target diagnostic category")
		}
		seen[check.Name] = true
		if check.Name != "ssh" && check.Name != "systemd" && check.Name != "gateway" && check.Name != "capacity" {
			return fmt.Errorf("invalid target diagnostic category")
		}
		if check.Status != "passed" && check.Status != "blocked" && check.Status != "unknown" {
			return fmt.Errorf("invalid target diagnostic status")
		}
	}
	return nil
}
