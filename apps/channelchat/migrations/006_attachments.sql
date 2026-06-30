-- User-supplied chat attachment metadata. The dashboard may send image
-- data URLs with a message so the server can forward them to core, but
-- the bytes are intentionally not persisted here.

ALTER TABLE channel_chat_messages ADD COLUMN attachments_json TEXT NOT NULL DEFAULT '[]';
