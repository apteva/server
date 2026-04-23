package tasks

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task mirrors one row of tasks_tasks. All wire shapes (REST, SSE,
// MCP tool results) marshal from this.
type Task struct {
	ID               int64      `json:"id"`
	InstanceID       int64      `json:"instance_id"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Status           string     `json:"status"`
	AssignedThread   string     `json:"assigned_thread,omitempty"`
	ParentTaskID     *int64     `json:"parent_task_id,omitempty"`
	CreatedByThread  string     `json:"created_by_thread,omitempty"`
	CreatedByUser    *int64     `json:"created_by_user,omitempty"`
	RewardXP         int        `json:"reward_xp"`
	Progress         int        `json:"progress"`
	Note             string     `json:"note"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
}

// ErrNotFound is returned when a task lookup misses.
var ErrNotFound = errors.New("tasks: not found")

// Valid statuses — kept in one place so REST validation, MCP validation,
// and the DB CHECK constraint stay in lockstep.
var validStatuses = map[string]bool{
	"open": true, "in_progress": true, "blocked": true, "done": true, "cancelled": true,
}

type store struct {
	db *sql.DB
}

func newStore(db *sql.DB) *store { return &store{db: db} }

// CreateParams bundles the inputs for an insert. Using a struct keeps
// the REST+MCP wrappers small; every optional field is a zero value by
// default.
type CreateParams struct {
	InstanceID       int64
	Title            string
	Description      string
	AssignedThread   string
	ParentTaskID     *int64
	CreatedByThread  string
	CreatedByUser    *int64
	RewardXP         int
}

func (s *store) Create(p CreateParams) (*Task, error) {
	p.Title = strings.TrimSpace(p.Title)
	if p.Title == "" {
		return nil, fmt.Errorf("title required")
	}
	if p.InstanceID == 0 {
		return nil, fmt.Errorf("instance_id required")
	}
	res, err := s.db.Exec(
		`INSERT INTO tasks_tasks
		 (instance_id, title, description, assigned_thread, parent_task_id,
		  created_by_thread, created_by_user, reward_xp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.InstanceID, p.Title, p.Description,
		nullIfEmpty(p.AssignedThread), p.ParentTaskID,
		nullIfEmpty(p.CreatedByThread), p.CreatedByUser, p.RewardXP,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Get(id)
}

// UpdateParams is the PATCH body. Nil pointers mean "leave unchanged";
// empty strings on string fields also mean unchanged so a UI can omit
// them without an explicit null.
type UpdateParams struct {
	Title          *string
	Description    *string
	Status         *string
	Progress       *int
	Note           *string
	AssignedThread *string
}

func (s *store) Update(id int64, p UpdateParams) (*Task, error) {
	// Dynamic SET clause keeps the handler code readable and avoids
	// accidentally stomping fields the caller didn't touch. Fields
	// appear in the DB in the same order as the struct for easier
	// diffing against the schema.
	sets := []string{}
	args := []any{}
	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, strings.TrimSpace(*p.Title))
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.Status != nil {
		if !validStatuses[*p.Status] {
			return nil, fmt.Errorf("invalid status %q", *p.Status)
		}
		sets = append(sets, "status = ?")
		args = append(args, *p.Status)
		// completed_at is a derived field; set/clear to match status.
		if *p.Status == "done" {
			sets = append(sets, "completed_at = CURRENT_TIMESTAMP")
		} else {
			sets = append(sets, "completed_at = NULL")
		}
	}
	if p.Progress != nil {
		v := *p.Progress
		if v < 0 {
			v = 0
		} else if v > 100 {
			v = 100
		}
		sets = append(sets, "progress = ?")
		args = append(args, v)
	}
	if p.Note != nil {
		sets = append(sets, "note = ?")
		args = append(args, *p.Note)
	}
	if p.AssignedThread != nil {
		sets = append(sets, "assigned_thread = ?")
		args = append(args, nullIfEmpty(*p.AssignedThread))
	}
	if len(sets) == 0 {
		return s.Get(id)
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, id)
	q := "UPDATE tasks_tasks SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(id)
}

func (s *store) Delete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM tasks_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) Get(id int64) (*Task, error) {
	row := s.db.QueryRow(
		`SELECT id, instance_id, title, description, status,
		        assigned_thread, parent_task_id, created_by_thread,
		        created_by_user, reward_xp, progress, note,
		        created_at, updated_at, completed_at
		 FROM tasks_tasks WHERE id = ?`, id,
	)
	return scanTask(row)
}

// ListParams filters the list. Zero values mean "no filter".
type ListParams struct {
	InstanceID     int64
	Status         string // comma-separated ok: "open,in_progress"
	AssignedThread string
	ParentTaskID   *int64 // use &(-1) to mean "top-level only (NULL)"
	SinceID        int64  // id > SinceID
	Limit          int    // default 500, max 1000
}

func (s *store) List(p ListParams) ([]Task, error) {
	if p.Limit <= 0 || p.Limit > 1000 {
		p.Limit = 500
	}
	where := []string{"instance_id = ?"}
	args := []any{p.InstanceID}
	if p.Status != "" {
		parts := strings.Split(p.Status, ",")
		placeholders := []string{}
		for _, s := range parts {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, s)
		}
		if len(placeholders) > 0 {
			where = append(where, "status IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if p.AssignedThread != "" {
		where = append(where, "assigned_thread = ?")
		args = append(args, p.AssignedThread)
	}
	if p.ParentTaskID != nil {
		if *p.ParentTaskID < 0 {
			where = append(where, "parent_task_id IS NULL")
		} else {
			where = append(where, "parent_task_id = ?")
			args = append(args, *p.ParentTaskID)
		}
	}
	if p.SinceID > 0 {
		where = append(where, "id > ?")
		args = append(args, p.SinceID)
	}
	args = append(args, p.Limit)
	q := `SELECT id, instance_id, title, description, status,
	             assigned_thread, parent_task_id, created_by_thread,
	             created_by_user, reward_xp, progress, note,
	             created_at, updated_at, completed_at
	      FROM tasks_tasks
	      WHERE ` + strings.Join(where, " AND ") + `
	      ORDER BY id DESC LIMIT ?`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// --- scanning helpers ------------------------------------------------

// rowScanner lets Get and List share one scan body.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	var t Task
	var assigned sql.NullString
	var parent sql.NullInt64
	var createdByThread sql.NullString
	var createdByUser sql.NullInt64
	var completed sql.NullTime
	err := row.Scan(
		&t.ID, &t.InstanceID, &t.Title, &t.Description, &t.Status,
		&assigned, &parent, &createdByThread, &createdByUser,
		&t.RewardXP, &t.Progress, &t.Note,
		&t.CreatedAt, &t.UpdatedAt, &completed,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if assigned.Valid {
		t.AssignedThread = assigned.String
	}
	if parent.Valid {
		v := parent.Int64
		t.ParentTaskID = &v
	}
	if createdByThread.Valid {
		t.CreatedByThread = createdByThread.String
	}
	if createdByUser.Valid {
		v := createdByUser.Int64
		t.CreatedByUser = &v
	}
	if completed.Valid {
		v := completed.Time
		t.CompletedAt = &v
	}
	return &t, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
