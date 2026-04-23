package status

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Status struct {
	InstanceID    int64     `json:"instance_id"`
	Message       string    `json:"message"`
	Emoji         string    `json:"emoji,omitempty"`
	Tone          string    `json:"tone"`
	SetByThread   string    `json:"set_by_thread,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var ErrNotFound = errors.New("status: not found")

var validTones = map[string]bool{
	"info": true, "working": true, "warn": true, "error": true, "success": true, "idle": true,
}

type store struct{ db *sql.DB }

func newStore(db *sql.DB) *store { return &store{db: db} }

type UpsertParams struct {
	InstanceID  int64
	Message     string
	Emoji       string
	Tone        string
	SetByThread string
}

// Upsert writes (or overwrites) the single row for an instance and
// returns the stored result. Callers validate that they own the
// instance before this point.
func (s *store) Upsert(p UpsertParams) (*Status, error) {
	if p.InstanceID == 0 {
		return nil, fmt.Errorf("instance_id required")
	}
	if p.Message == "" {
		return nil, fmt.Errorf("message required")
	}
	if p.Tone == "" {
		p.Tone = "info"
	}
	if !validTones[p.Tone] {
		return nil, fmt.Errorf("invalid tone %q", p.Tone)
	}
	_, err := s.db.Exec(
		`INSERT INTO status_status (instance_id, message, emoji, tone, set_by_thread, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(instance_id) DO UPDATE SET
		   message = excluded.message,
		   emoji = excluded.emoji,
		   tone = excluded.tone,
		   set_by_thread = excluded.set_by_thread,
		   updated_at = CURRENT_TIMESTAMP`,
		p.InstanceID, p.Message, p.Emoji, p.Tone, nullIfEmpty(p.SetByThread),
	)
	if err != nil {
		return nil, err
	}
	return s.Get(p.InstanceID)
}

func (s *store) Get(instanceID int64) (*Status, error) {
	var out Status
	var emoji, tone sql.NullString
	var by sql.NullString
	err := s.db.QueryRow(
		`SELECT instance_id, message, emoji, tone, set_by_thread, updated_at
		 FROM status_status WHERE instance_id = ?`, instanceID,
	).Scan(&out.InstanceID, &out.Message, &emoji, &tone, &by, &out.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if emoji.Valid {
		out.Emoji = emoji.String
	}
	if tone.Valid {
		out.Tone = tone.String
	}
	if by.Valid {
		out.SetByThread = by.String
	}
	return &out, nil
}

// Clear removes the status row for an instance. Used when the agent
// wants to return to "no status" (falls back to the derived thought
// chip on the UI side).
func (s *store) Clear(instanceID int64) error {
	_, err := s.db.Exec(`DELETE FROM status_status WHERE instance_id = ?`, instanceID)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
