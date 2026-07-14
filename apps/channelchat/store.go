package channelchat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/apteva/server/apps/framework"
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
	// Components — rich attachments the agent put on this message
	// via respond(components=…). Empty array when none. Always emitted
	// (not omitempty) so the dashboard can rely on the field existing.
	Components []framework.ChatComponent `json:"components"`
	// Attachments are user-supplied media attached to this message.
	// Kept separate from Components, which are agent-rendered UI hints.
	Attachments []ChatAttachment `json:"attachments"`
}

type ChatAttachment struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type"` // image
	DataURL   string `json:"data_url,omitempty"`
	Name      string `json:"name,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

type ApprovalMessage struct {
	Message   Message `json:"message"`
	AgentID   int64   `json:"instance_id"`
	AgentName string  `json:"instance_name"`
	ProjectID string  `json:"project_id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Status    string  `json:"status"`
	Dismissed bool    `json:"dismissed,omitempty"`
}

type ReportMessage struct {
	Message   Message `json:"message"`
	AgentID   int64   `json:"instance_id"`
	AgentName string  `json:"instance_name"`
	ProjectID string  `json:"project_id"`
	Title     string  `json:"title"`
	Summary   string  `json:"summary"`
	Period    string  `json:"period,omitempty"`
	Dismissed bool    `json:"dismissed,omitempty"`
}

type AlertMessage struct {
	Message   Message `json:"message"`
	AgentID   int64   `json:"instance_id"`
	AgentName string  `json:"instance_name"`
	ProjectID string  `json:"project_id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Severity  string  `json:"severity"`
	Dismissed bool    `json:"dismissed,omitempty"`
}

type CurrentStatusMessage struct {
	Message   Message  `json:"message"`
	AgentID   int64    `json:"instance_id"`
	AgentName string   `json:"instance_name"`
	ProjectID string   `json:"project_id"`
	Title     string   `json:"title"`
	Detail    string   `json:"detail,omitempty"`
	State     string   `json:"state"`
	Progress  *float64 `json:"progress,omitempty"`
	Stale     bool     `json:"stale"`
}

// Chat is one conversation — today typically one per instance.
type Chat struct {
	ID        string    `json:"id"`
	AgentID   int64     `json:"instance_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ThreadID is the core thread that handles this chat. Empty =
	// route to "main" (legacy / feature flag off). Assigned lazily
	// on first message via EnsureChatThread when the feature flag
	// is on, and persisted so subsequent messages and post-restart
	// reconnects reuse the same thread.
	ThreadID string `json:"thread_id,omitempty"`
}

// ErrNotFound is returned when a chat or message lookup misses.
var ErrNotFound = errors.New("channel-chat: not found")

type store struct {
	db *sql.DB
}

func newStore(db *sql.DB) *store { return &store{db: db} }

func (s *store) withImmediateWrite(fn func(context.Context, *sql.Conn) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// renameInstanceIDToAgentID catches up DBs created before the
// platform's instance → agent rename. The first 001_init.sql on
// fresh DBs now creates channel_chat_chats with agent_id directly,
// so this check no-ops there; on legacy DBs it issues the ALTER once.
// SQLite doesn't have IF EXISTS on ALTER COLUMN, so guard via
// PRAGMA table_info.
//
// Called from App.OnMount — runs on every boot, idempotent. We
// deliberately don't lean on framework_app_versions for this because
// the v1 migration's text now produces a post-rename schema, and we
// don't want fresh installs to fail on a v5 SQL migration that
// references the legacy column.
func (s *store) renameInstanceIDToAgentID() {
	rows, err := s.db.Query(`PRAGMA table_info(channel_chat_chats)`)
	if err != nil {
		return
	}
	defer rows.Close()
	var hasLegacy bool
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == "instance_id" {
			hasLegacy = true
		}
	}
	if !hasLegacy {
		return
	}
	s.db.Exec(`ALTER TABLE channel_chat_chats RENAME COLUMN instance_id TO agent_id`)
	s.db.Exec(`DROP INDEX IF EXISTS idx_channel_chat_chats_instance`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_channel_chat_chats_agent ON channel_chat_chats(agent_id)`)
}

// EnsureDefaultChat returns the existing default chat for an instance
// or creates one. Default chat id convention: "default-<agent_id>"
// — stable across process restarts, and unique across instances so a
// future multi-instance-per-project UI can still look them up safely.
func (s *store) EnsureDefaultChat(agentID int64) (*Chat, error) {
	chatID := defaultChatID(agentID)
	// Try insert-or-ignore and then read back. Cheaper than
	// select-then-insert and race-safe.
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO channel_chat_chats (id, agent_id, title)
		 VALUES (?, ?, 'Chat')`,
		chatID, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("ensure default chat: %w", err)
	}
	return s.GetChat(chatID)
}

