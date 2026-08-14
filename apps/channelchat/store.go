package channelchat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	AgentID   *int64    `json:"agent_id,omitempty"`
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
	Metadata    map[string]any   `json:"metadata,omitempty"`
	ClientID    string           `json:"client_message_id,omitempty"`
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

type agentConversationThreads struct {
	AgentID   int64
	UserID    int64
	ThreadIDs map[string]struct{}
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
	Message   Message   `json:"message"`
	AgentID   int64     `json:"instance_id"`
	AgentName string    `json:"instance_name"`
	ProjectID string    `json:"project_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	State     string    `json:"state"`
	Progress  *float64  `json:"progress,omitempty"`
	Next      string    `json:"next,omitempty"`
	NextAt    string    `json:"next_at,omitempty"`
	Stale     bool      `json:"stale"`
}

// Chat is one durable Apteva conversation. AgentID remains the lead agent for
// backwards compatibility; AgentIDs is the complete participant list.
type Chat struct {
	ID              string     `json:"id"`
	AgentID         int64      `json:"agent_id"`
	InstanceID      int64      `json:"instance_id,omitempty"` // legacy response alias
	AgentIDs        []int64    `json:"agent_ids"`
	ProjectID       string     `json:"project_id"`
	OwnerUserID     int64      `json:"owner_user_id,omitempty"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	Directive       string     `json:"directive,omitempty"`
	SubjectType     string     `json:"subject_type,omitempty"`
	SubjectID       string     `json:"subject_id,omitempty"`
	ConversationKey string     `json:"conversation_key,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ArchivedAt      *time.Time `json:"archived_at,omitempty"`
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

type pendingDelivery struct {
	Message Message
	Chat    Chat
	AgentID int64
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

// EnsureDefaultChat returns the agent's internal operator-inbox record or
// creates it. The stable default-<agent_id> id is reserved for main-thread
// reports, alerts, approvals, and status. It is not a user conversation and
// must never be returned by the user-facing conversation queries below.
func (s *store) EnsureDefaultChat(agentID int64) (*Chat, error) {
	chatID := defaultChatID(agentID)
	// Try insert-or-ignore and then read back. Cheaper than
	// select-then-insert and race-safe.
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO channel_chat_chats
			(id, agent_id, title, project_id, owner_user_id, kind)
		 VALUES (?, ?, 'Chat', '', 0, 'direct')`,
		chatID, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("ensure default chat: %w", err)
	}
	_, _ = s.db.Exec(`
		UPDATE channel_chat_chats
		SET project_id=COALESCE((SELECT project_id FROM agents WHERE id=?), project_id),
		    owner_user_id=COALESCE((SELECT user_id FROM agents WHERE id=?), owner_user_id),
		    title=CASE WHEN title='Chat' THEN COALESCE((SELECT name FROM agents WHERE id=?), title) ELSE title END
		WHERE id=?`, agentID, agentID, agentID, chatID)
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO channel_chat_participants (chat_id, agent_id, is_lead)
		VALUES (?, ?, 1)`, chatID, agentID); err != nil {
		return nil, fmt.Errorf("ensure default chat participant: %w", err)
	}
	return s.GetChat(chatID)
}

func defaultChatID(agentID int64) string {
	return fmt.Sprintf("default-%d", agentID)
}

func (s *store) GetChat(id string) (*Chat, error) {
	c, err := s.scanChat(s.db.QueryRow(
		`SELECT id, agent_id, title, created_at, updated_at, thread_id,
		        COALESCE(project_id, ''), COALESCE(owner_user_id, 0),
		        COALESCE(kind, 'direct'), archived_at, COALESCE(directive, ''),
		        COALESCE(subject_type, ''), COALESCE(subject_id, ''), COALESCE(conversation_key, '')
		 FROM channel_chat_chats WHERE id = ?`, id,
	))
	if err != nil {
		return nil, err
	}
	c.InstanceID = c.AgentID
	if err := s.loadChatParticipants(c); err != nil {
		return nil, err
	}
	return c, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *store) scanChat(row rowScanner) (*Chat, error) {
	var c Chat
	var archived sql.NullTime
	err := row.Scan(&c.ID, &c.AgentID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ThreadID,
		&c.ProjectID, &c.OwnerUserID, &c.Kind, &archived, &c.Directive,
		&c.SubjectType, &c.SubjectID, &c.ConversationKey)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if archived.Valid {
		v := archived.Time
		c.ArchivedAt = &v
	}
	return &c, nil
}

func (s *store) ListChatsForAgent(agentID int64) ([]Chat, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.agent_id, c.title, c.created_at, c.updated_at, c.thread_id,
		        COALESCE(c.project_id, ''), COALESCE(c.owner_user_id, 0),
		        COALESCE(c.kind, 'direct'), c.archived_at, COALESCE(c.directive, ''),
		        COALESCE(c.subject_type, ''), COALESCE(c.subject_id, ''), COALESCE(c.conversation_key, '')
		 FROM channel_chat_chats c
		 JOIN channel_chat_participants p ON p.chat_id = c.id
		 WHERE p.agent_id = ? AND c.archived_at IS NULL
		   AND c.id <> printf('default-%d', c.agent_id)
		 ORDER BY c.updated_at DESC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		c, err := s.scanChat(rows)
		if err != nil {
			return nil, err
		}
		if err := s.loadChatParticipants(c); err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListConversations returns the user-visible project conversation list.
// default-* rows are internal operator-inbox records, never conversations;
// includePrimaryChatID is retained only for source compatibility with older
// callers and deliberately cannot opt an internal row into the result.
func (s *store) ListConversations(ownerUserID int64, projectID string, includeArchived bool, includePrimaryChatID string) ([]Chat, error) {
	_ = includePrimaryChatID
	query := `SELECT id, agent_id, title, created_at, updated_at, thread_id,
	                 COALESCE(project_id, ''), COALESCE(owner_user_id, 0),
	                 COALESCE(kind, 'direct'), archived_at, COALESCE(directive, ''),
	                 COALESCE(subject_type, ''), COALESCE(subject_id, ''), COALESCE(conversation_key, '')
	          FROM channel_chat_chats c
	          WHERE owner_user_id = ? AND project_id = ?
	            AND c.id <> printf('default-%d', c.agent_id)`
	if !includeArchived {
		query += ` AND c.archived_at IS NULL`
	}
	query += ` ORDER BY c.updated_at DESC`
	rows, err := s.db.Query(query, ownerUserID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		c, err := s.scanChat(rows)
		if err != nil {
			return nil, err
		}
		if err := s.loadChatParticipants(c); err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *store) CreateConversation(ownerUserID int64, projectID, title string, agentIDs []int64, leadAgentID int64, directive ...string) (*Chat, error) {
	if len(agentIDs) == 0 {
		return nil, fmt.Errorf("at least one agent required")
	}
	if leadAgentID == 0 {
		leadAgentID = agentIDs[0]
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New conversation"
	}
	id, err := conversationID()
	if err != nil {
		return nil, err
	}
	kind := "direct"
	if len(agentIDs) > 1 {
		kind = "room"
	}
	conversationDirective := ""
	if len(directive) > 0 {
		conversationDirective = strings.TrimSpace(directive[0])
	}
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO channel_chat_chats
				(id, agent_id, title, project_id, owner_user_id, kind, directive)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, leadAgentID, title, projectID, ownerUserID, kind, conversationDirective); err != nil {
			return err
		}
		for _, agentID := range agentIDs {
			isLead := 0
			if agentID == leadAgentID {
				isLead = 1
			}
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO channel_chat_participants (chat_id, agent_id, is_lead)
				VALUES (?, ?, ?)`, id, agentID, isLead); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetChat(id)
}

