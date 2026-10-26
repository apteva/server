package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const runtimeResponseLimit = 1 << 20

// Retry is authorized by the backend response, never inferred from a conflict.
type runtimeBackendRetryError struct {
	message string
	delay   time.Duration
}

func (e *runtimeBackendRetryError) Error() string { return e.message }

type remoteRuntimeBackend struct {
	manager      *appRuntimeManager
	registration runtimeBackendRegistration
	base         string
	token        string
}

func (b *remoteRuntimeBackend) Describe() runtimeBackendCaps { return b.registration.Caps }
func (b *remoteRuntimeBackend) request(ctx context.Context, method, action string, in any, out any) error {
	var body io.Reader
	if in != nil {
		body = bytes.NewBufferString(runtimeJSON(in))
	}
	req, err := http.NewRequestWithContext(ctx, method, runtimeEndpoint(b.base, b.registration.Endpoint)+"/"+action, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	if execution, ok := in.(*runtimeBackendRequest); ok && execution != nil {
		req.Header.Set("Idempotency-Key", execution.IdempotencyKey+":"+action)
	}
	resp, err := b.manager.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("backend %s %s: transport unavailable", b.registration.ID, action)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		message := fmt.Sprintf("backend %s %s returned HTTP %d", b.registration.ID, action, resp.StatusCode)
		if resp.StatusCode == 409 || resp.StatusCode == 429 || resp.StatusCode == 503 {
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 1 && seconds <= 10 {
				return &runtimeBackendRetryError{message, time.Duration(seconds) * time.Second}
			}
		}
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&problem)
		return &runtimeBackendHTTPError{Action: action, Status: resp.StatusCode, Code: problem.Code}
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, runtimeResponseLimit+1))
	if err != nil || len(raw) > runtimeResponseLimit {
		return &runtimeInspectionProtocolError{message: "backend response exceeds limit or is unreadable"}
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return &runtimeInspectionProtocolError{message: "backend returned invalid JSON"}
	}
	return nil
}
func (b *remoteRuntimeBackend) invoke(action string, in runtimeBackendRequest) (runtimeExecution, error) {
	ctx, cancel := context.WithTimeout(b.manager.ctx, 30*time.Second)
	defer cancel()
	return b.invokeContext(ctx, action, in)
}
func (b *remoteRuntimeBackend) invokeContext(ctx context.Context, action string, in runtimeBackendRequest) (runtimeExecution, error) {
	var out runtimeExecution
	for {
		err := b.request(ctx, http.MethodPost, action, &in, &out)
		var retry *runtimeBackendRetryError
		if err == nil || !errors.As(err, &retry) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("backend %s %s retry deadline exceeded", b.registration.ID, action)
		case <-time.After(retry.delay):
		}
	}
}
func (b *remoteRuntimeBackend) Prepare(in runtimeBackendRequest) (runtimeExecution, error) {
	target, err := b.invoke("validate-target", in)
	if err != nil {
		return runtimeExecution{}, err
	}
	if in.Package != nil && (target.Platform != in.Package.Platform || target.ReleaseDigest != in.Package.ExecutableSHA256) {
		return target, fmt.Errorf("validated target does not match the expected platform/executable")
	}
	if target.HostKey == "" {
		return runtimeExecution{}, fmt.Errorf("backend did not resolve a physical host identity")
	}
	prepared, err := b.invoke("prepare", in)
	if err != nil {
		return prepared, err
	}
	if prepared.HostKey != target.HostKey {
		return prepared, fmt.Errorf("prepared host differs from validated target")
	}
	return prepared, nil
}