func defaultChatID(agentID int64) string {
	return fmt.Sprintf("default-%d", agentID)
}

func (s *store) GetChat(id string) (*Chat, error) {
	var c Chat
	err := s.db.QueryRow(
		`SELECT id, agent_id, title, created_at, updated_at, thread_id
		 FROM channel_chat_chats WHERE id = ?`, id,
	).Scan(&c.ID, &c.AgentID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ThreadID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *store) ListChatsForAgent(agentID int64) ([]Chat, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, title, created_at, updated_at, thread_id
		 FROM channel_chat_chats WHERE agent_id = ? ORDER BY created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.AgentID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ThreadID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnsureChatThread returns the stored thread id for a chat, or
// assigns "chat-<chatID>" and persists it on first call. Used by the
// handler when CHANNELCHAT_PER_THREAD is on, so each chat gets its
// own core thread instead of sharing "main".
func (s *store) EnsureChatThread(chatID string) (string, error) {
	var existing string
	err := s.db.QueryRow(
		`SELECT thread_id FROM channel_chat_chats WHERE id = ?`, chatID,
	).Scan(&existing)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	threadID := "chat-" + chatID
	if _, err := s.db.Exec(
		`UPDATE channel_chat_chats SET thread_id = ? WHERE id = ? AND thread_id = ''`,
		threadID, chatID,
	); err != nil {
		return "", err
	}
	// Re-read in case a concurrent caller won the race.
	if err := s.db.QueryRow(
		`SELECT thread_id FROM channel_chat_chats WHERE id = ?`, chatID,
	).Scan(&existing); err != nil {
		return "", err
	}
	return existing, nil
}

// Append inserts a new message and returns it (with the assigned id
// + created_at). Also bumps the parent chat's updated_at so client
// lists stay sorted by most-recent-activity.
//
// components is optional — pass nil for plain text messages.
// Persisted as JSON in components_json; the dashboard reads it back
// on stream/list and mounts each entry as a rich attachment.
func (s *store) Append(chatID, role, content string, userID *int64, threadID, status string, components []framework.ChatComponent) (*Message, error) {
	return s.AppendFull(chatID, role, content, userID, threadID, status, components, nil)
}

func (s *store) AppendFull(chatID, role, content string, userID *int64, threadID, status string, components []framework.ChatComponent, attachments []ChatAttachment) (*Message, error) {
	if role != "user" && role != "agent" && role != "system" {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	if status == "" {
		status = "final"
	}
	if components == nil {
		components = []framework.ChatComponent{}
	}
	componentsJSON, err := json.Marshal(components)
	if err != nil {
		return nil, fmt.Errorf("marshal components: %w", err)
	}
	if attachments == nil {
		attachments = []ChatAttachment{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return nil, fmt.Errorf("marshal attachments: %w", err)
	}
	res, err := s.db.Exec(
		`INSERT INTO channel_chat_messages (chat_id, role, content, user_id, thread_id, status, components_json, attachments_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		chatID, role, content, userID, threadID, status, string(componentsJSON), string(attachmentsJSON),
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

// AppendAgentMessageOnce persists a normal agent reply while suppressing an
// immediate exact retry. Tool calls can be retried after a parallel sibling
// fails even when the first message was delivered successfully. Treat the
// retry as idempotent only when no newer visible user message exists, so two
// separate user turns can still receive the same legitimate response.
func (s *store) AppendAgentMessageOnce(chatID, content, threadID string, components []framework.ChatComponent) (*Message, bool, error) {
	if components == nil {
		components = []framework.ChatComponent{}
	}
	componentsJSON, err := json.Marshal(components)
	if err != nil {
		return nil, false, fmt.Errorf("marshal components: %w", err)
	}
	encodedComponents := string(componentsJSON)
	var id int64
	inserted := false
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		var latestRole, latestContent, latestThread, latestComponents string
		latestErr := conn.QueryRowContext(ctx, `
			SELECT id, role, content, COALESCE(thread_id, ''), COALESCE(components_json, '[]')
			FROM channel_chat_messages
			WHERE chat_id = ?
			  AND created_at >= datetime('now', '-5 seconds')
			  AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"status-card"%'
			ORDER BY id DESC
			LIMIT 1`, chatID).Scan(&id, &latestRole, &latestContent, &latestThread, &latestComponents)
		if latestErr == nil && latestRole == "agent" && latestContent == content &&
			latestThread == threadID && latestComponents == encodedComponents {
			return nil
		}
		if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
			return latestErr
		}

		res, err := conn.ExecContext(ctx, `
			INSERT INTO channel_chat_messages
				(chat_id, role, content, user_id, thread_id, status, components_json, attachments_json)
			VALUES (?, 'agent', ?, NULL, ?, 'final', ?, '[]')`,
			chatID, content, threadID, encodedComponents)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE channel_chat_chats SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, chatID); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	m, err := s.GetMessage(id)
	return m, inserted, err
}

// UpsertCurrentStatus keeps exactly one mutable status row per chat. Status is
// operational state rather than conversation history, so it deliberately does
// not update channel_chat_chats.updated_at or the unread watermark.
func (s *store) UpsertCurrentStatus(chatID, content string, components []framework.ChatComponent) (*Message, error) {
	componentsJSON, err := json.Marshal(components)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		// A normal database/sql transaction is DEFERRED in SQLite. The SELECT
		// below then takes a read lock which can fail immediately when upgraded
		// to a writer if a parallel chat message already owns the write
		// reservation. BEGIN IMMEDIATE queues for the writer lock up front, so
		// the connection's busy_timeout applies instead of surfacing SQLITE_BUSY.
		err = conn.QueryRowContext(ctx, `
			SELECT id FROM channel_chat_messages
			WHERE chat_id = ? AND COALESCE(components_json, '[]') LIKE '%"status-card"%'
			ORDER BY id DESC LIMIT 1`, chatID).Scan(&id)
		switch {
		case err == nil:
			_, err = conn.ExecContext(ctx, `
				UPDATE channel_chat_messages
				SET role='agent', content=?, thread_id='', status='final', created_at=CURRENT_TIMESTAMP,
				    components_json=?, attachments_json='[]'
				WHERE id=?`, content, string(componentsJSON), id)
		case errors.Is(err, sql.ErrNoRows):
			err = conn.QueryRowContext(ctx, `
				INSERT INTO channel_chat_messages
					(chat_id, role, content, thread_id, status, components_json, attachments_json)
				VALUES (?, 'agent', ?, '', 'final', ?, '[]')
				RETURNING id`, chatID, content, string(componentsJSON)).Scan(&id)
		}
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetMessage(id)
}

func (s *store) GetMessage(id int64) (*Message, error) {
	var m Message
	var userID sql.NullInt64
	var threadID sql.NullString
	var componentsJSON sql.NullString
	var attachmentsJSON sql.NullString
	err := s.db.QueryRow(
		`SELECT id, chat_id, role, content, user_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]')
		 FROM channel_chat_messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt, &componentsJSON, &attachmentsJSON)
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
	m.Components = decodeComponents(componentsJSON.String)
	m.Attachments = decodeAttachments(attachmentsJSON.String)
	return &m, nil
}

func (s *store) UpdateMessageComponents(id int64, components []framework.ChatComponent) (*Message, error) {
	if components == nil {
		components = []framework.ChatComponent{}
	}
	componentsJSON, err := json.Marshal(components)
	if err != nil {
		return nil, fmt.Errorf("marshal components: %w", err)
	}
	res, err := s.db.Exec(
		`UPDATE channel_chat_messages SET components_json = ? WHERE id = ?`,
		string(componentsJSON), id,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetMessage(id)
}

// decodeComponents tolerates legacy rows (NULL or empty string) and
// always returns a non-nil slice so the JSON marshaler emits [] rather
// than null. The dashboard relies on the field always existing.
func decodeComponents(raw string) []framework.ChatComponent {
	if raw == "" {
		return []framework.ChatComponent{}
	}
	var out []framework.ChatComponent
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []framework.ChatComponent{}
	}
	return out
}

func decodeAttachments(raw string) []ChatAttachment {
	if raw == "" {
		return []ChatAttachment{}
	}
	var out []ChatAttachment
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []ChatAttachment{}
	}
	return out
}

// ListMessages returns the next page of rows for a chat with id > since,
// ordered by id asc. It is the cursor API used by SSE catch-up.
func (s *store) ListMessages(chatID string, since int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, chat_id, role, content, user_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]')
		 FROM channel_chat_messages
		 WHERE chat_id = ? AND id > ?
		   AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"status-card"%'
		 ORDER BY id ASC
		 LIMIT ?`,
		chatID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	return scanMessageRows(rows)
}

// ListRecentMessages returns the newest page of a chat in chronological
// order. Fetching DESC in SQL and reversing the bounded page avoids the old
// behavior where a long-lived chat always rendered its first 500 messages and
// hid everything recent.
func (s *store) ListRecentMessages(chatID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, chat_id, role, content, user_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]')
		 FROM channel_chat_messages
		 WHERE chat_id = ?
		   AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"status-card"%'
		 ORDER BY id DESC
		 LIMIT ?`,
		chatID, limit,
	)
	if err != nil {
		return nil, err
	}
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, nil
}

