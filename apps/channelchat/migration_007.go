package channelchat

import (
	"database/sql"
	"fmt"

	"github.com/apteva/server/apps/framework"
)

func applyMigration007(tx *framework.MigrationTx) error {
	columns := []struct {
		table string
		name  string
		ddl   string
	}{
		{"channel_chat_chats", "project_id", `ALTER TABLE channel_chat_chats ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`},
		{"channel_chat_chats", "owner_user_id", `ALTER TABLE channel_chat_chats ADD COLUMN owner_user_id INTEGER NOT NULL DEFAULT 0`},
		{"channel_chat_chats", "kind", `ALTER TABLE channel_chat_chats ADD COLUMN kind TEXT NOT NULL DEFAULT 'direct'`},
		{"channel_chat_chats", "archived_at", `ALTER TABLE channel_chat_chats ADD COLUMN archived_at DATETIME`},
		{"channel_chat_messages", "agent_id", `ALTER TABLE channel_chat_messages ADD COLUMN agent_id INTEGER`},
		{"channel_chat_messages", "metadata_json", `ALTER TABLE channel_chat_messages ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}'`},
		{"channel_chat_messages", "client_message_id", `ALTER TABLE channel_chat_messages ADD COLUMN client_message_id TEXT NOT NULL DEFAULT ''`},
	}
	for _, column := range columns {
		exists, err := sqliteColumnExists(tx, column.table, column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(column.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", column.table, column.name, err)
		}
	}
	if _, err := tx.Exec(migration007); err != nil {
		return fmt.Errorf("apply conversation schema: %w", err)
	}
	return nil
}

func sqliteColumnExists(tx *framework.MigrationTx, table, column string) (bool, error) {
	var pragma string
	switch table {
	case "channel_chat_chats":
		pragma = `PRAGMA table_info(channel_chat_chats)`
	case "channel_chat_messages":
		pragma = `PRAGMA table_info(channel_chat_messages)`
	default:
		return false, fmt.Errorf("unsupported migration table %q", table)
	}
	rows, err := tx.Query(pragma)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			defaultSQL  sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &defaultSQL, &pk); err != nil {
			return false, fmt.Errorf("scan %s schema: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect %s schema: %w", table, err)
	}
	return false, nil
}
