package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *Store) migrateAppRuntime() error {
	if err := s.migrateAppComponents(); err != nil {
		return err
	}
	if err := s.migrateRuntimeConsole(); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS app_runtime_ingress_bindings (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id),record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_recovery (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id) ON DELETE CASCADE, attempts INTEGER NOT NULL, last_attempt INTEGER NOT NULL, next_attempt INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_resource_policies (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id) ON DELETE CASCADE, revision INTEGER NOT NULL, policy_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_ingress (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id),hostname TEXT NOT NULL UNIQUE,record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_packages (id TEXT PRIMARY KEY,build_key TEXT NOT NULL UNIQUE,record_json TEXT NOT NULL,bytes INTEGER NOT NULL,last_used INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_package_builds (build_key TEXT PRIMARY KEY,package_id TEXT NOT NULL,record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_package_deliveries (backend_install_id INTEGER NOT NULL,backend_id TEXT NOT NULL,package_id TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(backend_install_id,backend_id,package_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_backends (install_id INTEGER NOT NULL REFERENCES app_installs(id),backend_id TEXT NOT NULL,record_json TEXT NOT NULL,PRIMARY KEY(install_id,backend_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_deployments (id TEXT PRIMARY KEY,install_id INTEGER NOT NULL REFERENCES app_installs(id),generation INTEGER NOT NULL,replica INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL,record_json TEXT NOT NULL,UNIQUE(install_id,generation,replica))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_operations (id TEXT PRIMARY KEY,install_id INTEGER NOT NULL REFERENCES app_installs(id),kind TEXT NOT NULL,status TEXT NOT NULL,cancel_requested INTEGER NOT NULL DEFAULT 0,desired_replicas INTEGER NOT NULL DEFAULT 1,owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,idempotency_key TEXT NOT NULL,fingerprint TEXT NOT NULL,record_json TEXT NOT NULL,UNIQUE(install_id,idempotency_key))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS app_runtime_one_operation ON app_runtime_operations(install_id) WHERE status IN ('queued','running')`,
		`CREATE TABLE IF NOT EXISTS app_runtime_ports (host_key TEXT NOT NULL,protocol TEXT NOT NULL,port_start INTEGER NOT NULL,port_end INTEGER NOT NULL,install_id INTEGER NOT NULL,deployment_id TEXT NOT NULL REFERENCES app_runtime_deployments(id),UNIQUE(host_key,protocol,port_start,port_end,deployment_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_execution_owners (deployment_id TEXT PRIMARY KEY REFERENCES app_runtime_deployments(id) ON DELETE CASCADE, install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE, generation INTEGER NOT NULL, operation_id TEXT NOT NULL, granted INTEGER NOT NULL DEFAULT 0, admission_confirmed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS app_runtime_execution_owners_install ON app_runtime_execution_owners(install_id)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_credentials (deployment_id TEXT PRIMARY KEY REFERENCES app_runtime_deployments(id),token_hash TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_browser_revocations (token_hash TEXT PRIMARY KEY,expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_pools (id TEXT PRIMARY KEY,record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_scaling (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id),record_json TEXT NOT NULL)`,
	}
	for _, q := range statements {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("runtime schema: %w", err)
		}
	}
	for _, q := range []string{`ALTER TABLE app_runtime_execution_owners ADD COLUMN admission_confirmed INTEGER NOT NULL DEFAULT 0`, `ALTER TABLE app_runtime_operations ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0`, `ALTER TABLE app_runtime_operations ADD COLUMN desired_replicas INTEGER NOT NULL DEFAULT 1`} {
		if _, e := s.db.Exec(q); e != nil && !isRuntimeDuplicateColumn(e) {
			return e
		}
	}
	// Older installations stay on their existing supervisor until explicitly managed.
	for _, q := range []string{
		`ALTER TABLE app_installs ADD COLUMN runtime_managed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE app_installs ADD COLUMN runtime_placement_json TEXT NOT NULL DEFAULT '{"type":"local"}'`,
		`ALTER TABLE app_installs ADD COLUMN runtime_active_deployment TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE app_installs ADD COLUMN runtime_pending_deployment TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE app_installs ADD COLUMN runtime_generation INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(q); err != nil && !isRuntimeDuplicateColumn(err) {
			return err
		}
	}
	// Upgrade only installations that predate the ownership journal. Revocation
	// tombstones prevent a later restart from re-authorizing an old route.
	_, err := s.db.Exec(`INSERT OR IGNORE INTO app_runtime_execution_owners(deployment_id,install_id,generation,operation_id,granted,admission_confirmed)
	 SELECT d.id,d.install_id,d.generation,'legacy-active',1,1 FROM app_runtime_deployments d
	 JOIN app_installs i ON i.id=d.install_id JOIN app_runtime_deployments a ON a.id=i.runtime_active_deployment
	 WHERE i.runtime_managed=1 AND i.status='running' AND d.status='active' AND a.status='active' AND d.generation=a.generation
	 AND NOT EXISTS(SELECT 1 FROM app_runtime_execution_owners o WHERE o.install_id=i.id)`)
	return err
}
func isRuntimeDuplicateColumn(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "duplicate column name"))
}

func runtimeJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Private persisted fields never cross the operator API boundary.
type runtimeStoredDeployment struct {
	PID int `json:"local_pid,omitempty"`
	runtimeDeployment
	Credential  string `json:"credential_encrypted,omitempty"`
	Environment string `json:"environment_encrypted,omitempty"`
	Bin         string `json:"local_bin,omitempty"`
	Port        int    `json:"local_port,omitempty"`
}

func (m *appRuntimeManager) saveDeployment(d runtimeDeployment) error {
	if d.Placement.Ingress != nil && d.Placement.Ingress.Provider != nil {
		runtimeIngressEndpoints(&d)
	}
	raw := runtimeJSON(runtimeStoredDeployment{runtimeDeployment: d, Credential: d.CredentialEncrypted, Environment: d.EnvironmentEncrypted, Bin: d.LocalBin, Port: d.LocalPort, PID: d.LocalPID})
	_, err := m.s.store.db.Exec(`INSERT INTO app_runtime_deployments(id,install_id,generation,replica,status,record_json) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,record_json=excluded.record_json`, d.ID, d.InstallID, d.Generation, d.Replica, d.Status, raw)
	if err == nil && d.CredentialEncrypted != "" {
		token, e := Decrypt(m.s.secret, d.CredentialEncrypted)
		if e != nil {
			return e
		}
		_, err = m.s.store.db.Exec(`INSERT INTO app_runtime_credentials(deployment_id,token_hash) VALUES(?,?) ON CONFLICT(deployment_id) DO UPDATE SET token_hash=excluded.token_hash`, d.ID, HashAPIKey(token))
	}
	return err
}
func (m *appRuntimeManager) loadDeployment(id string) (runtimeDeployment, error) {
	var v runtimeStoredDeployment
	var raw string
	err := m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_deployments WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return v.runtimeDeployment, err
	}
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return v.runtimeDeployment, err
	}
	d := v.runtimeDeployment
	d.CredentialEncrypted = v.Credential
	d.EnvironmentEncrypted = v.Environment
	d.LocalBin = v.Bin
	d.LocalPort = v.Port
	d.LocalPID = v.PID
	return d, nil
}

type runtimeStoredOperation struct {
	runtimeOperation
	Snapshot string `json:"snapshot_path,omitempty"`
}

func (m *appRuntimeManager) operation(id string) (runtimeOperation, error) {
	var op runtimeOperation
	var raw string
	err := m.s.store.db.QueryRow(`SELECT record_json,owner,lease_until FROM app_runtime_operations WHERE id=?`, id).Scan(&raw, &op.Owner, &op.LeaseUntil)
	if err != nil {
		return op, err
	}
	owner, lease := op.Owner, op.LeaseUntil
	var stored runtimeStoredOperation
	err = json.Unmarshal([]byte(raw), &stored)
	op = stored.runtimeOperation
	op.SnapshotPath = stored.Snapshot
	op.Owner, op.LeaseUntil = owner, lease
	if start, e := time.Parse(time.RFC3339Nano, op.StartedAt); e == nil {
		end := time.Now()
		if op.Status != "running" && op.Status != "queued" {
			if completed, e := time.Parse(time.RFC3339Nano, op.UpdatedAt); e == nil {
				end = completed
			}
		}
		op.ElapsedMS = max(0, end.Sub(start).Milliseconds())
	}
	return op, err
}
func (m *appRuntimeManager) saveOperation(op runtimeOperation) error {
	// Backend work for a rollback targets its recovery execution; retain the
	// original candidate identity in the durable lifecycle operation.
	if op.RollbackID != "" && op.DeploymentID == op.RollbackID {
		op.DeploymentID = op.FailedID
	}
	op.UpdatedAt = runtimeTimestamp()
	result, err := m.s.store.db.Exec(`UPDATE app_runtime_operations SET status=?,record_json=? WHERE id=? AND owner=?`, op.Status, runtimeJSON(runtimeStoredOperation{op, op.SnapshotPath}), op.ID, m.owner)
	if err == nil {
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return fmt.Errorf("operation ownership lost")
		}
	}
	if err == nil && m.s.appBus != nil {
		var name, project string
		m.s.store.db.QueryRow(`SELECT a.name,COALESCE(i.project_id,'') FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, op.InstallID).Scan(&name, &project)
		b, _ := json.Marshal(op)
		m.s.appBus.Publish(name, project, op.InstallID, "runtime.operation.updated", b)
	}
	return err
}
func (m *appRuntimeManager) active(id int64) (runtimeDeployment, error) {
	var ref string
	if err := m.s.store.db.QueryRow(`SELECT runtime_active_deployment FROM app_installs WHERE id=?`, id).Scan(&ref); err != nil {
		return runtimeDeployment{}, err
	}
	if ref == "" {
		return runtimeDeployment{}, sql.ErrNoRows
	}
	return m.loadDeployment(ref)
}
func (s *Server) runtimeManaged(id int64) bool {
	var n int
	s.store.db.QueryRow(`SELECT runtime_managed FROM app_installs WHERE id=?`, id).Scan(&n)
	return n == 1
}
func (m *appRuntimeManager) reservePorts(d runtimeDeployment) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range d.Execution.Ports {
		if p.End == 0 {
			p.End = p.Start
		}
		if p.Start < 1 || p.End < p.Start || p.End > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") {
			return fmt.Errorf("invalid allocated port range")
		}
		var owner int64
		err = tx.QueryRow(`SELECT install_id FROM app_runtime_ports WHERE host_key=? AND protocol=? AND port_start<=? AND port_end>=? AND install_id!=? LIMIT 1`, d.Execution.HostKey, p.Protocol, p.End, p.Start, d.InstallID).Scan(&owner)
		if err == nil {
			return fmt.Errorf("port range is reserved by another installation")
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO app_runtime_ports(host_key,protocol,port_start,port_end,install_id,deployment_id) VALUES(?,?,?,?,?,?)`, d.Execution.HostKey, p.Protocol, p.Start, p.End, d.InstallID, d.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