// CreateOrResumeSubjectConversation atomically creates the one durable
// conversation identified by an external subject and conversation key, or
// returns the existing row. Ordinary dashboard conversations do not use this
// path and remain unconstrained by the external-subject unique index.
func (s *store) CreateOrResumeSubjectConversation(ownerUserID int64, projectID, title string, agentID int64, directive, subjectType, subjectID, conversationKey string) (*Chat, bool, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New conversation"
	}
	directive = strings.TrimSpace(directive)
	subjectType = strings.TrimSpace(subjectType)
	subjectID = strings.TrimSpace(subjectID)
	conversationKey = strings.TrimSpace(conversationKey)
	if ownerUserID <= 0 || strings.TrimSpace(projectID) == "" || agentID <= 0 || subjectType == "" || subjectID == "" || conversationKey == "" {
		return nil, false, fmt.Errorf("external conversation identity is incomplete")
	}
	id, err := conversationID()
	if err != nil {
		return nil, false, err
	}
	created := false
	resolvedID := ""
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO channel_chat_chats
				(id, agent_id, title, project_id, owner_user_id, kind, directive,
				 subject_type, subject_id, conversation_key)
			VALUES (?, ?, ?, ?, ?, 'direct', ?, ?, ?, ?)`,
			id, agentID, title, projectID, ownerUserID, directive, subjectType, subjectID, conversationKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			created = true
			resolvedID = id
			_, err = conn.ExecContext(ctx, `
				INSERT INTO channel_chat_participants (chat_id, agent_id, is_lead)
				VALUES (?, ?, 1)`, id, agentID)
			return err
		}
		return conn.QueryRowContext(ctx, `
			SELECT id FROM channel_chat_chats
			WHERE owner_user_id = ? AND project_id = ? AND agent_id = ?
			  AND subject_type = ? AND subject_id = ? AND conversation_key = ?`,
			ownerUserID, projectID, agentID, subjectType, subjectID, conversationKey).Scan(&resolvedID)
	})
	if err != nil {
		return nil, false, err
	}
	chat, err := s.GetChat(resolvedID)
	return chat, created, err
}

// ListChatsForAgentSubject is the delegated-client collection view. It never
// returns another external subject's conversations or first-party dashboard
// conversations.
func (s *store) ListChatsForAgentSubject(agentID int64, subjectType, subjectID string) ([]Chat, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.agent_id, c.title, c.created_at, c.updated_at, c.thread_id,
		        COALESCE(c.project_id, ''), COALESCE(c.owner_user_id, 0),
		        COALESCE(c.kind, 'direct'), c.archived_at, COALESCE(c.directive, ''),
		        COALESCE(c.subject_type, ''), COALESCE(c.subject_id, ''), COALESCE(c.conversation_key, '')
		 FROM channel_chat_chats c
		 JOIN channel_chat_participants p ON p.chat_id = c.id
		 WHERE p.agent_id = ? AND c.subject_type = ? AND c.subject_id = ?
		   AND c.archived_at IS NULL
		 ORDER BY c.updated_at DESC`, agentID, strings.TrimSpace(subjectType), strings.TrimSpace(subjectID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		chat, err := s.scanChat(rows)
		if err != nil {
			return nil, err
		}
		if err := s.loadChatParticipants(chat); err != nil {
			return nil, err
		}
		out = append(out, *chat)
	}
	return out, rows.Err()
}

