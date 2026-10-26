package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	runtimeHealthInterval          = 15 * time.Second
	runtimeInspectionRetryInterval = 2 * time.Minute
	runtimeObservationMaxAge       = 90 * time.Second
	runtimeMonitorConcurrency      = 4
)

type runtimeHealthObservation struct {
	State, Message, Admission   string
	IdentityVerified, Admitting bool
	Recovery                    string
	Healthy                     bool
	Load                        *float64
}
type runtimeMonitorEntry struct {
	Deployment                 runtimeDeployment
	Identity                   string
	NextHealth, NextFull       time.Time
	InFlight                   bool
	FullChecked, HealthChecked time.Time
	ProbeHealthy               bool
	HealthFailures             int
	RecoveryFailures           int
	FullOK                     bool
	Health                     runtimeHealthObservation
	Execution                  runtimeExecution
}

// Only active generations enter this bounded cache. UI reads the corresponding
// stored health observation; autoscaling reads fresh identity-bound workload from the same collector.
func (m *appRuntimeManager) queueRuntimeObservations(installID int64, immediate bool) {
	rows, err := m.s.store.db.Query(`SELECT d.id FROM app_runtime_deployments d JOIN app_installs i ON i.id=d.install_id WHERE i.runtime_managed=1 AND i.runtime_generation=d.generation AND d.status='active' AND i.status='running' AND (?=0 OR i.id=?) AND NOT EXISTS(SELECT 1 FROM app_runtime_operations o WHERE o.install_id=i.id AND o.status IN ('queued','running'))`, installID, installID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	deployments := make([]runtimeDeployment, 0, len(ids))
	for _, id := range ids {
		d, e := m.loadDeployment(id)
		if e != nil {
			return
		}
		deployments = append(deployments, d)
	}
	now := time.Now()
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if m.monitors == nil {
		m.monitors = map[string]*runtimeMonitorEntry{}
		m.monitorSlots = make(chan struct{}, runtimeMonitorConcurrency)
	}
	live := map[string]bool{}
	for _, d := range deployments {
		live[d.ID] = true
		identity := runtimeMonitorIdentity(d)
		entry := m.monitors[d.ID]
		if entry == nil || entry.Identity != identity || immediate {
			phase := runtimeMonitorPhase(d.ID, runtimeHealthInterval)
			if immediate {
				phase = 0
			}
			entry = &runtimeMonitorEntry{Deployment: d, Identity: identity, NextHealth: now.Add(phase), NextFull: now.Add(phase)}
			m.monitors[d.ID] = entry
		} else {
			entry.Deployment = d
		}
	}
	for id, entry := range m.monitors {
		if (installID == 0 || entry.Deployment.InstallID == installID) && !live[id] {
			delete(m.monitors, id)
		}
	}
}

// Periodic host audits are opt-in and deliberately infrequent. Environment
// changes take effect on Server restart; an invalid interval leaves audits off.
func runtimeInspectionAuditInterval() time.Duration {
	interval, err := time.ParseDuration(os.Getenv("APTEVA_RUNTIME_INSPECTION_AUDIT_INTERVAL"))
	if err != nil || interval < 15*time.Minute || interval > 30*time.Minute {
		return 0
	}
	return interval
}

func runtimeMonitorIdentity(d runtimeDeployment) string {
	return runtimeFingerprint(struct {
		Generation                               int64
		Placement                                runtimePlacement
		Handle, Host, Digest, Platform, Endpoint string
		Credential                               string
	}{d.Generation, d.Placement, d.Execution.Handle, d.Execution.HostKey, d.Execution.ReleaseDigest, d.Execution.Platform, d.Execution.Endpoint, d.CredentialEncrypted})
}
func runtimeMonitorPhase(id string, window time.Duration) time.Duration {
	sum := sha256.Sum256([]byte(id))
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(window))
}
func (m *appRuntimeManager) startRuntimeMonitor() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case now := <-tick.C:
				m.dispatchRuntimeObservations(now)
			}
		}
	}()
}
func (m *appRuntimeManager) dispatchRuntimeObservations(now time.Time) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	for id, entry := range m.monitors {
		if entry.InFlight || now.Before(entry.NextHealth) {
			continue
		}
		select {
		case m.monitorSlots <- struct{}{}:
		default:
			return
		}
		entry.InFlight = true
		d := entry.Deployment
		m.wg.Add(1)
		go func(id string, entry *runtimeMonitorEntry, d runtimeDeployment) {
			defer m.wg.Done()
			defer func() { <-m.monitorSlots }()
			m.checkRuntimeObservation(id, entry, d)
		}(id, entry, d)
	}
}

