package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
)

// appSkillReconcileStats describes the changes made while bringing one
// agent's app-owned skill memories in line with its enabled app bindings.
// Skills are agent-scoped: Core shares the agent memory store with main and
// every existing or future child thread, so no per-thread copies are needed.
type appSkillReconcileStats struct {
	Added   int
	Updated int
	Removed int
}

func (s *Server) lockAgentSkills(agentID int64) func() {
	value, _ := s.agentSkillLocks.LoadOrStore(agentID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// reconcileAgentAppSkills makes app_agent_bindings authoritative for the
// app-owned skills assigned to an agent. Every enabled skill shipped by an
// attached app is materialized in the agent's shared memory; app skills whose
// app is no longer attached (or whose catalog row is disabled/removed) are
// tombstoned. User and builtin skills are never touched.
func (s *Server) reconcileAgentAppSkills(inst *Agent) (appSkillReconcileStats, error) {
	var stats appSkillReconcileStats
	if inst == nil {
		return stats, errors.New("agent required")
	}
	unlock := s.lockAgentSkills(inst.ID)
	defer unlock()

	rows, err := s.store.db.Query(`
		SELECT install_id
		FROM app_agent_bindings
		WHERE agent_id=? AND enabled=1`, inst.ID)
	if err != nil {
		return stats, fmt.Errorf("list app bindings: %w", err)
	}
	bound := map[int64]bool{}
	for rows.Next() {
		var installID int64
		if err := rows.Scan(&installID); err != nil {
			_ = rows.Close()
			return stats, fmt.Errorf("scan app binding: %w", err)
		}
		bound[installID] = true
	}
	if err := rows.Close(); err != nil {
		return stats, fmt.Errorf("close app bindings: %w", err)
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("iterate app bindings: %w", err)
	}

	catalog, err := s.listProjectSkills(inst.ProjectID)
	if err != nil {
		return stats, fmt.Errorf("list app skills: %w", err)
	}
	desired := make(map[string]Skill)
	for _, sk := range catalog {
		if sk.Source != "app" || sk.InstallID == nil || !bound[*sk.InstallID] {
			continue
		}
		desired[sk.Slug] = sk
	}

	journalPath := filepath.Join(s.agents.instanceDir(inst.ID), "memory.jsonl")
	active, err := journalActiveSkillRecords(journalPath)
	if err != nil {
		return stats, fmt.Errorf("read agent skill memories: %w", err)
	}

	desiredSlugs := make([]string, 0, len(desired))
	for slug := range desired {
		desiredSlugs = append(desiredSlugs, slug)
	}
	sort.Strings(desiredSlugs)
	var reconcileErrors []error
	for _, slug := range desiredSlugs {
		sk := desired[slug]
		rec, exists := active[slug]
		if exists && extractTag(rec.Tags, SkillSourceTagPrefix) == "app" &&
			extractTag(rec.Tags, SkillHashTagPrefix) == skillBodyHash(sk.Body) {
			continue
		}
		if err := s.PushSkillToInstance(inst.ID, sk); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("push %s: %w", slug, err))
			continue
		}
		if exists {
			stats.Updated++
		} else {
			stats.Added++
		}
	}

	activeSlugs := make([]string, 0, len(active))
	for slug := range active {
		activeSlugs = append(activeSlugs, slug)
	}
	sort.Strings(activeSlugs)
	for _, slug := range activeSlugs {
		rec := active[slug]
		if extractTag(rec.Tags, SkillSourceTagPrefix) != "app" {
			continue
		}
		if _, keep := desired[slug]; keep {
			continue
		}
		if err := s.removeSkillMemoryRecord(inst.ID, rec.ID, "app detached or app skill disabled"); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("remove %s: %w", slug, err))
			continue
		}
		stats.Removed++
	}

	if stats.Added > 0 || stats.Updated > 0 || stats.Removed > 0 {
		log.Printf("[APP-SKILLS] reconciled agent=%d added=%d updated=%d removed=%d",
			inst.ID, stats.Added, stats.Updated, stats.Removed)
	}
	return stats, errors.Join(reconcileErrors...)
}

func (s *Server) removeSkillMemoryRecord(agentID int64, memoryID, reason string) error {
	if memoryID == "" {
		return nil
	}
	if s.agents.IsRunning(agentID) {
		return s.deleteMemoryHTTP(agentID, memoryID, reason)
	}
	return s.tombstoneOnDisk(agentID, memoryID, reason)
}

// reconcileAppSkillsForInstall repairs every agent currently bound to one app.
// This is used after app skill registration/upgrades so a newly introduced
// skill reaches agents that were already attached before that skill existed.
func (s *Server) reconcileAppSkillsForInstall(installID int64) error {
	rows, err := s.store.db.Query(`
		SELECT a.id
		FROM agents a
		JOIN app_agent_bindings b ON b.agent_id=a.id
		WHERE b.install_id=? AND b.enabled=1
		ORDER BY a.id`, installID)
	if err != nil {
		return fmt.Errorf("list bound agents: %w", err)
	}
	var agentIDs []int64
	for rows.Next() {
		var agentID int64
		if err := rows.Scan(&agentID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan bound agent: %w", err)
		}
		agentIDs = append(agentIDs, agentID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close bound agents: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate bound agents: %w", err)
	}

	var reconcileErrors []error
	for _, agentID := range agentIDs {
		inst, err := s.store.GetAgentByID(agentID)
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("load agent %d: %w", agentID, err))
			continue
		}
		if _, err := s.reconcileAgentAppSkills(inst); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("agent %d: %w", agentID, err))
		}
	}
	return errors.Join(reconcileErrors...)
}
