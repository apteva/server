package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/apteva/server/apps/framework"
)

type projectCleanupPlan struct {
	AgentIDs       []int64
	InstallIDs     []int64
	ManagedMCPIDs  []int64
	EnvironmentIDs []string
}

func (s *Store) projectCleanupPlan(projectID string) (projectCleanupPlan, error) {
	var plan projectCleanupPlan
	queries := []struct {
		query string
		add   func(*sql.Rows) error
	}{
		{`SELECT id FROM agents WHERE project_id=?`, func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			plan.AgentIDs = append(plan.AgentIDs, id)
			return nil
		}},
		{`SELECT id FROM app_installs WHERE project_id=?`, func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			plan.InstallIDs = append(plan.InstallIDs, id)
			return nil
		}},
		{`SELECT id FROM mcp_servers WHERE project_id=? AND COALESCE(source,'')='managed'`, func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			plan.ManagedMCPIDs = append(plan.ManagedMCPIDs, id)
			return nil
		}},
		{`SELECT id FROM environments WHERE project_id=?`, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			plan.EnvironmentIDs = append(plan.EnvironmentIDs, id)
			return nil
		}},
	}
	for _, item := range queries {
		rows, err := s.db.Query(item.query, projectID)
		if err != nil {
			return plan, err
		}
		for rows.Next() {
			if err := item.add(rows); err != nil {
				rows.Close()
				return plan, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (s *Store) projectScopedTables() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'projects'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		columns, err := s.db.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(table) + `)`)
		if err != nil {
			return nil, err
		}
		hasProjectID := false
		for columns.Next() {
			var cid, notNull, pk int
			var name, kind string
			var defaultValue any
			if err := columns.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err == nil && name == "project_id" {
				hasProjectID = true
			}
		}
		columns.Close()
		if hasProjectID {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	return tables, rows.Err()
}

// DeleteProjectCascade removes all server-owned rows for a project in one
// transaction. The schema grew many project_id-bearing tables without foreign
// keys; discovering that owned column prevents a new table from becoming an
// orphan source merely because this list was not updated.
func (s *Store) DeleteProjectCascade(projectID string) error {
	tables, err := s.projectScopedTables()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Agent children that do not carry project_id must be removed before their
	// parent agent rows. DeleteAgent cannot join this transaction, so repeat its
	// explicit dependent set here.
	for _, query := range []string{
		`DELETE FROM telemetry WHERE agent_id IN (SELECT id FROM agents WHERE project_id=?)`,
		`DELETE FROM app_agent_bindings WHERE agent_id IN (SELECT id FROM agents WHERE project_id=?)`,
		`DELETE FROM agent_creation_keys WHERE agent_id IN (SELECT id FROM agents WHERE project_id=?)`,
	} {
		if _, err := tx.Exec(query, projectID); err != nil {
			return err
		}
	}
	for _, table := range tables {
		if _, err := tx.Exec(`DELETE FROM `+quoteSQLiteIdentifier(table)+` WHERE project_id=?`, projectID); err != nil {
			return fmt.Errorf("delete project rows from %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE id=?`, projectID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) deleteProjectCompletely(projectID string) error {
	plan, err := s.store.projectCleanupPlan(projectID)
	if err != nil {
		return err
	}
	for _, id := range plan.AgentIDs {
		var detachInfo *framework.InstanceInfo
		if s.apps != nil {
			detachInfo = s.buildInstanceInfo(id)
		}
		s.agents.Stop(id)
		if s.apps != nil && detachInfo != nil {
			s.apps.NotifyInstanceDetach(*detachInfo)
		}
	}
	for _, id := range plan.InstallIDs {
		if s.localApps != nil {
			_ = s.localApps.Stop(id)
			s.localApps.ReleaseFixedPorts(id)
		}
		if s.installedApps != nil {
			s.installedApps.Remove(id)
		}
	}
	for _, id := range plan.EnvironmentIDs {
		if s.environments != nil {
			s.environments.Destroy(id)
		}
	}
	if err := s.store.DeleteProjectCascade(projectID); err != nil {
		return err
	}
	for _, id := range plan.AgentIDs {
		if err := os.RemoveAll(s.agents.instanceDir(id)); err != nil {
			log.Printf("[PROJECT-CLEANUP] agent directory project=%s agent=%d: %v", projectID, id, err)
		}
	}
	for _, id := range plan.ManagedMCPIDs {
		if err := os.RemoveAll(s.managedMCPWorkspace(id)); err != nil {
			log.Printf("[PROJECT-CLEANUP] managed MCP directory project=%s mcp=%d: %v", projectID, id, err)
		}
	}
	if s.installedApps != nil {
		s.RemountStaticApps()
	}
	return nil
}

func (s *Store) OwnedProjectIDs(userID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM projects WHERE user_id=? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) deleteUserCompletely(userID int64) error {
	if err := s.store.ValidateUserDeletion(userID); err != nil {
		return err
	}
	projectIDs, err := s.store.OwnedProjectIDs(userID)
	if err != nil {
		return err
	}
	for _, projectID := range projectIDs {
		if err := s.deleteProjectCompletely(projectID); err != nil {
			return err
		}
	}
	return s.store.DeleteUser(userID)
}

func (s *Store) CreateProjectFromAccessPolicy(userID int64, policy AccessPolicy) (*Project, error) {
	id := generateID()
	var expiresAt any
	if raw := policy.WorkspaceLifecycle.ExpiresAfter; raw != "" {
		d, _ := time.ParseDuration(raw)
		expiresAt = time.Now().UTC().Add(d).Format(time.RFC3339)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO projects(id,user_id,name,description,color,expires_at,provisioning_preset_id)
		VALUES(?,?,?,?,?,?,?)`, id, userID, policy.Provisioning.ProjectName, policy.Provisioning.ProjectDescription,
		"#6366f1", expiresAt, policy.Provisioning.PresetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES(?,?,'owner',?)`, id, userID, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Project{ID: id, UserID: userID, Name: policy.Provisioning.ProjectName, Description: policy.Provisioning.ProjectDescription, Color: "#6366f1", CreatedAt: time.Now()}, nil
}

func (s *Server) startWorkspaceLifecycle() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		policy, err := s.loadAccessPolicy()
		if err != nil {
			continue
		}
		s.stopIdleAgents(policy)
		if policy.WorkspaceLifecycle.ExpiresAfter == "" {
			continue
		}
		rows, err := s.store.db.Query(`SELECT id,user_id FROM projects
			WHERE expires_at IS NOT NULL AND datetime(expires_at) <= CURRENT_TIMESTAMP`)
		if err != nil {
			log.Printf("[WORKSPACE-LIFECYCLE] list expired: %v", err)
			continue
		}
		type expiredProject struct {
			id     string
			userID int64
		}
		var expired []expiredProject
		for rows.Next() {
			var item expiredProject
			if rows.Scan(&item.id, &item.userID) == nil {
				expired = append(expired, item)
			}
		}
		rows.Close()
		for _, item := range expired {
			if err := s.deleteProjectCompletely(item.id); err != nil {
				log.Printf("[WORKSPACE-LIFECYCLE] delete project=%s: %v", item.id, err)
				continue
			}
			if policy.WorkspaceLifecycle.ResetFromPreset {
				project, err := s.store.CreateProjectFromAccessPolicy(item.userID, policy)
				if err == nil {
					err = s.materializeProvisioningPreset(item.userID, project.ID, policy)
				}
				if err != nil {
					log.Printf("[WORKSPACE-LIFECYCLE] reprovision user=%d: %v", item.userID, err)
				}
			}
		}
	}
}

func (s *Server) stopIdleAgents(policy AccessPolicy) {
	raw := policy.WorkspaceLifecycle.IdleShutdownAfter
	if raw == "" {
		return
	}
	idleFor, err := time.ParseDuration(raw)
	if err != nil || idleFor <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-idleFor).Format(time.RFC3339)
	rows, err := s.store.db.Query(`SELECT a.id FROM agents a
		JOIN users u ON u.id=a.user_id
		WHERE a.status='running' AND COALESCE(a.kind,'user')='user' AND COALESCE(u.role,'user')!='admin'
		AND datetime(COALESCE((SELECT MAX(t.time) FROM telemetry t WHERE t.agent_id=a.id), a.created_at)) < datetime(?)`, cutoff)
	if err != nil {
		log.Printf("[WORKSPACE-LIFECYCLE] list idle agents: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		agent, err := s.store.GetAgentByID(id)
		if err != nil || !s.agents.IsRunning(id) {
			continue
		}
		s.agents.Stop(id)
		agent.Status, agent.Pid, agent.Port = "stopped", 0, 0
		if err := s.store.UpdateAgent(agent); err != nil {
			log.Printf("[WORKSPACE-LIFECYCLE] persist idle stop agent=%d: %v", id, err)
		}
	}
}