func (m *appRuntimeManager) checkRuntimeObservation(id string, entry *runtimeMonitorEntry, d runtimeDeployment) {
	defer func() {
		m.monitorMu.Lock()
		entry.InFlight = false
		if entry.NextHealth.Before(time.Now()) {
			entry.NextHealth = time.Now().Add(runtimeHealthInterval)
		}
		m.monitorMu.Unlock()
	}()
	if !m.runtimeMonitorCurrent(d) {
		return
	}
	m.monitorMu.Lock()
	due := !entry.NextFull.IsZero() && !time.Now().Before(entry.NextFull)
	previousHealthy, fullOK := entry.ProbeHealthy, entry.FullOK
	previouslyChecked := !entry.FullChecked.IsZero()
	previousExecution := entry.Execution
	failures := entry.HealthFailures
	recoveryFailures := entry.RecoveryFailures
	m.monitorMu.Unlock()
	out := d.Execution
	health := m.runtimeAppHealth(d, out)
	probeHealthy := health.Healthy
	if probeHealthy {
		failures = 0
	} else {
		failures++
	}
	// A transient failed health poll is not a reason to inspect the host. One
	// investigation per persistent outage is enough when ownership still verifies.
	// Failed verification retries are bounded; recovery never grants new ownership.
	full := d.Placement.local() || due || (health.Recovery != "" && !fullOK) || (!probeHealthy && failures == 3 && fullOK) || (probeHealthy && !previousHealthy && previouslyChecked && !fullOK)
	var verifyErr error
	verifiedAt := time.Time{}
	if full {
		verifiedAt = time.Now()
		backend, err := m.backend(d)
		if err == nil {
			var request runtimeBackendRequest
			request, err = m.request(d, runtimeOperation{ID: "monitor"}, "inspect")
			if err == nil {
				out, err = backend.Inspect(request)
			}
		}
		if err == nil {
			out, err = runtimeVerifiedObservation(d, out)
		}
		verifyErr = err
		// Fenced admission legitimately makes readiness false; identity and
		// process state can still be authoritatively verified for replacement.
		// Inspect may recreate a failed tunnel at a different endpoint. Probe
		// that verified endpoint even when its app readiness is currently false.
		if err == nil && out.State == "running" && out.Endpoint != d.Execution.Endpoint {
			health = m.runtimeAppHealth(d, out)
			probeHealthy = health.Healthy
		}
		fullOK = err == nil && out.State == "running" && (out.Ready || health.Recovery != "")
		if err == nil && out.StoppedConfirmed && d.Placement.local() {
			// Only a fresh, authoritative local stop can trigger the existing restart
			// flow. A failed probe or remote observation never authorizes a start.
			if m.runtimeMonitorCurrent(d) {
				_, _ = m.begin(d.InstallID, "restart", "reconcile-"+d.ID, d.Placement, nil)
			}
			return
		}
	} else {
		out = previousExecution
	}
	if !fullOK {
		health = runtimeHealthObservation{State: "unavailable", Message: "Deployment verification unavailable; ownership retained", Admission: "unknown"}
		if verifyErr == nil && full && out.State != "running" {
			health.Message = "Deployment is not running; ownership retained"
		}
	}
	var save *runtimeExecution
	if full && fullOK {
		save = &out
	}
	if health.Recovery != "" {
		recoveryFailures++
	} else {
		recoveryFailures = 0
	}
	stored, err := m.persistRuntimeObservation(d, health, save)
	if stored && err == nil && recoveryFailures >= 2 {
		m.maybeRecoverRuntime(d, health)
	}
	now := time.Now()
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if m.monitors[id] != entry {
		return
	}
	entry.NextHealth = now.Add(runtimeHealthInterval)
	if !stored || err != nil {
		entry.FullOK = false
		entry.NextFull = now.Add(runtimeInspectionRetryInterval)
		return
	}
	entry.Health, entry.HealthChecked = health, now
	entry.ProbeHealthy = probeHealthy
	entry.HealthFailures = failures
	entry.RecoveryFailures = recoveryFailures
	if full {
		entry.FullChecked = verifiedAt
		entry.FullOK = fullOK
		entry.Execution = out
		entry.NextFull = time.Time{}
		if audit := runtimeInspectionAuditInterval(); audit > 0 {
			entry.NextFull = now.Add(audit)
		}
		if !fullOK {
			entry.NextFull = now.Add(runtimeInspectionRetryInterval)
		}
		if d.Placement.local() {
			entry.NextFull = entry.NextHealth
		}
		if save != nil {
			entry.Deployment.Execution = out
			entry.Identity = runtimeMonitorIdentity(entry.Deployment)
		}
	}
}
func runtimeVerifiedObservation(d runtimeDeployment, out runtimeExecution) (runtimeExecution, error) {
	if !d.Placement.local() && (out.Handle == "" || out.HostKey == "" || out.ReleaseDigest == "") {
		return out, fmt.Errorf("backend inspection omitted execution identity")
	}
	if out.Platform != "" && d.Execution.Platform != "" && out.Platform != d.Execution.Platform {
		return out, fmt.Errorf("execution platform changed")
	}
	if out.StateRef != "" && d.Execution.StateRef != "" && out.StateRef != d.Execution.StateRef {
		return out, fmt.Errorf("execution state ownership changed")
	}
	merged, err := runtimeMergeExecution(d.Execution, out)
	if err == nil && out.State == "running" {
		err = runtimeSecureURL(merged.Endpoint)
	}
	return merged, err
}

