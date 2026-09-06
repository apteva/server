package main

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// fixedPortKey is the host-level resource that must be owned by exactly one
// local app installation. TCP and UDP have independent port namespaces.
type fixedPortKey struct {
	protocol string
	port     int
}

func (k fixedPortKey) String() string {
	return strings.ToUpper(k.protocol) + " port " + fmt.Sprint(k.port)
}

type fixedRuntimePort struct {
	name          string
	protocol      string
	containerPort int
	hostPort      int
}

func (p fixedRuntimePort) key() fixedPortKey {
	return fixedPortKey{protocol: p.protocol, port: p.hostPort}
}

type fixedPortReservation struct {
	installID int64
	appName   string
}

// activationSpec is deliberately complete enough to restart a stopped old
// sidecar. Fixed-port upgrades cannot keep OLD alive during NEW's start, so a
// verified rollback needs more than the pid and primary HTTP port previously
// stored in localProc.
type activationSpec struct {
	startupTimeout  time.Duration
	databaseUpgrade string
	installID       int64
	appName         string
	binPath         string
	httpPort        int
	env             map[string]string
	healthPath      string
	probeHost       string
	fixedPorts      []fixedRuntimePort
}

func newActivationSpec(installID int64, m *sdk.Manifest, binPath string, httpPort int, env map[string]string) (activationSpec, error) {
	if m == nil {
		return activationSpec{}, errors.New("activation manifest required")
	}
	if m.Runtime.StartupTimeoutSeconds < 0 || m.Runtime.StartupTimeoutSeconds > 3600 {
		return activationSpec{}, errors.New("invalid startup timeout")
	}
	ports, err := fixedRuntimePorts(m)
	if err != nil {
		return activationSpec{}, err
	}
	healthPath := strings.TrimSpace(m.Runtime.HealthCheck)
	if healthPath == "" {
		healthPath = "/health"
	}
	probeHost := strings.TrimSpace(m.Runtime.BindHost)
	if probeHost == "" || probeHost == "0.0.0.0" || probeHost == "::" || probeHost == "[::]" {
		probeHost = "127.0.0.1"
	}
	return activationSpec{
		startupTimeout:  time.Duration(m.Runtime.StartupTimeoutSeconds) * time.Second,
		databaseUpgrade: m.Runtime.DatabaseUpgrade,
		installID:       installID,
		appName:         m.Name,
		binPath:         binPath,
		httpPort:        httpPort,
		env:             cloneStringMap(env),
		healthPath:      healthPath,
		probeHost:       probeHost,
		fixedPorts:      ports,
	}, nil
}

func (s activationSpec) clone() activationSpec {
	s.env = cloneStringMap(s.env)
	s.fixedPorts = append([]fixedRuntimePort(nil), s.fixedPorts...)
	return s
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func fixedRuntimePorts(m *sdk.Manifest) ([]fixedRuntimePort, error) {
	if m == nil {
		return nil, nil
	}
	out := make([]fixedRuntimePort, 0, len(m.Runtime.Ports))
	seen := make(map[fixedPortKey]string)
	for _, declared := range m.Runtime.Ports {
		if declared.HostPort <= 0 {
			continue
		}
		protocol := strings.ToLower(strings.TrimSpace(declared.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" {
			return nil, fmt.Errorf("runtime port %q has unsupported protocol %q", declared.Name, declared.Protocol)
		}
		p := fixedRuntimePort{
			name:          strings.TrimSpace(declared.Name),
			protocol:      protocol,
			containerPort: declared.ContainerPort,
			hostPort:      declared.HostPort,
		}
		key := p.key()
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("%s is declared by both runtime ports %q and %q", key, previous, p.name)
		}
		seen[key] = p.name
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].protocol == out[j].protocol {
			return out[i].hostPort < out[j].hostPort
		}
		return out[i].protocol < out[j].protocol
	})
	return out, nil
}

func fixedPortSet(ports []fixedRuntimePort) map[fixedPortKey]struct{} {
	out := make(map[fixedPortKey]struct{}, len(ports))
	for _, p := range ports {
		out[p.key()] = struct{}{}
	}
	return out
}

// fixedPortLease reserves NEW's ports while retaining OLD's committed set.
// commit replaces the committed set; rollback removes new-only reservations.
// This makes upgrades that add/remove ports atomic from the supervisor's view.
type fixedPortLease struct {
	sup       *LocalSupervisor
	installID int64
	previous  map[fixedPortKey]struct{}
	desired   map[fixedPortKey]struct{}
	finished  bool
	mu        sync.Mutex
}

func (sup *LocalSupervisor) reserveActivationPorts(spec activationSpec) (*fixedPortLease, error) {
	desired := fixedPortSet(spec.fixedPorts)
	sup.portMu.Lock()
	defer sup.portMu.Unlock()

	previous := cloneFixedPortSet(sup.installFixedPorts[spec.installID])
	for key := range desired {
		if owner, exists := sup.fixedPorts[key]; exists && owner.installID != spec.installID {
			return nil, fmt.Errorf("%s is already reserved by %s installation %d", key, owner.appName, owner.installID)
		}
	}
	for key := range desired {
		sup.fixedPorts[key] = fixedPortReservation{installID: spec.installID, appName: spec.appName}
	}
	return &fixedPortLease{sup: sup, installID: spec.installID, previous: previous, desired: desired}, nil
}

