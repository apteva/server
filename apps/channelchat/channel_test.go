package channelchat

import (
	"database/sql"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
	_ "modernc.org/sqlite"
)

func TestChatChannelReportAndAlertPublishToUserStream(t *testing.T) {
	db := openChannelTestDB(t, false)
	defer db.Close()

	st := newStore(db)
	h := newHub()
	userCh, _, cancel := h.subscribeUser(99)
	defer cancel()
	ch := &chatChannel{
		chatID: defaultChatID(285),
		userID: 99,
		store:  st,
		hub:    h,
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatalf("EnsureDefaultChat: %v", err)
	}

	if _, err := ch.SendReport(framework.ReportRequest{Title: "Pantry check", Summary: "Milk is on the list."}); err != nil {
		t.Fatalf("SendReport: %v", err)
	}
	report := readUserMessage(t, userCh)
	if got := report.Components[0].Name; got != "report-card" {
		t.Fatalf("report component=%q, want report-card", got)
	}

	if err := ch.Status("Pantry failed: auth expired", "warning"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	alert := readUserMessage(t, userCh)
	if got := alert.Components[0].Name; got != "alert-card" {
		t.Fatalf("alert component=%q, want alert-card", got)
	}
}

func TestInboxDismissFiltersMessagesAndPersistsProps(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Test Agent', 'default')`); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatalf("EnsureDefaultChat: %v", err)
	}
	msg, err := st.Append(defaultChatID(285), "agent", "Report: Pantry check", nil, "main", "final", []framework.ChatComponent{{
		App:  "channel-chat",
		Name: "report-card",
		Props: map[string]any{
			"title":   "Pantry check",
			"summary": "Milk is on the list.",
		},
	}})
	if err != nil {
		t.Fatalf("append report: %v", err)
	}
	before, err := st.ListReportMessages([]int64{285}, "default", 20)
	if err != nil {
		t.Fatalf("ListReportMessages before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("reports before dismiss=%d, want 1", len(before))
	}
	components, err := applyInboxDismiss(msg.Components, 99)
	if err != nil {
		t.Fatalf("applyInboxDismiss: %v", err)
	}
	updated, err := st.UpdateMessageComponents(msg.ID, components)
	if err != nil {
		t.Fatalf("UpdateMessageComponents: %v", err)
	}
	props := updated.Components[0].Props
	if props["dismissed"] != true || props["dismissed_at"] == "" || props["dismissed_by"] == nil {
		t.Fatalf("dismiss props not persisted: %#v", props)
	}
	after, err := st.ListReportMessages([]int64{285}, "default", 20)
	if err != nil {
		t.Fatalf("ListReportMessages after: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("reports after dismiss=%d, want 0", len(after))
	}
}

func openChannelTestDB(t *testing.T, withAgents bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE channel_chat_chats (
			id TEXT PRIMARY KEY,
			agent_id INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT 'Chat',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen_id INTEGER NOT NULL DEFAULT 0,
			thread_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE channel_chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			user_id INTEGER,
			thread_id TEXT,
			status TEXT NOT NULL DEFAULT 'final',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			components_json TEXT NOT NULL DEFAULT '[]',
			attachments_json TEXT NOT NULL DEFAULT '[]'
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	if withAgents {
		if _, err := db.Exec(`
			CREATE TABLE agents (
				id INTEGER PRIMARY KEY,
				user_id INTEGER NOT NULL,
				name TEXT NOT NULL,
				project_id TEXT NOT NULL
			);
		`); err != nil {
			db.Close()
			t.Fatalf("create agents schema: %v", err)
		}
	}
	return db
}

func readUserMessage(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for user-stream message")
		return Message{}
	}
}
