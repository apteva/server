package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Devices belong to an account. Project and agent IDs are defaults/current
// context, never credential scopes; ordinary authorization runs per operation.
type AccountDevice struct {
	ID        string          `json:"id"`
	UserID    int64           `json:"-"`
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Hardware  json.RawMessage `json:"hardware"`
	Tools     json.RawMessage `json:"tools"`
	Events    json.RawMessage `json:"events"`
	ProjectID string          `json:"project_id"`
	AgentID   string          `json:"agent_id"`
	CreatedAt int64           `json:"created_at"`
	LastSeen  int64           `json:"last_seen"`
	RevokedAt int64           `json:"revoked_at"`
	Connected bool            `json:"connected"`
	Current   bool            `json:"current"`
}

type devicePrincipalKey struct{}

func requestDevice(rctx context.Context) *AccountDevice {
	d, _ := rctx.Value(devicePrincipalKey{}).(*AccountDevice)
	return d
}

func (s *Store) migrateAccountDevices() error {
	_, err := s.dbExec(
		"CREATE TABLE IF NOT EXISTS account_devices (" +
			"id TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE," +
			"name TEXT NOT NULL,version TEXT NOT NULL DEFAULT '',hardware TEXT NOT NULL DEFAULT '[]'," +
			"tools TEXT NOT NULL DEFAULT '[]',events TEXT NOT NULL DEFAULT '[]'," +
			"token_hash TEXT NOT NULL UNIQUE,project_id TEXT NOT NULL DEFAULT '',agent_id TEXT NOT NULL DEFAULT ''," +
			"created_at INTEGER NOT NULL,last_seen INTEGER NOT NULL DEFAULT 0,revoked_at INTEGER NOT NULL DEFAULT 0);" +
			"CREATE INDEX IF NOT EXISTS account_devices_user ON account_devices(user_id);" +
			"CREATE TABLE IF NOT EXISTS device_pairings (" +
			"id_hash TEXT PRIMARY KEY,user_code TEXT NOT NULL UNIQUE,name TEXT NOT NULL,version TEXT NOT NULL,hardware TEXT NOT NULL," +
			"status TEXT NOT NULL DEFAULT 'pending',expires_at INTEGER NOT NULL," +
			"user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,device_id TEXT NOT NULL DEFAULT '',credential TEXT NOT NULL DEFAULT '');" +
			"CREATE INDEX IF NOT EXISTS device_pairings_expiry ON device_pairings(expires_at)")
	if err != nil {
		return err
	}
	return s.migrateDeviceAgents()
}

const deviceColumns = "id,user_id,name,version,hardware,tools,events,project_id,agent_id,created_at,last_seen,revoked_at"

func scanAccountDevice(row interface{ Scan(...any) error }) (*AccountDevice, error) {
	d := &AccountDevice{}
	var hardware, tools, events string
	err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.Version, &hardware, &tools, &events, &d.ProjectID, &d.AgentID, &d.CreatedAt, &d.LastSeen, &d.RevokedAt)
	d.Hardware, d.Tools, d.Events = json.RawMessage(hardware), json.RawMessage(tools), json.RawMessage(events)
	return d, err
}

func (s *Store) accountDevice(id string, userID int64) (*AccountDevice, error) {
	return scanAccountDevice(s.dbQueryRow("SELECT "+deviceColumns+" FROM account_devices WHERE id=? AND user_id=?", id, userID))
}

func (s *Store) accountDeviceForToken(token string) (*AccountDevice, error) {
	return s.accountDeviceForTokenContext(nil, token)
}
func (s *Store) accountDeviceForTokenContext(ctx context.Context, token string) (*AccountDevice, error) {
	d, err := scanAccountDevice(s.dbQueryRowContext(ctx, "SELECT "+deviceColumns+" FROM account_devices WHERE token_hash=? AND revoked_at=0 AND EXISTS(SELECT 1 FROM users WHERE users.id=account_devices.user_id AND users.disabled_at IS NULL)", HashAPIKey(token)))
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = s.dbExecContext(ctx, "UPDATE account_devices SET last_seen=? WHERE id=? AND last_seen<? AND revoked_at=0", now, d.ID, now-30)
	return d, err
}

func (s *Store) revokeAccountDevice(id string, userID int64) error {
	tx, err := s.dbBegin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE account_devices SET revoked_at=? WHERE id=? AND user_id=?", time.Now().Unix(), id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.Exec("UPDATE device_pairings SET credential='',status='denied' WHERE device_id=? AND user_id=?", id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// Session-wide invalidation includes Node credentials and uncollected pairings.
// Sessions are revoked (not deleted) so the security audit keeps their record;
// device credentials and pending pairing handoffs are revoked in the same
// transaction so a credential change cannot leave a Node signed in.
func (s *Store) revokeSessionsAndDevices(userID int64, keepToken string, actor int64, reason string) error {
	tx, err := s.dbBegin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL AND token != ?`, sqliteNow(), actor, reason, userID, sessionKey(keepToken)); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account_devices SET revoked_at=? WHERE user_id=? AND revoked_at=0", time.Now().Unix(), userID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE device_pairings SET credential='',status='denied' WHERE user_id=?", userID); err != nil {
		return err
	}
	return tx.Commit()
}
