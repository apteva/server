package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Store) migrateAppRuntime() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS app_runtime_ingress (install_id INTEGER PRIMARY KEY REFERENCES app_installs(id),hostname TEXT NOT NULL UNIQUE,record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_packages (id TEXT PRIMARY KEY,build_key TEXT NOT NULL UNIQUE,record_json TEXT NOT NULL,bytes INTEGER NOT NULL,last_used INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_package_builds (build_key TEXT PRIMARY KEY,package_id TEXT NOT NULL,record_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS app_runtime_package_deliveries (backend_install_id INTEGER NOT NULL,backend_id TEXT NOT NULL,package_id TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(backend_install_id,backend_id,package_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_backends (install_id INTEGER NOT NULL REFERENCES app_installs(id),backend_id TEXT NOT NULL,record_json TEXT NOT NULL,PRIMARY KEY(install_id,backend_id))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_deployments (id TEXT PRIMARY KEY,install_id INTEGER NOT NULL REFERENCES app_installs(id),generation INTEGER NOT NULL,replica INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL,record_json TEXT NOT NULL,UNIQUE(install_id,generation,replica))`,
		`CREATE TABLE IF NOT EXISTS app_runtime_operations (id TEXT PRIMARY KEY,install_id INTEGER NOT NULL REFERENCES app_installs(id),kind TEXT NOT NULL,status TEXT NOT NULL,cancel_requested INTEGER NOT NULL DEFAULT 0,desired_replicas INTEGER NOT NULL DEFAULT 1,owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,idempotency_key TEXT NOT NULL,fingerprint TEXT NOT NULL,record_json TEXT NOT NULL,UNIQUE(install_id,idempotency_key))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS app_runtime_one_operation ON app_runtime_operations(install_id) WHERE status IN ('queued','running')`,
		`CREATE TABLE IF NOT EXISTS app_runtime_ports (host_key TEXT NOT NULL,protocol TEXT NOT NULL,port_start INTEGER NOT NULL,port_end INTEGER NOT NULL,install_id INTEGER NOT NULL,deployment_id TEXT NOT NULL REFERENCES app_runtime_deployments(id),UNIQUE(host_key,protocol,port_start,port_end,deployment_id))`,
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
	for _, q := range []string{`ALTER TABLE app_runtime_operations ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0`, `ALTER TABLE app_runtime_operations ADD COLUMN desired_replicas INTEGER NOT NULL DEFAULT 1`} {
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
	return nil
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
	return op, err
}
func (m *appRuntimeManager) saveOperation(op runtimeOperation) error {
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
