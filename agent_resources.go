package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"strconv"
	"time"
)

func (s *Store) migrateAgentResources() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS agent_resource_policies(agent_id INTEGER PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,revision INTEGER NOT NULL,policy_json TEXT NOT NULL)`)
	return err
}
func (s *Store) agentResourcePolicy(id int64) (runtimeResourceRecord, error) {
	var r runtimeResourceRecord
	var raw string
	err := s.db.QueryRow(`SELECT revision,policy_json FROM agent_resource_policies WHERE agent_id=?`, id).Scan(&r.Revision, &raw)
	if err == sql.ErrNoRows {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(raw), &r.Policy)
	return r, err
}
func validateAgentResources(p sdk.ResourcePolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// Reuse the shared policy contract, exposing only process CPU weight for
	// agents in this increment. Never accept cooperative/socket settings that
	// Core has not implemented, or permit ordinary config updates to set policy.
	if p.Process != (sdk.ProcessResources{CPUWeight: p.Process.CPUWeight}) || p.TotalConcurrency != 0 || len(p.Workloads) != 0 || len(p.Network) != 0 {
		return fmt.Errorf("agents currently support process.cpu_weight only")
	}
	return validateHostResourcePolicy(p)
}
func (s *Store) saveAgentResources(id int64, next runtimeResourceRecord) error {
	if next.Revision < 0 {
		return fmt.Errorf("invalid revision")
	}
	result, err := s.db.Exec(`INSERT INTO agent_resource_policies(agent_id,revision,policy_json) SELECT ?,1,? WHERE ?=0 OR EXISTS(SELECT 1 FROM agent_resource_policies WHERE agent_id=?) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1,policy_json=excluded.policy_json WHERE revision=?`, id, runtimeJSON(next.Policy), next.Revision, id, next.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("resource policy changed; reload before saving")
	}
	return nil
}
func (s *Server) handleAgentResources(w http.ResponseWriter, r *http.Request, id int64) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	agent, ok := s.requireAgentAccess(w, r, id, ProjectViewer)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.Method == http.MethodPut {
		var next runtimeResourceRecord
		if runtimeDecode(r, &next) != nil {
			http.Error(w, "invalid resource policy", 400)
			return
		}
		if err := validateAgentResources(next.Policy); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := validateLocalProcessResources(resolveProcessResources(next.Policy.Process)); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.saveAgentResources(id, next); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	}
	current, err := s.store.agentResourcePolicy(id)
	if err != nil {
		http.Error(w, "resource policy unavailable", 500)
		return
	}
	resolved := sdk.ResourcePolicy{Process: resolveProcessResources(current.Policy.Process)}
	observed := map[string]string{}
	var running bool
	if s.agents != nil {
		s.agents.mu.RLock()
		ri := s.agents.processes[id]
		s.agents.mu.RUnlock()
		if ri != nil && ri.isRunning() {
			running = true
			observed = ri.processResources(id)
		}
	}
	state := "stopped"
	if agent.Status == "running" {
		state = "unavailable"
	}
	if running {
		state = "unavailable"
		if observed["scope"] == "dedicated" {
			state = "applied"
			want := resolved.Process.CPUWeight
			if want == 0 {
				want = 100
			}
			if observed["cpu.weight"] != strconv.Itoa(want) {
				state = "pending_restart"
			}
		} else if len(observed) > 0 {
			state = "inherited_shared"
		}
	}
	if observed == nil {
		observed = map[string]string{}
	}
	caps := localResourceCapabilities()
	supported := false
	for _, control := range caps.ProcessControls {
		if control == "cpu_weight" {
			supported = true
		}
	}
	if !supported {
		state = "unsupported"
	} else {
		caps.ProcessControls = []string{"cpu_weight"}
		caps.ProcessApply = "next_restart"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"revision": current.Revision, "policy": current.Policy, "resolved": resolved, "apply": "next_restart", "state": state, "supported": supported, "capabilities": caps, "observed": observed, "note": "Saving does not restart this agent. CPU weight applies to the whole Core process and its subprocesses. Existing ancestor limits still apply."})
}
func (r *runningAgent) processResources(id int64) map[string]string {
	if r.resourceIdentity == nil {
		return nil
	}
	before, err := readRuntimeProcessIdentity(r.pid)
	if err != nil || !r.resourceIdentity.sameProcess(before) {
		return nil
	}
	observed := inspectProcessResources(r.pid, processResourceOwner{"agent", strconv.FormatInt(id, 10)})
	after, err := readRuntimeProcessIdentity(r.pid)
	if err != nil || !before.sameProcess(after) {
		return nil
	}
	return observed
}

func (r *runningAgent) watchResourceExit() {
	if r.resourceIdentity == nil || r.releaseResources == nil {
		return
	}
	watch, err := watchRuntimeProcessExit(r.pid)
	if err != nil {
		return
	}
	current, err := readRuntimeProcessIdentity(r.pid)
	if err != nil || !r.resourceIdentity.sameProcess(current) {
		watch.close()
		return
	}
	go func() {
		defer watch.close()
		for {
			exited, err := watch.wait(30 * time.Second)
			if err != nil {
				return
			}
			if exited {
				r.resourceReleaseOnce.Do(r.releaseResources)
				return
			}
		}
	}()
}
