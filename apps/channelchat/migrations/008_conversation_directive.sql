-- channel-chat v8: durable instructions scoped to one conversation.
-- The value is composed ahead of the protected server-owned chat policy;
-- it never replaces that policy or the agent's inherited global directive.

ALTER TABLE channel_chat_chats
ADD COLUMN directive TEXT NOT NULL DEFAULT '';