func (s *store) UpdateConversation(id, title string, archived *bool, directive *string) (*Chat, error) {
	if strings.TrimSpace(title) != "" {
		if _, err := s.db.Exec(`UPDATE channel_chat_chats SET title=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, strings.TrimSpace(title), id); err != nil {
			return nil, err
		}
	}
	if archived != nil {
		if *archived {
			_, err := s.db.Exec(`UPDATE channel_chat_chats SET archived_at=CURRENT_TIMESTAMP WHERE id=?`, id)
			if err != nil {
				return nil, err
			}
		} else {
			_, err := s.db.Exec(`UPDATE channel_chat_chats SET archived_at=NULL WHERE id=?`, id)
			if err != nil {
				return nil, err
			}
		}
	}
	if directive != nil {
		if _, err := s.db.Exec(`UPDATE channel_chat_chats SET directive=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, strings.TrimSpace(*directive), id); err != nil {
			return nil, err
		}
	}
	return s.GetChat(id)
}

func (s *store) DeleteConversation(id string) error {
	res, err := s.db.Exec(`DELETE FROM channel_chat_chats WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AgentConversationThreads returns every agent, including agents with no
// conversations, together with the chat thread ids still backed by a row and
// participant relationship. Including empty owners is essential: those are
// exactly the agents for which every persisted chat-conv-* thread is orphaned.
func (s *store) AgentConversationThreads() ([]agentConversationThreads, error) {
	rows, err := s.db.Query(`SELECT id, user_id FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	owners := make([]agentConversationThreads, 0)
	byAgent := make(map[int64]int)
	for rows.Next() {
		var owner agentConversationThreads
		if err := rows.Scan(&owner.AgentID, &owner.UserID); err != nil {
			rows.Close()
			return nil, err
		}
		owner.ThreadIDs = make(map[string]struct{})
		owners = append(owners, owner)
		byAgent[owner.AgentID] = len(owners) - 1
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	threadRows, err := s.db.Query(`
		SELECT DISTINCT p.agent_id, c.thread_id
		FROM channel_chat_participants p
		JOIN channel_chat_chats c ON c.id = p.chat_id
		WHERE TRIM(COALESCE(c.thread_id, '')) LIKE 'chat-conv-%'
		UNION
		SELECT DISTINCT c.agent_id, c.thread_id
		FROM channel_chat_chats c
		WHERE TRIM(COALESCE(c.thread_id, '')) LIKE 'chat-conv-%'`)
	if err != nil {
		return nil, err
	}
	defer threadRows.Close()
	for threadRows.Next() {
		var agentID int64
		var threadID string
		if err := threadRows.Scan(&agentID, &threadID); err != nil {
			return nil, err
		}
		if index, exists := byAgent[agentID]; exists {
			owners[index].ThreadIDs[strings.TrimSpace(threadID)] = struct{}{}
		}
	}
	return owners, threadRows.Err()
}

// UnusedConversationThreads returns conversation rows whose Core thread was
// assigned even though the user never sent a message. Older presence handling
// created these merely by opening a dashboard page. The conversation row is
// preserved; only its unnecessary runtime thread is eligible for cleanup.
func (s *store) UnusedConversationThreads() ([]Chat, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, title, created_at, updated_at, thread_id,
		       COALESCE(project_id, ''), COALESCE(owner_user_id, 0),
		       COALESCE(kind, 'direct'), archived_at, COALESCE(directive, ''),
		       COALESCE(subject_type, ''), COALESCE(subject_id, ''), COALESCE(conversation_key, '')
		FROM channel_chat_chats c
		WHERE TRIM(COALESCE(thread_id, '')) <> ''
		  AND NOT EXISTS (
		    SELECT 1 FROM channel_chat_messages m
		    WHERE m.chat_id = c.id AND m.role = 'user'
		  )
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chats []Chat
	for rows.Next() {
		chat, err := s.scanChat(rows)
		if err != nil {
			return nil, err
		}
		if err := s.loadChatParticipants(chat); err != nil {
			return nil, err
		}
		chats = append(chats, *chat)
	}
	return chats, rows.Err()
}

func (s *store) ClearUnusedConversationThread(chatID, threadID string) error {
	_, err := s.db.Exec(`
		UPDATE channel_chat_chats
		SET thread_id = ''
		WHERE id = ? AND thread_id = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM channel_chat_messages m
		    WHERE m.chat_id = channel_chat_chats.id AND m.role = 'user'
		  )`, chatID, threadID)
	return err
}

// DeleteAgentData removes an agent from every conversation it participates in.
// Conversations with no remaining agents are deleted together with their
// messages. Shared conversations remain available to the other participants;
// a new lead is selected when necessary and historical messages from the
// deleted agent are retained without a stale agent reference.
func (s *store) DeleteAgentData(agentID int64) error {
	return s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		return s.deleteAgentData(ctx, conn, agentID)
	})
}

