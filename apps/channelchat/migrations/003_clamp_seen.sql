-- channel-chat v3: rescue inflated last_seen_id values.
-- An earlier build wrote Number.MAX_SAFE_INTEGER (9007199254740991) as
-- a "fully read" sentinel from the dashboard. Once persisted, the
-- monotonic-only UPDATE in MarkSeen prevented any later real id from
-- ever beating it, so unread tracking went silent forever.
--
-- This migration clamps every chat's last_seen_id down to the actual
-- MAX(id) of its messages (or 0 if none). Idempotent — once clamped,
-- re-running is a no-op since last_seen_id will already be ≤ MAX.

UPDATE channel_chat_chats
SET last_seen_id = COALESCE(
    (SELECT MAX(id) FROM channel_chat_messages WHERE chat_id = channel_chat_chats.id),
    0
)
WHERE last_seen_id > COALESCE(
    (SELECT MAX(id) FROM channel_chat_messages WHERE chat_id = channel_chat_chats.id),
    0
);
