-- channel-chat v2: per-chat read watermark.
-- last_seen_id holds the highest message id any viewer has acknowledged
-- for this chat. Computed by /unread-summary as
-- unread = max(0, latest_id - last_seen_id) and updated by POST /seen.
-- Single-column addition: today every chat has one owner so a single
-- watermark is sufficient. If/when we add multi-user shared chats,
-- migrate this into a separate (user_id, chat_id, last_seen_id) table.

ALTER TABLE channel_chat_chats
    ADD COLUMN last_seen_id INTEGER NOT NULL DEFAULT 0;
