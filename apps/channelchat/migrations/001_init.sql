-- channel-chat: initial schema.
-- Tables are prefixed with "channel_chat_" so cross-app ownership is
-- unambiguous when the app is later extracted into its own repo.

CREATE TABLE IF NOT EXISTS channel_chat_chats (
    id           TEXT    PRIMARY KEY,
    instance_id  INTEGER NOT NULL,
    title        TEXT    NOT NULL DEFAULT 'Chat',
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channel_chat_chats_instance
    ON channel_chat_chats(instance_id);

-- Autoincrement id is the ordering primitive — never reset, never
-- reused, strictly monotonic. SSE clients reconnect with since=<last_id>
-- and get the exact gap.
CREATE TABLE IF NOT EXISTS channel_chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    TEXT    NOT NULL REFERENCES channel_chat_chats(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK(role IN ('user','agent','system')),
    content    TEXT    NOT NULL,
    user_id    INTEGER,           -- NULL for agent/system
    thread_id  TEXT,               -- which agent thread replied
    status     TEXT    NOT NULL DEFAULT 'final' CHECK(status IN ('streaming','final')),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channel_chat_messages_chat_id
    ON channel_chat_messages(chat_id, id);
