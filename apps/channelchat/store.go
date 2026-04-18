package channelchat

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Message mirrors one row of channel_chat_messages. All wire shapes
// (REST, SSE) marshal from this.
type Message struct {
	ID        int64     `json:"id"`
	ChatID    string    `json:"chat_id"`
	Role      string    `json:"role"` // user | agent | system
	Content   string    `json:"content"`
	UserID    *int64    `json:"user_id,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Status    string    `json:"status"` // streaming | final
	CreatedAt time.Time `json:"created_at"`
}

// Chat is one conversation — today typically one per instance.
type Chat struct {
	ID         string    `json:"id"`
	InstanceID int64     `json:"instance_id"`
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ErrNotFound is returned when a chat or message lookup misses.
var ErrNotFound = errors.New("channel-chat: not found")

type store struct {
	db *sql.DB
}

func newStore(db *sql.DB) *store { return &store{db: db} }

// EnsureDefaultChat returns the existing default chat for an instance
// or creates one. Default chat id convention: "default-<instance_id>"
// — stable across process restarts, and unique across instances so a
// future multi-instance-per-project UI can still look them up safely.
func (s *store) EnsureDefaultChat(instanceID int64) (*Chat, error) {
	chatID := defaultChatID(instanceID)
	// Try insert-or-ignore and then read back. Cheaper than
	// select-then-insert and race-safe.
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO channel_chat_chats (id, instance_id, title)
		 VALUES (?, ?, 'Chat')`,
		chatID, instanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("ensure default chat: %w", err)
	}
	return s.GetChat(chatID)
}

func defaultChatID(instanceID int64) string {
	return fmt.Sprintf("default-%d", instanceID)
}

func (s *store) GetChat(id string) (*Chat, error) {
	var c Chat
	err := s.db.QueryRow(
		`SELECT id, instance_id, title, created_at, updated_at
		 FROM channel_chat_chats WHERE id = ?`, id,
	).Scan(&c.ID, &c.InstanceID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *store) ListChatsForInstance(instanceID int64) ([]Chat, error) {
	rows, err := s.db.Query(
		`SELECT id, instance_id, title, created_at, updated_at
		 FROM channel_chat_chats WHERE instance_id = ? ORDER BY created_at ASC`,
		instanceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.InstanceID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Append inserts a new message and returns it (with the assigned id
// + created_at). Also bumps the parent chat's updated_at so client
// lists stay sorted by most-recent-activity.
func (s *store) Append(chatID, role, content string, userID *int64, threadID, status string) (*Message, error) {
	if role != "user" && role != "agent" && role != "system" {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	if status == "" {
		status = "final"
	}
	res, err := s.db.Exec(
		`INSERT INTO channel_chat_messages (chat_id, role, content, user_id, thread_id, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		chatID, role, content, userID, threadID, status,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	_, _ = s.db.Exec(
		`UPDATE channel_chat_chats SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		chatID,
	)
	return s.GetMessage(id)
}

func (s *store) GetMessage(id int64) (*Message, error) {
	var m Message
	var userID sql.NullInt64
	var threadID sql.NullString
	err := s.db.QueryRow(
		`SELECT id, chat_id, role, content, user_id, thread_id, status, created_at
		 FROM channel_chat_messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if userID.Valid {
		v := userID.Int64
		m.UserID = &v
	}
	if threadID.Valid {
		m.ThreadID = threadID.String
	}
	return &m, nil
}

// ListMessages returns rows for a chat with id > since, ordered by id
// asc. Limit caps the page size (default 500 if <= 0).
func (s *store) ListMessages(chatID string, since int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, chat_id, role, content, user_id, thread_id, status, created_at
		 FROM channel_chat_messages
		 WHERE chat_id = ? AND id > ?
		 ORDER BY id ASC
		 LIMIT ?`,
		chatID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMessages clears every message for a chat. The chat row stays.
func (s *store) DeleteMessages(chatID string) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM channel_chat_messages WHERE chat_id = ?`, chatID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// LatestID returns the highest message id for a chat (0 if empty).
// Used by SSE reconnect and by the dashboard to detect new messages.
func (s *store) LatestID(chatID string) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(id) FROM channel_chat_messages WHERE chat_id = ?`, chatID,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}
