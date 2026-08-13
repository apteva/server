-- channel-chat v9: durable external-subject identity and idempotent
-- conversation keys for delegated website/application chat clients.
--
-- Empty values identify ordinary first-party dashboard conversations. The
-- partial unique index deliberately excludes them so dashboard users may keep
-- creating multiple conversations with the same agent.

ALTER TABLE channel_chat_chats
ADD COLUMN subject_type TEXT NOT NULL DEFAULT '';

ALTER TABLE channel_chat_chats
ADD COLUMN subject_id TEXT NOT NULL DEFAULT '';

ALTER TABLE channel_chat_chats
ADD COLUMN conversation_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_chat_external_conversation
ON channel_chat_chats (
    owner_user_id,
    project_id,
    agent_id,
    subject_type,
    subject_id,
    conversation_key
)
WHERE subject_type <> ''
  AND subject_id <> ''
  AND conversation_key <> '';

CREATE INDEX IF NOT EXISTS idx_channel_chat_external_subject
ON channel_chat_chats (
    owner_user_id,
    project_id,
    subject_type,
    subject_id,
    updated_at
)
WHERE subject_type <> '' AND subject_id <> '';