// CleanupOrphanedAgentData repairs rows left by server versions that deleted
// agents without notifying channel-chat. It is idempotent and intentionally
// runs at mount so upgrading fixes existing installations as well as future
// deletions.
func (s *store) CleanupOrphanedAgentData() error {
	return s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `
			DELETE FROM channel_chat_deliveries
			WHERE NOT EXISTS (
				SELECT 1 FROM agents WHERE agents.id = channel_chat_deliveries.agent_id
			)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			DELETE FROM channel_chat_participants
			WHERE NOT EXISTS (
				SELECT 1 FROM agents WHERE agents.id = channel_chat_participants.agent_id
			)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE channel_chat_messages
			SET agent_id = NULL
			WHERE agent_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM agents WHERE agents.id = channel_chat_messages.agent_id
			)`); err != nil {
			return err
		}

		rows, err := conn.QueryContext(ctx, `
			SELECT c.agent_id
			FROM channel_chat_chats c
			LEFT JOIN agents a ON a.id = c.agent_id
			WHERE a.id IS NULL
			GROUP BY c.agent_id`)
		if err != nil {
			return err
		}
		var orphanedAgentIDs []int64
		for rows.Next() {
			var agentID int64
			if err := rows.Scan(&agentID); err != nil {
				rows.Close()
				return err
			}
			orphanedAgentIDs = append(orphanedAgentIDs, agentID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, agentID := range orphanedAgentIDs {
			if err := s.deleteAgentData(ctx, conn, agentID); err != nil {
				return err
			}
		}
		return nil
	})
}

// CleanupLegacyMainConversationData removes ordinary chat replies written by
// main before internal dashboard conversations became conversation-owned.
// Main-owned Inbox artifacts remain valid and are preserved because they have
// structured components. Any chat row that itself pointed at main is migrated
// to its dedicated chat-<id> thread. The operation is idempotent and runs at
// mount so existing installations are repaired automatically.
func (s *store) CleanupLegacyMainConversationData() error {
	return s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		legacyOrdinary := `
			SELECT id FROM channel_chat_messages
			WHERE role = 'agent'
			  AND TRIM(COALESCE(thread_id, '')) = 'main'
			  AND TRIM(COALESCE(components_json, '[]')) IN ('', '[]', 'null')`
		if _, err := conn.ExecContext(ctx, `
			DELETE FROM channel_chat_deliveries
			WHERE message_id IN (`+legacyOrdinary+`)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			DELETE FROM channel_chat_messages
			WHERE id IN (`+legacyOrdinary+`)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE channel_chat_chats
			SET thread_id = 'chat-' || id
			WHERE TRIM(COALESCE(thread_id, '')) = 'main'`); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `
			UPDATE channel_chat_chats
			SET last_seen_id = COALESCE((
				SELECT MAX(m.id) FROM channel_chat_messages m
				WHERE m.chat_id = channel_chat_chats.id
			), 0)
			WHERE last_seen_id > COALESCE((
				SELECT MAX(m.id) FROM channel_chat_messages m
				WHERE m.chat_id = channel_chat_chats.id
			), 0)`)
		return err
	})
}

func (s *store) deleteAgentData(ctx context.Context, conn *sql.Conn, agentID int64) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT DISTINCT c.id, c.agent_id
		FROM channel_chat_chats c
		LEFT JOIN channel_chat_participants p
		       ON p.chat_id = c.id AND p.agent_id = ?
		WHERE c.agent_id = ? OR p.agent_id IS NOT NULL`, agentID, agentID)
	if err != nil {
		return err
	}
	type affectedChat struct {
		id     string
		leadID int64
	}
	var chats []affectedChat
	for rows.Next() {
		var chat affectedChat
		if err := rows.Scan(&chat.id, &chat.leadID); err != nil {
			rows.Close()
			return err
		}
		chats = append(chats, chat)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, chat := range chats {
		var replacementID int64
		err := conn.QueryRowContext(ctx, `
			SELECT p.agent_id
			FROM channel_chat_participants p
			JOIN agents a ON a.id = p.agent_id
			WHERE p.chat_id = ? AND p.agent_id <> ?
			ORDER BY p.is_lead DESC, p.joined_at ASC, p.agent_id ASC
			LIMIT 1`, chat.id, agentID).Scan(&replacementID)
		if err == sql.ErrNoRows {
			if _, err := conn.ExecContext(ctx, `
				DELETE FROM channel_chat_deliveries
				WHERE message_id IN (SELECT id FROM channel_chat_messages WHERE chat_id = ?)`, chat.id); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM channel_chat_messages WHERE chat_id = ?`, chat.id); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM channel_chat_participants WHERE chat_id = ?`, chat.id); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM channel_chat_chats WHERE id = ?`, chat.id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}

		if _, err := conn.ExecContext(ctx, `
			DELETE FROM channel_chat_deliveries
			WHERE agent_id = ? AND message_id IN (
				SELECT id FROM channel_chat_messages WHERE chat_id = ?
			)`, agentID, chat.id); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM channel_chat_participants WHERE chat_id = ? AND agent_id = ?`, chat.id, agentID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE channel_chat_messages SET agent_id = NULL WHERE chat_id = ? AND agent_id = ?`, chat.id, agentID); err != nil {
			return err
		}

		if chat.leadID == agentID {
			if _, err := conn.ExecContext(ctx, `
				UPDATE channel_chat_participants
				SET is_lead = CASE WHEN agent_id = ? THEN 1 ELSE 0 END
				WHERE chat_id = ?`, replacementID, chat.id); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `
				UPDATE channel_chat_chats
				SET agent_id = ?,
				    kind = CASE WHEN (
				        SELECT COUNT(*) FROM channel_chat_participants WHERE chat_id = ?
				    ) > 1 THEN 'room' ELSE 'direct' END,
				    updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`, replacementID, chat.id, chat.id); err != nil {
				return err
			}
		} else if _, err := conn.ExecContext(ctx, `
			UPDATE channel_chat_chats
			SET kind = CASE WHEN (
			    SELECT COUNT(*) FROM channel_chat_participants WHERE chat_id = ?
			) > 1 THEN 'room' ELSE 'direct' END,
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, chat.id, chat.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) AddParticipant(chatID string, agentID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO channel_chat_participants (chat_id, agent_id) VALUES (?, ?)`, chatID, agentID)
	if err == nil {
		_, err = s.db.Exec(`UPDATE channel_chat_chats SET kind='room', updated_at=CURRENT_TIMESTAMP WHERE id=?`, chatID)
	}
	return err
}