func scanMessageRows(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON sql.NullString
		var attachmentsJSON sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt, &componentsJSON, &attachmentsJSON); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) ListApprovalMessages(ownerIDs []int64, projectID, status string, limit int) ([]ApprovalMessage, error) {
	if len(ownerIDs) == 0 {
		return []ApprovalMessage{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	queryLimit := limit
	if status != "" && status != "all" {
		queryLimit = limit * 5
		if queryLimit < 100 {
			queryLimit = 100
		}
		if queryLimit > 500 {
			queryLimit = 500
		}
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+3)
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := `c.agent_id IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"approval-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, queryLimit)
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       c.agent_id, COALESCE(i.name, ''), COALESCE(i.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = c.agent_id
		WHERE ` + where + `
		ORDER BY m.id DESC
		LIMIT ?`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalMessage{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON sql.NullString
		var attachmentsJSON sql.NullString
		var row ApprovalMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt,
			&componentsJSON, &attachmentsJSON, &row.AgentID, &row.AgentName, &row.ProjectID); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		title, body, cardStatus, dismissed, ok := approvalSummary(m.Components)
		if !ok {
			continue
		}
		if dismissed {
			continue
		}
		if status != "" && status != "all" && cardStatus != status {
			continue
		}
		row.Message = m
		row.Title = title
		row.Body = body
		row.Status = cardStatus
		row.Dismissed = dismissed
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func (s *store) ListReportMessages(ownerIDs []int64, projectID string, limit int) ([]ReportMessage, error) {
	if len(ownerIDs) == 0 {
		return []ReportMessage{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+2)
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := `c.agent_id IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"report-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, limit)
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       c.agent_id, COALESCE(i.name, ''), COALESCE(i.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = c.agent_id
		WHERE ` + where + `
		ORDER BY m.id DESC
		LIMIT ?`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportMessage{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON sql.NullString
		var attachmentsJSON sql.NullString
		var row ReportMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt,
			&componentsJSON, &attachmentsJSON, &row.AgentID, &row.AgentName, &row.ProjectID); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		title, summary, period, dismissed, ok := reportSummary(m.Components)
		if !ok {
			continue
		}
		if dismissed {
			continue
		}
		row.Message = m
		row.Title = title
		row.Summary = summary
		row.Period = period
		row.Dismissed = dismissed
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *store) ListAlertMessages(ownerIDs []int64, projectID string, limit int) ([]AlertMessage, error) {
	if len(ownerIDs) == 0 {
		return []AlertMessage{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+2)
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := `c.agent_id IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"alert-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, limit)
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       c.agent_id, COALESCE(i.name, ''), COALESCE(i.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = c.agent_id
		WHERE ` + where + `
		ORDER BY m.id DESC
		LIMIT ?`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertMessage{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON sql.NullString
		var attachmentsJSON sql.NullString
		var row AlertMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt,
			&componentsJSON, &attachmentsJSON, &row.AgentID, &row.AgentName, &row.ProjectID); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		title, body, severity, dismissed, ok := alertSummary(m.Components)
		if !ok {
			continue
		}
		if dismissed {
			continue
		}
		row.Message = m
		row.Title = title
		row.Body = body
		row.Severity = severity
		row.Dismissed = dismissed
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *store) ListCurrentStatuses(ownerIDs []int64, projectID string) ([]CurrentStatusMessage, error) {
	if len(ownerIDs) == 0 {
		return []CurrentStatusMessage{}, nil
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+1)
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := `c.agent_id IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"status-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       c.agent_id, COALESCE(i.name, ''), COALESCE(i.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = c.agent_id
		WHERE ` + where + `
		ORDER BY m.created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := []CurrentStatusMessage{}
	for rows.Next() {
		var m Message
		var userID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON, attachmentsJSON sql.NullString
		var row CurrentStatusMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &threadID, &m.Status, &m.CreatedAt,
			&componentsJSON, &attachmentsJSON, &row.AgentID, &row.AgentName, &row.ProjectID); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		title, detail, state, progress, ok := currentStatusSummary(m.Components)
		if !ok {
			continue
		}
		row.Message = m
		row.Title, row.Detail, row.State, row.Progress = title, detail, state, progress
		row.Stale = state != "completed" && now.Sub(m.CreatedAt) > 30*time.Minute
		out = append(out, row)
	}
	return out, rows.Err()
}

func approvalSummary(components []framework.ChatComponent) (title, body, status string, dismissed bool, ok bool) {
	for _, c := range components {
		if c.App != "channel-chat" || c.Name != "approval-card" {
			continue
		}
		props := c.Props
		title, _ = props["title"].(string)
		body, _ = props["body"].(string)
		status, _ = props["status"].(string)
		dismissed = componentDismissed(props)
		if status == "" {
			status = "pending"
		}
		return title, body, status, dismissed, true
	}
	return "", "", "", false, false
}

func reportSummary(components []framework.ChatComponent) (title, summary, period string, dismissed bool, ok bool) {
	for _, c := range components {
		if c.App != "channel-chat" || c.Name != "report-card" {
			continue
		}
		props := c.Props
		title, _ = props["title"].(string)
		summary, _ = props["summary"].(string)
		period, _ = props["period"].(string)
		dismissed = componentDismissed(props)
		return title, summary, period, dismissed, true
	}
	return "", "", "", false, false
}

func alertSummary(components []framework.ChatComponent) (title, body, severity string, dismissed bool, ok bool) {
	for _, c := range components {
		if c.App != "channel-chat" || c.Name != "alert-card" {
			continue
		}
		props := c.Props
		title, _ = props["title"].(string)
		body, _ = props["body"].(string)
		severity, _ = props["severity"].(string)
		dismissed = componentDismissed(props)
		if severity == "" {
			severity = "info"
		}
		return title, body, severity, dismissed, true
	}
	return "", "", "", false, false
}

func currentStatusSummary(components []framework.ChatComponent) (title, detail, state string, progress *float64, ok bool) {
	for _, c := range components {
		if c.App != "channel-chat" || c.Name != "status-card" {
			continue
		}
		title, _ = c.Props["title"].(string)
		detail, _ = c.Props["detail"].(string)
		state, _ = c.Props["state"].(string)
		if state == "" {
			state = "working"
		}
		if value, exists := c.Props["progress"].(float64); exists {
			progress = &value
		}
		return title, detail, state, progress, true
	}
	return "", "", "", nil, false
}

func componentDismissed(props map[string]any) bool {
	if props == nil {
		return false
	}
	if v, ok := props["dismissed"].(bool); ok && v {
		return true
	}
	if v, ok := props["dismissed_at"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	return false
}

// DeleteMessages clears every message for a chat. The chat row stays.
func (s *store) DeleteMessages(chatID string) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM channel_chat_messages
		 WHERE chat_id = ?
		   AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
		   AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'`,
		chatID,
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

// ChatLatest is the per-chat snapshot the dashboard's notifications
// tray uses to compute unread counts. Latest* fields describe the most
// recent message on the chat (zero values if the chat is empty).
// LastSeenID is the persisted read watermark — the dashboard takes
// max(localStorage, LastSeenID) so reads on any device propagate.
type ChatLatest struct {
	ChatID        string    `json:"chat_id"`
	AgentID       int64     `json:"instance_id"`
	AgentName     string    `json:"instance_name"`
	Title         string    `json:"title"`
	LatestID      int64     `json:"latest_id"`
	LatestRole    string    `json:"latest_role"`
	LatestPreview string    `json:"latest_preview"`
	LatestAt      time.Time `json:"latest_at"`
	LastSeenID    int64     `json:"last_seen_id"`
}

// LatestForOwner returns one ChatLatest per chat whose instance is
// owned by ownerIDs. Joins channel_chat_chats to instances so the tray
// can render the instance name without a second round-trip; the
// instances table lives in the apteva-server schema, not the app's,
// but they share one SQLite db so the JOIN works.
//
// Single query, indexed on (chat_id, id) for the message subquery and
// agent_id is the primary key. Cheap even with hundreds of chats.
func (s *store) LatestForOwner(ownerIDs []int64) ([]ChatLatest, error) {
	if len(ownerIDs) == 0 {
		return []ChatLatest{}, nil
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, len(ownerIDs))
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `
		SELECT c.id, c.agent_id, COALESCE(i.name, ''), c.title,
		       COALESCE(m.id, 0),
		       COALESCE(m.role, ''),
		       COALESCE(m.content, ''),
		       COALESCE(m.created_at, c.updated_at),
		       c.last_seen_id
		FROM channel_chat_chats c
		JOIN agents i ON i.id = c.agent_id
		LEFT JOIN channel_chat_messages m
			ON m.id = (
				SELECT MAX(id)
				FROM channel_chat_messages
				WHERE chat_id = c.id
				  AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
				  AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
				  AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'
				  AND COALESCE(components_json, '[]') NOT LIKE '%"status-card"%'
			)
		WHERE c.agent_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY COALESCE(m.created_at, c.updated_at) DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatLatest{}
	for rows.Next() {
		var cl ChatLatest
		// COALESCE strips the DATETIME type declaration, so the driver
		// returns the value as string. Scan into string then parse.
		var latestAtStr string
		if err := rows.Scan(&cl.ChatID, &cl.AgentID, &cl.AgentName, &cl.Title,
			&cl.LatestID, &cl.LatestRole, &cl.LatestPreview, &latestAtStr, &cl.LastSeenID); err != nil {
			return nil, err
		}
		cl.LatestAt, _ = parseSQLiteTime(latestAtStr)
		// Trim long previews server-side so the wire stays small.
		if len(cl.LatestPreview) > 200 {
			cl.LatestPreview = cl.LatestPreview[:200]
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

// MarkSeen advances the chat's read watermark. Monotonic + clamped:
// the input is capped at the chat's current MAX(message id) so a buggy
// caller can't push the watermark above any real message and silently
// suppress all future unread tracking. Returns the watermark in effect
// after the call.
func (s *store) MarkSeen(chatID string, lastSeenID int64) (int64, error) {
	maxID, err := s.LatestID(chatID)
	if err != nil {
		return 0, err
	}
	if lastSeenID > maxID {
		lastSeenID = maxID
	}
	if _, err := s.db.Exec(
		`UPDATE channel_chat_chats SET last_seen_id = ?
		 WHERE id = ? AND last_seen_id < ?`,
		lastSeenID, chatID, lastSeenID,
	); err != nil {
		return 0, err
	}
	var current int64
	if err := s.db.QueryRow(
		`SELECT last_seen_id FROM channel_chat_chats WHERE id = ?`, chatID,
	).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return current, nil
}

// parseSQLiteTime — SQLite's DATETIME default writes "YYYY-MM-DD HH:MM:SS"
// (no T, no zone), but rows that flowed through Go's time.Time round-trip
// arrive as RFC3339. Try both, give up gracefully on neither (zero time
// is a fine fallback for the dashboard's relative formatting).
func parseSQLiteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}
