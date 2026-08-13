package channelchat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/apteva/server/apps/framework"
	_ "modernc.org/sqlite"
)

func TestMigration007RecoversUnrecordedPartialSchema(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "partial-v7.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE agents (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			project_id TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatal(err)
	}

	migrations := New(nil).Migrations()
	if err := framework.RunMigrations(db, "channel-chat", migrations[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Media Agent', 'media');
		INSERT INTO channel_chat_chats (id, agent_id, title) VALUES ('default-285', 285, 'Chat');
		INSERT INTO channel_chat_chats (id, agent_id, title) VALUES ('default-999', 999, 'Deleted agent history');
		INSERT INTO channel_chat_messages (chat_id, role, content) VALUES ('default-285', 'agent', 'Ready');
		ALTER TABLE channel_chat_chats ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE channel_chat_messages ADD COLUMN agent_id INTEGER;
	`); err != nil {
		t.Fatal(err)
	}

	if err := framework.RunMigrations(db, "channel-chat", migrations); err != nil {
		t.Fatalf("recover partial v7: %v", err)
	}
	if err := framework.RunMigrations(db, "channel-chat", migrations); err != nil {
		t.Fatalf("rerun completed v7: %v", err)
	}

	for table, columns := range map[string][]string{
		"channel_chat_chats":    {"project_id", "owner_user_id", "kind", "archived_at", "directive", "subject_type", "subject_id", "conversation_key"},
		"channel_chat_messages": {"agent_id", "metadata_json", "client_message_id"},
	} {
		for _, column := range columns {
			if !testColumnExists(t, db, table, column) {
				t.Fatalf("missing recovered column %s.%s", table, column)
			}
		}
	}
	var projectID string
	var ownerUserID int64
	if err := db.QueryRow(`SELECT project_id, owner_user_id FROM channel_chat_chats WHERE id='default-285'`).Scan(&projectID, &ownerUserID); err != nil {
		t.Fatal(err)
	}
	if projectID != "media" || ownerUserID != 99 {
		t.Fatalf("chat backfill project=%q owner=%d", projectID, ownerUserID)
	}
	var participants, deliveries, version int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_participants WHERE chat_id='default-285' AND agent_id=285 AND is_lead=1`).Scan(&participants); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='channel_chat_deliveries'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT MAX(version) FROM framework_app_versions WHERE app_slug='channel-chat'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if participants != 1 || deliveries != 1 || version != 9 {
		t.Fatalf("participants=%d deliveries=%d version=%d", participants, deliveries, version)
	}
	var orphanParticipants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_participants WHERE chat_id='default-999'`).Scan(&orphanParticipants); err != nil {
		t.Fatal(err)
	}
	if orphanParticipants != 0 {
		t.Fatalf("orphan chat received %d invalid participants", orphanParticipants)
	}
}

func testColumnExists(t *testing.T, db *sql.DB, table, want string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == want {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}