// The existing private control endpoint/tunnel is used directly; no host API,
// SSH, public hostname, or new app/backend route is needed for cheap probes.
func (m *appRuntimeManager) runtimeAppHealth(d runtimeDeployment, out runtimeExecution) runtimeHealthObservation {
	return m.runtimeAppHealthContext(m.ctx, d, out)
}
func (m *appRuntimeManager) runtimeAppHealthContext(parent context.Context, d runtimeDeployment, out runtimeExecution) runtimeHealthObservation {
	failed := runtimeHealthObservation{State: "unavailable", Message: "App health check failed; ownership retained", Admission: "unknown"}
	if d.Manifest.Runtime.Kind == "static" && d.Placement.local() {
		return runtimeHealthObservation{State: "running", Admission: "unknown", Healthy: true}
	}
	if runtimeSecureURL(out.Endpoint) != nil {
		return failed
	}
	token, err := m.token(d)
	if err != nil {
		return failed
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	path := d.Manifest.Runtime.HealthCheck
	if path == "" {
		path = "/health"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(out.Endpoint, path), nil)
	if err != nil {
		return failed
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return failed
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	live := resp.StatusCode == http.StatusOK
	if !live && resp.StatusCode != http.StatusServiceUnavailable {
		return failed
	}
	result := runtimeHealthObservation{State: "running", Message: "Process running; app admission is not reported", Admission: "unknown", Healthy: true}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(out.Endpoint, runtimeControlPrefix+"app/status"), nil)
	if err != nil {
		return result
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = m.http.Do(req)
	if err != nil {
		return failed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		if !live {
			return failed
		}
		return result
	}
	if resp.StatusCode != http.StatusOK {
		return failed
	}
	var status struct {
		State        string   `json:"state"`
		DeploymentID string   `json:"deployment_id"`
		Generation   *int64   `json:"generation"`
		Load         *float64 `json:"load"`
		Admitting    *bool    `json:"admitting"`
		Recovery     struct {
			Action string `json:"action"`
		} `json:"recovery"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&status) != nil {
		return failed
	}
	if (status.DeploymentID != "" && status.DeploymentID != d.ID) || (status.Generation != nil && *status.Generation != d.Generation) {
		failed.Message = "App status identity does not match the active deployment; ownership retained"
		return failed
	}
	result.IdentityVerified = status.DeploymentID == d.ID && status.Generation != nil && *status.Generation == d.Generation
	if status.Admitting == nil {
		if !live {
			return failed
		}
		return result
	}
	if *status.Admitting && (status.State == "standby" || status.State == "fenced" || status.State == "draining" || status.State == "inactive") {
		failed.Message = "App admission status is inconsistent; ownership retained"
		return failed
	}
	if *status.Admitting && status.DeploymentID == d.ID && status.Generation != nil && *status.Generation == d.Generation && status.Load != nil && *status.Load >= 0 && !math.IsNaN(*status.Load) && !math.IsInf(*status.Load, 0) {
		result.Load = status.Load
	}
	result.Admitting = *status.Admitting
	result.Admission = "active"
	result.State = "healthy"
	result.Message = ""
	if !*status.Admitting {
		result.Admission = "inactive"
		result.State = "degraded"
		result.Message = "App is not admitting work"
		result.Healthy = false
	}
	if status.State != "" && len(status.State) <= 64 && !strings.ContainsAny(status.State, "\r\n\t") {
		result.Admission = status.State
	}
	if !*status.Admitting {
		result.Message += ": " + result.Admission
		if status.State == "standby" && result.IdentityVerified {
			result.Recovery = "activate"
		}
		if status.State == "fenced" && status.Recovery.Action == "restart" && status.DeploymentID == d.ID && status.Generation != nil && *status.Generation == d.Generation {
			result.Recovery = "restart"
		}
	} else if !live {
		return failed
	}
	return result
}

// Guard every publication/update by the current generation and absence of an
// in-progress lifecycle operation. A slow old probe cannot revive old routing.
const runtimeMonitorGuard = `d.id=? AND d.generation=? AND d.status='active' AND i.runtime_managed=1 AND i.runtime_generation=d.generation AND i.status='running' AND NOT EXISTS(SELECT 1 FROM app_runtime_operations o WHERE o.install_id=i.id AND o.status IN ('queued','running'))`

func (m *appRuntimeManager) runtimeMonitorCurrent(d runtimeDeployment) bool {
	var n int
	err := m.s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_deployments d JOIN app_installs i ON i.id=d.install_id WHERE `+runtimeMonitorGuard, d.ID, d.Generation).Scan(&n)
	return err == nil && n == 1
}
func (m *appRuntimeManager) persistRuntimeObservation(d runtimeDeployment, health runtimeHealthObservation, out *runtimeExecution) (bool, error) {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO app_runtime_observations(deployment_id,checked_at,state,message,admission_state) SELECT d.id,?,?,?,? FROM app_runtime_deployments d JOIN app_installs i ON i.id=d.install_id WHERE `+runtimeMonitorGuard+` ON CONFLICT(deployment_id) DO UPDATE SET checked_at=excluded.checked_at,state=excluded.state,message=excluded.message,admission_state=excluded.admission_state`, runtimeTimestamp(), health.State, health.Message, health.Admission, d.ID, d.Generation)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return false, nil
	}
	if out != nil {
		_, err = tx.Exec(`UPDATE app_runtime_deployments SET record_json=json_set(record_json,'$.execution',json(?)) WHERE id=? AND generation=? AND status='active'`, runtimeJSON(out), d.ID, d.Generation)
		if err != nil {
			return false, err
		}
	}
	// Clear only the old reconciler's marker. Current monitoring messages live
	// in one observation record; lifecycle/operator messages remain untouched.
	_, err = tx.Exec(`UPDATE app_installs SET status_message='' WHERE id=? AND runtime_active_deployment=? AND status_message='Deployment control unavailable; ownership retained'`, d.InstallID, d.ID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (m *appRuntimeManager) recentRuntimeExecution(d runtimeDeployment, now time.Time) (runtimeExecution, bool) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	e := m.monitors[d.ID]
	if e == nil || e.Identity != runtimeMonitorIdentity(d) || e.InFlight || !e.FullOK || !e.Health.Healthy || now.Sub(e.HealthChecked) > runtimeObservationMaxAge {
		return runtimeExecution{}, false
	}
	// Host inspection's workload is deliberately ignored. It ages independently
	// of app health and must never force another host inspection just to scale.
	if e.Health.Load == nil {
		return runtimeExecution{}, false
	}
	out := e.Execution
	out.Load = e.Health.Load
	return out, true
}

// Serialize full inspections across background checks and lifecycle callers.
// Lifecycle callers always make a fresh request; they never consume the cache.
type runtimeInspectionLock struct {
	token chan struct{}
	users int
}

func (m *appRuntimeManager) lockRuntimeInspection(ctx context.Context, id string) (func(), error) {
	m.monitorMu.Lock()
	if m.inspectionLocks == nil {
		m.inspectionLocks = map[string]*runtimeInspectionLock{}
	}
	lock := m.inspectionLocks[id]
	if lock == nil {
		lock = &runtimeInspectionLock{token: make(chan struct{}, 1)}
		m.inspectionLocks[id] = lock
	}
	lock.users++
	m.monitorMu.Unlock()
	releaseUser := func() {
		m.monitorMu.Lock()
		defer m.monitorMu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(m.inspectionLocks, id)
		}
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; releaseUser() }, nil
	case <-ctx.Done():
		releaseUser()
		return nil, ctx.Err()
	}
}