func (s *store) RemoveParticipant(chatID string, agentID int64) error {
	chat, err := s.GetChat(chatID)
	if err != nil {
		return err
	}
	if chat.AgentID == agentID {
		return fmt.Errorf("lead agent cannot be removed")
	}
	_, err = s.db.Exec(`DELETE FROM channel_chat_participants WHERE chat_id=? AND agent_id=?`, chatID, agentID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		UPDATE channel_chat_chats
		SET kind=CASE WHEN (SELECT COUNT(*) FROM channel_chat_participants WHERE chat_id=?) > 1 THEN 'room' ELSE 'direct' END,
		    updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, chatID, chatID)
	return err
}

func (s *store) loadChatParticipants(c *Chat) error {
	rows, err := s.db.Query(`SELECT agent_id FROM channel_chat_participants WHERE chat_id=? ORDER BY is_lead DESC, joined_at ASC`, c.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	c.AgentIDs = []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		c.AgentIDs = append(c.AgentIDs, id)
	}
	return rows.Err()
}

func conversationID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "conv-" + hex.EncodeToString(raw[:]), nil
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

// ChatForAgentThread resolves an outbound Channels MCP call back to the
// conversation that originated it. The primary/main path intentionally falls
// back to the stable default conversation for backwards compatibility.
func (s *store) ChatForAgentThread(agentID int64, threadID string) (*Chat, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || threadID == "main" {
		return s.EnsureDefaultChat(agentID)
	}
	c, err := s.scanChat(s.db.QueryRow(`
		SELECT c.id, c.agent_id, c.title, c.created_at, c.updated_at, c.thread_id,
		       COALESCE(c.project_id, ''), COALESCE(c.owner_user_id, 0),
		       COALESCE(c.kind, 'direct'), c.archived_at, COALESCE(c.directive, ''),
		       COALESCE(c.subject_type, ''), COALESCE(c.subject_id, ''), COALESCE(c.conversation_key, '')
		FROM channel_chat_chats c
		JOIN channel_chat_participants p ON p.chat_id = c.id
		WHERE p.agent_id = ? AND c.thread_id = ? AND c.archived_at IS NULL
		LIMIT 1`, agentID, threadID))
	if err != nil {
		return nil, err
	}
	if err := s.loadChatParticipants(c); err != nil {
		return nil, err
	}
	return c, nil
}

// SubjectForAgentThread resolves trusted external identity for an agent MCP
// call. Empty values mean the thread is an ordinary first-party conversation
// (or is not a Channel Chat thread).
func (s *store) SubjectForAgentThread(agentID int64, threadID string) (subjectType, subjectID, conversationID string, ok bool) {
	threadID = strings.TrimSpace(threadID)
	if agentID <= 0 || threadID == "" || threadID == "main" {
		return "", "", "", false
	}
	err := s.db.QueryRow(`
		SELECT c.subject_type, c.subject_id, c.id
		FROM channel_chat_chats c
		JOIN channel_chat_participants p ON p.chat_id = c.id
		WHERE p.agent_id = ? AND c.thread_id = ?
		  AND c.archived_at IS NULL
		  AND c.subject_type <> '' AND c.subject_id <> ''
		LIMIT 1`, agentID, threadID).Scan(&subjectType, &subjectID, &conversationID)
	if err != nil {
		return "", "", "", false
	}
	return subjectType, subjectID, conversationID, true
}

