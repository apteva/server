-- channel-chat v7: general conversations with one or more agent participants.
--
-- Existing channel_chat_chats.agent_id remains the lead agent for backwards
-- compatibility. New code uses the participants table for membership and the
-- lead when a room message does not explicitly address another participant.
-- Column additions are performed by applyMigration007 after checking the live
-- schema, allowing this migration to recover databases whose first attempt was
-- interrupted after SQLite committed some DDL but before v7 was recorded.

UPDATE channel_chat_chats
SET project_id = COALESCE((SELECT project_id FROM agents WHERE agents.id = channel_chat_chats.agent_id), ''),
    owner_user_id = COALESCE((SELECT user_id FROM agents WHERE agents.id = channel_chat_chats.agent_id), 0),
    title = CASE
        WHEN title = 'Chat' AND id = 'default-' || agent_id
        THEN COALESCE((SELECT name FROM agents WHERE agents.id = channel_chat_chats.agent_id), title)
        ELSE title
    END;

CREATE TABLE IF NOT EXISTS channel_chat_participants (
    chat_id      TEXT    NOT NULL REFERENCES channel_chat_chats(id) ON DELETE CASCADE,
    agent_id     INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    is_lead      INTEGER NOT NULL DEFAULT 0,
    joined_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (chat_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_chat_participants_agent
    ON channel_chat_participants(agent_id, chat_id);

INSERT OR IGNORE INTO channel_chat_participants (chat_id, agent_id, is_lead)
SELECT c.id, c.agent_id, 1
FROM channel_chat_chats c
JOIN agents a ON a.id = c.agent_id;

UPDATE channel_chat_messages
SET agent_id = (
    SELECT c.agent_id FROM channel_chat_chats c WHERE c.id = channel_chat_messages.chat_id
)
WHERE role = 'agent' AND agent_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_chat_client_message
    ON channel_chat_messages(chat_id, user_id, client_message_id)
    WHERE client_message_id <> '';

CREATE TABLE IF NOT EXISTS channel_chat_deliveries (
    message_id    INTEGER NOT NULL REFERENCES channel_chat_messages(id) ON DELETE CASCADE,
    agent_id      INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    status        TEXT    NOT NULL DEFAULT 'pending'
                  CHECK(status IN ('pending', 'delivered', 'failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT    NOT NULL DEFAULT '',
    delivered_at DATETIME,
    updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (message_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_chat_deliveries_pending
    ON channel_chat_deliveries(status, updated_at);

CREATE INDEX IF NOT EXISTS idx_channel_chat_chats_project
    ON channel_chat_chats(owner_user_id, project_id, archived_at, updated_at);