func (l *fixedPortLease) commit() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finished {
		return
	}
	l.sup.portMu.Lock()
	for key := range l.previous {
		if _, keep := l.desired[key]; keep {
			continue
		}
		if owner, ok := l.sup.fixedPorts[key]; ok && owner.installID == l.installID {
			delete(l.sup.fixedPorts, key)
		}
	}
	if len(l.desired) == 0 {
		delete(l.sup.installFixedPorts, l.installID)
	} else {
		l.sup.installFixedPorts[l.installID] = cloneFixedPortSet(l.desired)
	}
	l.sup.portMu.Unlock()
	l.finished = true
}

func (l *fixedPortLease) rollback() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finished {
		return
	}
	l.sup.portMu.Lock()
	for key := range l.desired {
		if _, existed := l.previous[key]; existed {
			continue
		}
		if owner, ok := l.sup.fixedPorts[key]; ok && owner.installID == l.installID {
			delete(l.sup.fixedPorts, key)
		}
	}
	if len(l.previous) == 0 {
		delete(l.sup.installFixedPorts, l.installID)
	} else {
		l.sup.installFixedPorts[l.installID] = cloneFixedPortSet(l.previous)
	}
	l.sup.portMu.Unlock()
	l.finished = true
}

func cloneFixedPortSet(in map[fixedPortKey]struct{}) map[fixedPortKey]struct{} {
	out := make(map[fixedPortKey]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

// RestoreCommittedFixedPorts reconstructs reservation ownership from the
// persisted manifest before boot resume starts any parallel workers.
func (sup *LocalSupervisor) RestoreCommittedFixedPorts(installID int64, appName string, ports []fixedRuntimePort) error {
	return sup.restoreFixedPorts(installID, appName, ports, true)
}

// RestorePendingFixedPorts additionally holds the target upgrade ports while
// keeping installFixedPorts pointed at the currently active manifest.
func (sup *LocalSupervisor) RestorePendingFixedPorts(installID int64, appName string, ports []fixedRuntimePort) error {
	return sup.restoreFixedPorts(installID, appName, ports, false)
}

func (sup *LocalSupervisor) restoreFixedPorts(installID int64, appName string, ports []fixedRuntimePort, committed bool) error {
	desired := fixedPortSet(ports)
	sup.portMu.Lock()
	defer sup.portMu.Unlock()
	for key := range desired {
		if owner, exists := sup.fixedPorts[key]; exists && owner.installID != installID {
			return fmt.Errorf("%s is already reserved by %s installation %d", key, owner.appName, owner.installID)
		}
	}
	for key := range desired {
		sup.fixedPorts[key] = fixedPortReservation{installID: installID, appName: appName}
	}
	if committed {
		if len(desired) == 0 {
			delete(sup.installFixedPorts, installID)
		} else {
			sup.installFixedPorts[installID] = desired
		}
	}
	return nil
}

func (sup *LocalSupervisor) ResetFixedPortReservations() {
	sup.portMu.Lock()
	sup.fixedPorts = make(map[fixedPortKey]fixedPortReservation)
	sup.installFixedPorts = make(map[int64]map[fixedPortKey]struct{})
	sup.portMu.Unlock()
}

// ReleaseFixedPorts is intentionally separate from Stop. Stop is also used by
// upgrades and configuration respawns, where the installation must retain its
// ownership while temporarily not running. Uninstall and failed fresh installs
// are the callers that release ownership.
func (sup *LocalSupervisor) ReleaseFixedPorts(installID int64) {
	sup.portMu.Lock()
	for key, owner := range sup.fixedPorts {
		if owner.installID == installID {
			delete(sup.fixedPorts, key)
		}
	}
	delete(sup.installFixedPorts, installID)
	sup.portMu.Unlock()
}

// DiscardPendingFixedPorts removes target-only reservations while retaining
// the committed manifest's ownership. It is idempotent and covers failures in
// clone/download/compile that occur before activate creates its own lease.
func (sup *LocalSupervisor) DiscardPendingFixedPorts(installID int64) {
	sup.portMu.Lock()
	committed := sup.installFixedPorts[installID]
	for key, owner := range sup.fixedPorts {
		if owner.installID != installID {
			continue
		}
		if _, keep := committed[key]; !keep {
			delete(sup.fixedPorts, key)
		}
	}
	sup.portMu.Unlock()
}

type activationError struct {
	cause              error
	rollbackVerified   bool
	rollbackFailureErr error
}

func (e *activationError) Error() string {
	if e.rollbackFailureErr != nil {
		return fmt.Sprintf("%v; previous version restart failed: %v; committed database changes were not restored", e.cause, e.rollbackFailureErr)
	}
	return e.cause.Error() + "; binary fallback does not restore committed database changes"
}

func (e *activationError) Unwrap() error { return e.cause }

func activationRollbackVerified(err error) bool {
	var activationErr *activationError
	return errors.As(err, &activationErr) && activationErr.rollbackVerified
}

func requiresExclusiveActivation(old *localProc, next activationSpec) bool {
	return len(next.fixedPorts) > 0 || (old != nil && len(old.spec.fixedPorts) > 0)
}

// activate keeps ordinary HTTP-only sidecars on the existing blue-green path.
// Any old or new fixed host port makes the activation exclusive: OLD is stopped
// only after NEW is fully prepared, then NEW is started and verified. Failure
// restarts and verifies OLD before the caller is allowed to report rollback.
func (sup *LocalSupervisor) activate(next activationSpec, deadline time.Duration) error {
	old := sup.currentProc(next.installID)
	if next.databaseUpgrade == "requires_restore" {
		return &activationError{cause: errors.New("automatic activation blocked: app requires an offline database upgrade and tested restore procedure"), rollbackVerified: old != nil && sup.verifyCurrentProc(next.installID, 5*time.Second)}
	}
	if next.startupTimeout > 0 {
		deadline = next.startupTimeout
	}
	lease, err := sup.reserveActivationPorts(next)
	if err != nil {
		verified := old != nil && sup.waitReady(old.spec, 5*time.Second) == nil
		return &activationError{cause: err, rollbackVerified: verified}
	}

	exclusive := requiresExclusiveActivation(old, next)
	if exclusive && old != nil {
		old = sup.takeCurrentProc(next.installID)
		if old != nil {
			terminateProc(old, 5*time.Second)
		}
	}

	if err := sup.spawn(next); err != nil {
		if exclusive {
			return sup.rollbackExclusiveActivation(old, lease, err, deadline)
		}
		lease.rollback()
		verified := sup.verifyCurrentProc(next.installID, 5*time.Second)
		return &activationError{cause: err, rollbackVerified: verified}
	}

	if err := sup.waitReady(next, deadline); err != nil {
		_ = sup.Stop(next.installID)
		if exclusive {
			return sup.rollbackExclusiveActivation(old, lease, err, deadline)
		}
		restored := sup.rollbackToOld(next.installID)
		lease.rollback()
		verified := restored != nil && sup.waitReady(restored.spec, 5*time.Second) == nil
		return &activationError{cause: err, rollbackVerified: verified}
	}

	lease.commit()
	return nil
}

func (sup *LocalSupervisor) rollbackExclusiveActivation(old *localProc, lease *fixedPortLease, cause error, deadline time.Duration) error {
	lease.rollback()
	if old == nil {
		return &activationError{cause: cause}
	}
	oldSpec := old.spec.clone()
	if err := sup.spawn(oldSpec); err != nil {
		return &activationError{cause: cause, rollbackFailureErr: err}
	}
	if err := sup.waitReady(oldSpec, deadline); err != nil {
		_ = sup.Stop(oldSpec.installID)
		return &activationError{cause: cause, rollbackFailureErr: err}
	}
	return &activationError{cause: cause, rollbackVerified: true}
}

func (sup *LocalSupervisor) currentProc(installID int64) *localProc {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.procs[installID]
}

func (sup *LocalSupervisor) takeCurrentProc(installID int64) *localProc {
	sup.mu.Lock()
	p := sup.procs[installID]
	delete(sup.procs, installID)
	sup.mu.Unlock()
	return p
}

func (sup *LocalSupervisor) verifyCurrentProc(installID int64, deadline time.Duration) bool {
	p := sup.currentProc(installID)
	return p != nil && sup.waitReady(p.spec, deadline) == nil
}

func (sup *LocalSupervisor) waitReady(spec activationSpec, deadline time.Duration) error {
	started := time.Now()
	if err := sup.waitHealthy(spec.installID, spec.httpPort, spec.healthPath, deadline); err != nil {
		return err
	}
	remaining := deadline - time.Since(started)
	if remaining <= 0 && len(spec.fixedPorts) > 0 {
		return fmt.Errorf("startup deadline expired before fixed-port readiness")
	}
	return waitFixedTCPPorts(spec.probeHost, spec.fixedPorts, remaining)
}

func waitFixedTCPPorts(host string, ports []fixedRuntimePort, deadline time.Duration) error {
	tcpPorts := make([]fixedRuntimePort, 0, len(ports))
	for _, port := range ports {
		if port.protocol == "tcp" {
			tcpPorts = append(tcpPorts, port)
		}
	}
	if len(tcpPorts) == 0 {
		return nil
	}
	end := time.Now().Add(deadline)
	for _, port := range tcpPorts {
		address := net.JoinHostPort(host, fmt.Sprint(port.hostPort))
		for {
			remaining := time.Until(end)
			if remaining <= 0 {
				return fmt.Errorf("fixed TCP port %d (%s) did not accept connections within %s", port.hostPort, port.name, deadline)
			}
			timeout := 250 * time.Millisecond
			if remaining < timeout {
				timeout = remaining
			}
			conn, err := net.DialTimeout("tcp", address, timeout)
			if err == nil {
				_ = conn.Close()
				break
			}
			time.Sleep(min(100*time.Millisecond, max(0, time.Until(end))))
		}
	}
	return nil
}