func (s *store) CreateDeliveries(messageID int64, agentIDs []int64) error {
	return s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		for _, agentID := range agentIDs {
			if _, err := conn.ExecContext(ctx, `
				INSERT OR IGNORE INTO channel_chat_deliveries (message_id, agent_id)
				VALUES (?, ?)`, messageID, agentID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *store) MarkDelivery(messageID, agentID int64, delivered bool, deliveryErr error) error {
	status := "delivered"
	lastError := ""
	var deliveredAt any = time.Now().UTC()
	if !delivered {
		status = "failed"
		deliveredAt = nil
		if deliveryErr != nil {
			lastError = deliveryErr.Error()
		}
		if len(lastError) > 1000 {
			lastError = lastError[:1000]
		}
	}
	_, err := s.db.Exec(`
		UPDATE channel_chat_deliveries
		SET status=?, attempts=attempts+1, last_error=?, delivered_at=?, updated_at=CURRENT_TIMESTAMP
		WHERE message_id=? AND agent_id=?`, status, lastError, deliveredAt, messageID, agentID)
	return err
}

func (s *store) ListPendingDeliveries(limit int) ([]pendingDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.Query(`
		SELECT d.message_id, d.agent_id
		FROM channel_chat_deliveries d
		WHERE d.status IN ('pending', 'failed')
		  AND d.attempts < 10
		  AND d.updated_at <= datetime('now', '-30 seconds')
		ORDER BY d.updated_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type deliveryKey struct{ messageID, agentID int64 }
	keys := []deliveryKey{}
	for rows.Next() {
		var key deliveryKey
		if err := rows.Scan(&key.messageID, &key.agentID); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]pendingDelivery, 0, len(keys))
	for _, key := range keys {
		message, err := s.GetMessage(key.messageID)
		if err != nil {
			return nil, err
		}
		chat, err := s.GetChat(message.ChatID)
		if err != nil {
			return nil, err
		}
		out = append(out, pendingDelivery{Message: *message, Chat: *chat, AgentID: key.agentID})
	}
	return out, nil
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
	return s.appendFull(chatID, role, content, userID, nil, threadID, status, components, attachments, nil, "")
}

func (s *store) AppendUserMessage(chatID, content string, userID int64, attachments []ChatAttachment, metadata map[string]any, clientID string) (*Message, error) {
	return s.appendFull(chatID, "user", content, &userID, nil, "", "final", nil, attachments, metadata, clientID)
}

// AppendUserMessageWithDeliveries commits the visible user message and its
// target-agent delivery ledger in one SQLite write. A repeated client id
// returns the existing row without scheduling a second agent event.
func (s *store) AppendUserMessageWithDeliveries(chatID, content string, userID int64, attachments []ChatAttachment, metadata map[string]any, clientID string, agentIDs []int64) (*Message, bool, error) {
	if attachments == nil {
		attachments = []ChatAttachment{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return nil, false, fmt.Errorf("marshal attachments: %w", err)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, false, fmt.Errorf("marshal metadata: %w", err)
	}
	clientID = strings.TrimSpace(clientID)
	var id int64
	inserted := false
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		if clientID != "" {
			err := conn.QueryRowContext(ctx, `
				SELECT id FROM channel_chat_messages
				WHERE chat_id=? AND user_id=? AND client_message_id=?`, chatID, userID, clientID).Scan(&id)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		result, err := conn.ExecContext(ctx, `
			INSERT INTO channel_chat_messages
				(chat_id, role, content, user_id, thread_id, status, components_json,
				 attachments_json, metadata_json, client_message_id)
			VALUES (?, 'user', ?, ?, '', 'final', '[]', ?, ?, ?)`,
			chatID, content, userID, string(attachmentsJSON), string(metadataJSON), clientID)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		for _, agentID := range agentIDs {
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO channel_chat_deliveries (message_id, agent_id)
				VALUES (?, ?)`, id, agentID); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, `UPDATE channel_chat_chats SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, chatID)
		inserted = err == nil
		return err
	})
	if err != nil {
		return nil, false, err
	}
	message, err := s.GetMessage(id)
	return message, inserted, err
}

func (s *store) AppendAgentArtifact(chatID, content string, agentID int64, threadID string, components []framework.ChatComponent) (*Message, error) {
	return s.appendFull(chatID, "agent", content, nil, &agentID, threadID, "final", components, nil, nil, "")
}

func (s *store) appendFull(chatID, role, content string, userID, agentID *int64, threadID, status string, components []framework.ChatComponent, attachments []ChatAttachment, metadata map[string]any, clientID string) (*Message, error) {
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
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	res, err := s.db.Exec(
		`INSERT INTO channel_chat_messages
			(chat_id, role, content, user_id, agent_id, thread_id, status,
			 components_json, attachments_json, metadata_json, client_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chatID, role, content, userID, agentID, threadID, status,
		string(componentsJSON), string(attachmentsJSON), string(metadataJSON), strings.TrimSpace(clientID),
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
func (s *store) AppendAgentMessageOnce(chatID, content, threadID string, agentID int64, components []framework.ChatComponent) (*Message, bool, error) {
	return s.AppendAgentMessageOnceWithMetadata(
		chatID,
		content,
		threadID,
		agentID,
		components,
		map[string]any{"phase": "final"},
	)
}

// AppendAgentMessageOnceWithMetadata persists an agent reply and includes its
// lifecycle metadata in immediate-retry detection. The same text can
// legitimately appear in different lifecycle phases, so phase must be part of
// the idempotency fingerprint.
func (s *store) AppendAgentMessageOnceWithMetadata(chatID, content, threadID string, agentID int64, components []framework.ChatComponent, metadata map[string]any) (*Message, bool, error) {
	if components == nil {
		components = []framework.ChatComponent{}
	}
	if metadata == nil {
		metadata = map[string]any{"phase": "final"}
	}
	componentsJSON, err := json.Marshal(components)
	if err != nil {
		return nil, false, fmt.Errorf("marshal components: %w", err)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, false, fmt.Errorf("marshal metadata: %w", err)
	}
	encodedComponents := string(componentsJSON)
	encodedMetadata := string(metadataJSON)
	var id int64
	inserted := false
	err = s.withImmediateWrite(func(ctx context.Context, conn *sql.Conn) error {
		var latestRole, latestContent, latestThread, latestComponents, latestMetadata string
		latestErr := conn.QueryRowContext(ctx, `
			SELECT id, role, content, COALESCE(thread_id, ''), COALESCE(components_json, '[]'),
			       COALESCE(metadata_json, '{}')
			FROM channel_chat_messages
			WHERE chat_id = ?
			  AND created_at >= datetime('now', '-5 seconds')
			  AND COALESCE(components_json, '[]') NOT LIKE '%"approval-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"report-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"alert-card"%'
			  AND COALESCE(components_json, '[]') NOT LIKE '%"status-card"%'
			ORDER BY id DESC
			LIMIT 1`, chatID).Scan(&id, &latestRole, &latestContent, &latestThread, &latestComponents, &latestMetadata)
		if latestErr == nil && latestRole == "agent" && latestThread == threadID &&
			latestComponents == encodedComponents && latestMetadata == encodedMetadata {
			exactRetry := latestContent == content
			latestFingerprint := immediateReplyFingerprint(latestContent)
			paraphrasedRetry := latestFingerprint != "" && latestFingerprint == immediateReplyFingerprint(content)
			if exactRetry || paraphrasedRetry {
				return nil
			}
		}
		if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
			return latestErr
		}

		res, err := conn.ExecContext(ctx, `
			INSERT INTO channel_chat_messages
				(chat_id, role, content, user_id, agent_id, thread_id, status,
				 components_json, attachments_json, metadata_json, client_message_id)
			VALUES (?, 'agent', ?, NULL, ?, ?, 'final', ?, '[]', ?, '')`,
			chatID, content, agentID, threadID, encodedComponents, encodedMetadata)
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

// immediateReplyFingerprint catches the common post-receipt retry where the
// model emits the same sentence with only punctuation or word order changed.
// It is intentionally conservative: short replies are exact-match only, the
// caller already limits comparison to the same thread/components and a five
// second window, and a new user row prevents the previous agent reply from
// being selected at all.
func immediateReplyFingerprint(content string) string {
	rawTokens := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(content)), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	tokens := make([]string, 0, len(rawTokens))
	for i := 0; i < len(rawTokens); i++ {
		switch rawTokens[i] {
		case "confirmed", "updated":
			// Presentation-only lead words commonly alternate after a
			// successful delivery receipt.
			continue
		case "every":
			if i+1 < len(rawTokens) && rawTokens[i+1] == "day" {
				tokens = append(tokens, "daily")
				i++
				continue
			}
		}
		tokens = append(tokens, rawTokens[i])
	}
	if len(tokens) < 6 {
		return ""
	}
	sort.Strings(tokens)
	return strings.Join(tokens, "\x00")
}

// UpsertCurrentStatus keeps exactly one mutable status row per chat. Status is
// operational state rather than conversation history, so it deliberately does
// not update channel_chat_chats.updated_at or the unread watermark.
func (s *store) UpsertCurrentStatus(chatID string, agentID int64, threadID, content string, components []framework.ChatComponent) (*Message, error) {
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
			WHERE chat_id = ? AND COALESCE(agent_id, 0) = ?
			  AND COALESCE(components_json, '[]') LIKE '%"status-card"%'
			ORDER BY id DESC LIMIT 1`, chatID, agentID).Scan(&id)
		switch {
		case err == nil:
			_, err = conn.ExecContext(ctx, `
				UPDATE channel_chat_messages
				SET role='agent', content=?, agent_id=?, thread_id=?, status='final', created_at=CURRENT_TIMESTAMP,
				    components_json=?, attachments_json='[]'
				WHERE id=?`, content, agentID, strings.TrimSpace(threadID), string(componentsJSON), id)
		case errors.Is(err, sql.ErrNoRows):
			err = conn.QueryRowContext(ctx, `
				INSERT INTO channel_chat_messages
					(chat_id, role, content, agent_id, thread_id, status, components_json,
					 attachments_json, metadata_json, client_message_id)
				VALUES (?, 'agent', ?, ?, ?, 'final', ?, '[]', '{}', '')
				RETURNING id`, chatID, content, agentID, strings.TrimSpace(threadID), string(componentsJSON)).Scan(&id)
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
	var agentID sql.NullInt64
	var threadID sql.NullString
	var componentsJSON sql.NullString
	var attachmentsJSON sql.NullString
	var metadataJSON sql.NullString
	err := s.db.QueryRow(
		`SELECT id, chat_id, role, content, user_id, agent_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]'),
		        COALESCE(metadata_json, '{}'), COALESCE(client_message_id, '')
		 FROM channel_chat_messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &agentID, &threadID, &m.Status,
		&m.CreatedAt, &componentsJSON, &attachmentsJSON, &metadataJSON, &m.ClientID)
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
	if agentID.Valid {
		v := agentID.Int64
		m.AgentID = &v
	}
	if threadID.Valid {
		m.ThreadID = threadID.String
	}
	m.Components = decodeComponents(componentsJSON.String)
	m.Attachments = decodeAttachments(attachmentsJSON.String)
	m.Metadata = decodeMetadata(metadataJSON.String)
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

func decodeMetadata(raw string) map[string]any {
	out := map[string]any{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
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
		`SELECT id, chat_id, role, content, user_id, agent_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]'),
		        COALESCE(metadata_json, '{}'), COALESCE(client_message_id, '')
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
		`SELECT id, chat_id, role, content, user_id, agent_id, thread_id, status, created_at,
		        COALESCE(components_json, '[]'), COALESCE(attachments_json, '[]'),
		        COALESCE(metadata_json, '{}'), COALESCE(client_message_id, '')
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
		var agentID sql.NullInt64
		var threadID sql.NullString
		var componentsJSON sql.NullString
		var attachmentsJSON sql.NullString
		var metadataJSON sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &userID, &agentID, &threadID,
			&m.Status, &m.CreatedAt, &componentsJSON, &attachmentsJSON, &metadataJSON, &m.ClientID); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			m.UserID = &v
		}
		if agentID.Valid {
			v := agentID.Int64
			m.AgentID = &v
		}
		if threadID.Valid {
			m.ThreadID = threadID.String
		}
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		m.Metadata = decodeMetadata(metadataJSON.String)
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
		queryLimit = inboxCandidateLimit(limit)
	}
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+3)
	for i, id := range ownerIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := `COALESCE(m.agent_id, c.agent_id) IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"approval-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, queryLimit)
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       COALESCE(m.agent_id, c.agent_id), COALESCE(i.name, ''), COALESCE(c.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = COALESCE(m.agent_id, c.agent_id)
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
		agentID := row.AgentID
		m.AgentID = &agentID
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
	where := `COALESCE(m.agent_id, c.agent_id) IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"report-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, inboxCandidateLimit(limit))
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       COALESCE(m.agent_id, c.agent_id), COALESCE(i.name, ''), COALESCE(c.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = COALESCE(m.agent_id, c.agent_id)
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
		agentID := row.AgentID
		m.AgentID = &agentID
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
		if len(out) >= limit {
			break
		}
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
	where := `COALESCE(m.agent_id, c.agent_id) IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"alert-card"%'`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	args = append(args, inboxCandidateLimit(limit))
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       COALESCE(m.agent_id, c.agent_id), COALESCE(i.name, ''), COALESCE(c.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = COALESCE(m.agent_id, c.agent_id)
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
		agentID := row.AgentID
		m.AgentID = &agentID
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
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// Inbox dismissal is stored inside component JSON, so it is evaluated after
// rows are decoded. Read a bounded surplus of candidates to ensure a recently
// dismissed card cannot consume the SQL LIMIT and hide an older visible item.
func inboxCandidateLimit(limit int) int {
	queryLimit := limit * 5
	if queryLimit < 100 {
		queryLimit = 100
	}
	if queryLimit > 500 {
		queryLimit = 500
	}
	return queryLimit
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
	where := `COALESCE(m.agent_id, c.agent_id) IN (` + strings.Join(placeholders, ",") + `)
		AND COALESCE(m.components_json, '[]') LIKE '%"status-card"%'
		AND m.chat_id = printf('default-%d', COALESCE(m.agent_id, c.agent_id))`
	if strings.TrimSpace(projectID) != "" {
		where += ` AND i.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	q := `
		SELECT m.id, m.chat_id, m.role, m.content, m.user_id, m.thread_id, m.status, m.created_at,
		       COALESCE(m.components_json, '[]'), COALESCE(m.attachments_json, '[]'),
		       COALESCE(m.agent_id, c.agent_id), COALESCE(i.name, ''), COALESCE(c.project_id, '')
		FROM channel_chat_messages m
		JOIN channel_chat_chats c ON c.id = m.chat_id
		JOIN agents i ON i.id = COALESCE(m.agent_id, c.agent_id)
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
		agentID := row.AgentID
		m.AgentID = &agentID
		m.Components = decodeComponents(componentsJSON.String)
		m.Attachments = decodeAttachments(attachmentsJSON.String)
		title, detail, state, progress, next, nextAt, ok := currentStatusSummary(m.Components)
		if !ok {
			continue
		}
		row.Message = m
		row.UpdatedAt = currentStatusUpdatedAt(m.Components, m.CreatedAt)
		row.Title, row.Detail, row.State, row.Progress, row.Next, row.NextAt = title, detail, state, progress, next, nextAt
		row.Stale = state != "completed" && now.Sub(row.UpdatedAt) > 30*time.Minute
		out = append(out, row)
	}
	return out, rows.Err()
}

func currentStatusUpdatedAt(components []framework.ChatComponent, fallback time.Time) time.Time {
	for _, c := range components {
		if c.App != "channel-chat" || c.Name != "status-card" {
			continue
		}
		value, _ := c.Props["updated_at"].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return parsed
		}
		break
	}
	return fallback
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

func currentStatusSummary(components []framework.ChatComponent) (title, detail, state string, progress *float64, next, nextAt string, ok bool) {
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
		next, _ = c.Props["next"].(string)
		nextAt, _ = c.Props["next_at"].(string)
		return title, detail, state, progress, next, nextAt, true
	}
	return "", "", "", nil, "", "", false
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
		  AND c.id <> printf('default-%d', c.agent_id)
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
