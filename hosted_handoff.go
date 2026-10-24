package main

import (
	"fmt"
)

// Work started after an HTTP 202 must finish before the controller retires.
// Admission and the handoff freeze share this lock; no polling race can start
// another deployment between the busy check and ownership release.
func (s *Server) beginHostedWork() (func(), error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	if s.handoffFrozen {
		return nil, fmt.Errorf("platform handoff is in progress")
	}
	s.handoffWork++
	return func() { s.handoffMu.Lock(); s.handoffWork--; s.handoffMu.Unlock() }, nil
}

func (s *Server) unfreezeHostedWork() {
	s.handoffMu.Lock()
	s.handoffFrozen = false
	s.handoffMu.Unlock()
	if s.localApps != nil {
		s.localApps.inflightG.Lock()
		s.localApps.handoff = false
		s.localApps.inflightG.Unlock()
	}
	if s.agentRollouts != nil {
		s.agentRollouts.mu.Lock()
		s.agentRollouts.handoff = false
		s.agentRollouts.mu.Unlock()
	}
}

func (s *Server) freezeHostedWork() (err error) {
	s.handoffMu.Lock()
	if s.handoffFrozen || s.handoffWork != 0 {
		s.handoffMu.Unlock()
		return fmt.Errorf("app operations are still in progress")
	}
	s.handoffFrozen = true
	s.handoffMu.Unlock()
	defer func() {
		if err != nil {
			s.unfreezeHostedWork()
		}
	}()
	if s.localApps != nil {
		s.localApps.inflightG.Lock()
		busy := len(s.localApps.inflight) > 0
		s.localApps.handoff = true
		s.localApps.inflightG.Unlock()
		if busy {
			return fmt.Errorf("app installs are still in progress")
		}
	}
	if s.agentRollouts != nil {
		s.agentRollouts.mu.Lock()
		busy := s.agentRollouts.status.State == "running"
		s.agentRollouts.handoff = true
		s.agentRollouts.mu.Unlock()
		if busy {
			return fmt.Errorf("a core rollout is still in progress")
		}
	}
	// Durable pending operations include work owned by a previous controller.
	// Query outside supervisor locks to avoid lock inversion with workers.
	var pending int
	err = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE status IN ('queued','running')`).Scan(&pending)
	if err != nil {
		return err
	}
	if pending != 0 {
		return fmt.Errorf("app transfers or deployments are still pending")
	}
	return nil
}

// A server handoff adopts every live core, including platform helpers. Boot
// resume policy controls starting stopped processes, never adopting live work.
// Failure leaves the original PID, key and durable records available to rollback.
func (s *Server) adoptHostedAgents() error {
	rows, err := s.store.ListAgentsByStatus("running")
	if err != nil {
		return err
	}
	for i := range rows {
		inst := &rows[i]
		if s.agents.IsRunning(inst.ID) {
			continue
		}
		if _, err := s.reattachManagedAgentMode(inst, true, s.loadChannelConfigs(inst.ID)...); err != nil {
			return fmt.Errorf("agent %d (%s): %w", inst.ID, inst.Kind, err)
		}
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
	}
	return nil
}

func (s *Server) hostedRuntimePIDs() (map[string]int, error) {
	pids := map[string]int{}
	s.agents.mu.RLock()
	for id, p := range s.agents.processes {
		pids[fmt.Sprintf("agent:%d", id)] = p.processID()
	}
	s.agents.mu.RUnlock()
	// Read durable identities for managed local deployments. Their controller
	// already inspects/stops recorded PIDs without the legacy supervisor map.
	rows, err := s.store.db.Query(`SELECT id,local_pid,local_bin_path FROM app_installs WHERE status='running' AND local_bin_path!='' AND runtime_managed=0`)
	if err != nil {
		return nil, err
	}
	type process struct {
		id  int64
		pid int
		bin string
	}
	var apps []process
	for rows.Next() {
		var p process
		if err = rows.Scan(&p.id, &p.pid, &p.bin); err != nil {
			break
		}
		apps = append(apps, p)
	}
	scanErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	for _, p := range apps {
		if p.pid < 1 || !processAlive(p.pid) || !runtimeProcessMatches(p.pid, p.bin) {
			return nil, fmt.Errorf("app %d process identity cannot be verified", p.id)
		}
		if s.localApps == nil || s.localApps.PID(p.id) != p.pid {
			return nil, fmt.Errorf("app %d is not adopted", p.id)
		}
		pids[fmt.Sprintf("app:%d", p.id)] = p.pid
	}
	rows, err = s.store.db.Query(`SELECT id FROM app_runtime_deployments WHERE status IN ('active','ready','retiring')`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	scanErr = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	m := s.appRuntimeManager()
	for _, id := range ids {
		d, err := m.loadDeployment(id)
		if err != nil {
			return nil, err
		}
		if !d.Placement.local() || d.Manifest.Runtime.Kind == "static" {
			continue
		}
		pid, err := m.localRecordedPID(d)
		if err != nil || !processAlive(pid) || !runtimeProcessMatches(pid, d.LocalBin) {
			return nil, fmt.Errorf("managed runtime %s process identity cannot be verified", id)
		}
		pids["deployment:"+id] = pid
	}
	// Reject stale running core records as well as missing in-memory ownership.
	agents, err := s.store.ListAgentsByStatus("running")
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Pid < 1 || !processAlive(a.Pid) || pids[fmt.Sprintf("agent:%d", a.ID)] != a.Pid {
			return nil, fmt.Errorf("agent %d is not adopted", a.ID)
		}
	}
	return pids, nil
}
