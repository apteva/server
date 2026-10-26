package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func runtimeCapabilitiesKnown(d runtimeDeployment) bool {
	// Older journals had no presence bit. Nonempty persisted capabilities are
	// still positive evidence; an old all-zero record is not a negative answer.
	return d.AppCapsKnown || d.AppCaps != (runtimeAppCaps{})
}

func (m *appRuntimeManager) releaseCapabilities(id int64, manifest sdk.Manifest, p runtimePlacement) (runtimeAppCaps, bool) {
	if caps, ok := declaredRuntimeCapabilities(manifest, p, runtimeDeployment{}); ok {
		return caps, true
	}
	previous, err := m.deployments(id)
	if err != nil {
		return runtimeAppCaps{}, false
	}
	for i := len(previous) - 1; i >= 0; i-- {
		d := previous[i]
		if runtimeFingerprint(d.Manifest) != runtimeFingerprint(manifest) {
			continue
		}
		if d.Package != nil {
			return d.Package.Metadata.AppCapabilities, true
		}
		if runtimeCapabilitiesKnown(d) {
			return d.AppCaps, true
		}
	}
	return runtimeAppCaps{}, false
}

// Unknown transport/auth/decoding failures never mean lifecycle unsupported.
// Only an explicit missing endpoint is compatible with a legacy app.
func (m *appRuntimeManager) readAppCapabilities(ctx context.Context, endpoint, token string) (runtimeAppCaps, error) {
	var caps runtimeAppCaps
	if err := runtimeSecureURL(endpoint); err != nil {
		return caps, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(endpoint, runtimeControlPrefix+"app/describe"), nil)
	if err != nil {
		return caps, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return caps, fmt.Errorf("app lifecycle capabilities unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 405 {
		return caps, nil
	}
	if resp.StatusCode != 200 {
		return caps, fmt.Errorf("app lifecycle describe returned HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, runtimeResponseLimit))
	var raw json.RawMessage
	if dec.Decode(&raw) != nil || dec.Decode(new(any)) != io.EOF || json.Unmarshal(raw, &caps) != nil {
		return caps, fmt.Errorf("invalid app lifecycle capabilities")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["passive_start"] == nil || fields["drain"] == nil || string(fields["passive_start"]) == "null" || string(fields["drain"]) == "null" {
		return caps, fmt.Errorf("app lifecycle capability declaration is incomplete")
	}
	if caps.Routing != "" && caps.Routing != "key" && caps.Routing != "stateless" {
		return caps, fmt.Errorf("invalid app lifecycle routing")
	}
	return caps, nil
}

func (m *appRuntimeManager) verifyRuntimeCapabilities(d *runtimeDeployment, endpoint, token string) error {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	actual, err := m.readAppCapabilities(ctx, endpoint, token)
	if err != nil {
		return err
	}
	if runtimeCapabilitiesKnown(*d) && actual != d.AppCaps {
		return fmt.Errorf("running app lifecycle capabilities differ from the approved release")
	}
	if d.Package != nil && actual != d.Package.Metadata.AppCapabilities {
		return fmt.Errorf("running app lifecycle capabilities differ from sealed release metadata")
	}
	if d.Component != nil && actual != d.Component.Capabilities {
		return fmt.Errorf("running component lifecycle capabilities differ from its declaration")
	}
	d.AppCaps, d.AppCapsKnown = actual, true
	return nil
}

func runtimeNeedsAdmission(d runtimeDeployment) bool {
	return d.AppCaps.PassiveStart || d.AppCaps.Drain
}

// Activation acknowledgement is not admission. Check the app's authenticated,
// exact-generation status before publishing routing or reporting rollback done.
func (m *appRuntimeManager) activateAndConfirm(d runtimeDeployment, op runtimeOperation) error {
	if !runtimeCapabilitiesKnown(d) {
		return fmt.Errorf("app lifecycle capabilities are unknown")
	}
	if err := m.appLifecycle(d, "activate"); err != nil {
		return err
	}
	if !runtimeNeedsAdmission(d) {
		health := m.runtimeAppHealth(d, d.Execution)
		if !health.Healthy || health.IdentityVerified && !health.Admitting {
			return fmt.Errorf("app is not admitting work after activation")
		}
		return nil
	}
	budget := 30 * time.Second
	if d.Manifest.Runtime.StartupTimeoutSeconds > 0 {
		budget = time.Duration(d.Manifest.Runtime.StartupTimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(m.ctx, budget)
	defer cancel()
	for {
		if err := m.checkOwner(op); err != nil {
			return err
		}
		health := m.runtimeAppHealthContext(ctx, d, d.Execution)
		if health.Healthy && health.IdentityVerified && health.Admitting {
			return nil
		}
		if health.Admission == "fenced" || health.Admission == "draining" {
			return fmt.Errorf("app activation did not admit this execution: %s", health.Admission)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("app activation admission was not confirmed: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
