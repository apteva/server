-- status: per-instance agent-authored status line. One row per instance;
-- upsert semantics. Short-horizon (minutes-to-hours) counterpart to the
-- long-lived directive.
CREATE TABLE IF NOT EXISTS status_status (
    instance_id   INTEGER PRIMARY KEY,
    message       TEXT    NOT NULL,
    emoji         TEXT    NOT NULL DEFAULT '',
    tone          TEXT    NOT NULL DEFAULT 'info'
                  CHECK(tone IN ('info','working','warn','error','success','idle')),
    set_by_thread TEXT,
    updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);
