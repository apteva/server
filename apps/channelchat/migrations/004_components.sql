-- Components attached to a chat message. The agent calls
-- respond(text=…, channel="chat", components=[…]) on the chat MCP
-- and the chat channel writes the components_json blob alongside
-- the text. The dashboard reads it back on stream / list and mounts
-- each entry as a rich attachment under the message bubble.
--
-- JSON shape: [{"app": "storage", "name": "file-card", "props": {…}}]
-- Empty array (no attachments) is the common case; we don't normalise
-- to NULL because that lets us scan into a TEXT field without sql.NullString.
ALTER TABLE channel_chat_messages ADD COLUMN components_json TEXT NOT NULL DEFAULT '[]';