func (b *remoteRuntimeBackend) Start(in runtimeBackendRequest) (runtimeExecution, error) {
	return b.invoke("start", in)
}
func (b *remoteRuntimeBackend) Inspect(in runtimeBackendRequest) (runtimeExecution, error) {
	return b.inspectContext(b.manager.ctx, in)
}
func (b *remoteRuntimeBackend) inspectContext(ctx context.Context, in runtimeBackendRequest) (runtimeExecution, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unlock, err := b.manager.lockRuntimeInspection(ctx, in.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	defer unlock()
	return b.invokeContext(ctx, "inspect", in)
}
func (b *remoteRuntimeBackend) Stop(in runtimeBackendRequest) (runtimeExecution, error) {
	return b.invoke("stop", in)
}
func (b *remoteRuntimeBackend) Fence(in runtimeBackendRequest) (runtimeExecution, error) {
	if !b.Describe().Fencing {
		return runtimeExecution{}, fmt.Errorf("backend does not support fencing")
	}
	return b.invoke("fence", in)
}
func (b *remoteRuntimeBackend) Delete(in runtimeBackendRequest) error {
	_, err := b.invoke("delete", in)
	return err
}
func (m *appRuntimeManager) registration(ref runtimeBackendRef) (runtimeBackendRegistration, error) {
	var v runtimeBackendRegistration
	var raw string
	err := m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_backends WHERE install_id=? AND backend_id=?`, ref.InstallID, ref.ID).Scan(&raw)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}
func (m *appRuntimeManager) backend(d runtimeDeployment) (runtimeBackend, error) {
	if d.Placement.local() {
		return &localRuntimeBackend{m: m, d: d}, nil
	}
	reg, err := m.registration(d.Placement.Backend)
	if err != nil {
		return nil, fmt.Errorf("execution backend is not registered")
	}
	var base, status string
	err = m.s.store.db.QueryRow(`SELECT sidecar_url_override,status FROM app_installs WHERE id=?`, reg.InstallID).Scan(&base, &status)
	if err != nil || status != "running" {
		return nil, fmt.Errorf("execution backend is unavailable")
	}
	if err = runtimeSecureURL(base); err != nil {
		return nil, err
	}
	token, err := m.s.runtimeControlToken(reg.InstallID)
	if err != nil {
		return nil, err
	}
	return &remoteRuntimeBackend{m, reg, base, token}, nil
}
func (m *appRuntimeManager) registerBackend(reg runtimeBackendRegistration) error {
	return m.registerBackendContext(m.ctx, reg)
}
func (m *appRuntimeManager) registerBackendContext(parent context.Context, reg runtimeBackendRegistration) error {
	if err := validateTargetDiscovery(reg.TargetDiscovery); err != nil {
		return err
	}
	if reg.TargetDiscovery == nil {
		if previous, e := m.registration(runtimeBackendRef{reg.InstallID, reg.ID}); e == nil {
			reg.TargetDiscovery = previous.TargetDiscovery
		}
	}
	if reg.InstallID <= 0 || !runtimeID(reg.ID) || reg.Endpoint != runtimeControlPrefix+"backends/"+reg.ID {
		return fmt.Errorf("backend endpoint must be /_apteva/runtime/backends/<id>")
	}
	var base, status string
	if err := m.s.store.db.QueryRow(`SELECT sidecar_url_override,status FROM app_installs WHERE id=?`, reg.InstallID).Scan(&base, &status); err != nil {
		return err
	}
	if status != "running" {
		return fmt.Errorf("backend installation must be running")
	}
	if err := runtimeSecureURL(base); err != nil {
		return err
	}
	token, err := m.s.runtimeControlToken(reg.InstallID)
	if err != nil {
		return err
	}
	b := remoteRuntimeBackend{m, reg, base, token}
	reg.Caps = runtimeBackendCaps{}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err = b.request(ctx, http.MethodGet, "describe", nil, &reg.Caps); err != nil {
		return err
	}
	if reg.Caps.StateTransfer != nil {
		if err := validateRuntimeStatePolicy(*reg.Caps.StateTransfer); err != nil {
			return err
		}
	}
	if reg.Caps.TargetDiscovery != nil {
		if err := validateTargetDiscovery(reg.Caps.TargetDiscovery); err != nil {
			return err
		}
		reg.TargetDiscovery = reg.Caps.TargetDiscovery
	}
	if reg.Caps.Contract != runtimeBackendContract || reg.Caps.ID != reg.ID || !reg.Caps.ConfirmedStop {
		return fmt.Errorf("unsupported backend contract or missing confirmed-stop guarantee")
	}
	if reg.Caps.Fencing && reg.Caps.FencingScope != "deployment" {
		return fmt.Errorf("v1 fencing must be scoped to the owned deployment")
	}
	if len(reg.Caps.RuntimeKinds) == 0 || len(reg.Caps.Formats) == 0 {
		return fmt.Errorf("backend must declare runtime kinds and delivery formats")
	}
	if reg.Caps.PackageDelivery != nil {
		if reg.PackageDelivery != nil && runtimeJSON(reg.PackageDelivery) != runtimeJSON(reg.Caps.PackageDelivery) {
			return fmt.Errorf("package delivery declaration conflicts with backend descriptor")
		}
		reg.PackageDelivery = reg.Caps.PackageDelivery
	}
	if reg.PackageDelivery != nil {
		if err := validateRuntimePackageDelivery(reg.PackageDelivery, reg.Caps); err != nil {
			return err
		}
	}
	_, err = m.s.store.db.Exec(`INSERT INTO app_runtime_backends(install_id,backend_id,record_json) VALUES(?,?,?) ON CONFLICT(install_id,backend_id) DO UPDATE SET record_json=excluded.record_json`, reg.InstallID, reg.ID, runtimeJSON(reg))
	return err
}
func (s *Server) runtimeControlToken(id int64) (string, error) {
	if s.runtimeManaged(id) {
		m := s.appRuntimeManager()
		d, err := m.active(id)
		if err == nil && d.CredentialEncrypted != "" {
			return Decrypt(s.secret, d.CredentialEncrypted)
		}
	}
	return s.appInstallToken(id)
}
func (m *appRuntimeManager) appCapabilities(id int64) (runtimeAppCaps, bool) {
	var base string
	if err := m.s.store.db.QueryRow(`SELECT sidecar_url_override FROM app_installs WHERE id=?`, id).Scan(&base); err != nil {
		return runtimeAppCaps{}, false
	}
	token, err := m.s.runtimeControlToken(id)
	if err != nil {
		return runtimeAppCaps{}, false
	}
	ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
	defer cancel()
	caps, err := m.readAppCapabilities(ctx, base, token)
	return caps, err == nil
}

var errRuntimeDraining = errors.New("app has not finished draining")

func (m *appRuntimeManager) appLifecycle(d runtimeDeployment, action string) error {
	return m.appLifecycleContext(m.ctx, d, action)
}
func (m *appRuntimeManager) appLifecycleContext(parent context.Context, d runtimeDeployment, action string) error {
	if !d.AppCaps.Drain && action == "drain" {
		return nil
	}
	if action == "activate" && !d.AppCaps.PassiveStart && !d.AppCaps.Drain {
		return nil
	}
	if err := runtimeSecureURL(d.Execution.Endpoint); err != nil {
		return err
	}
	token, err := m.token(d)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, runtimeEndpoint(d.Execution.Endpoint, runtimeControlPrefix+"app/"+action), strings.NewReader(runtimeJSON(map[string]any{"deployment_id": d.ID, "generation": d.Generation})))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", d.ID+":"+action)
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("app lifecycle %s unavailable", action)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("app lifecycle %s returned HTTP %d", action, resp.StatusCode)
	}
	if action == "drain" {
		var out struct {
			Drained bool `json:"drained"`
		}
		if err = json.NewDecoder(io.LimitReader(resp.Body, runtimeResponseLimit)).Decode(&out); err != nil {
			return fmt.Errorf("invalid app drain response")
		}
		if !out.Drained {
			return errRuntimeDraining
		}
	}
	return nil
}
func runtimeValidControlPath(v string) bool {
	u, e := url.Parse(v)
	return e == nil && u.Path == v && strings.HasPrefix(v, runtimeControlPrefix) && !strings.Contains(v, "..")
}
