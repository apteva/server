-- tasks: initial schema.
-- Tables are prefixed with "tasks_" so cross-app ownership is
-- unambiguous when the app is later extracted into its own repo.

CREATE TABLE IF NOT EXISTS tasks_tasks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id     INTEGER NOT NULL,
    title           TEXT    NOT NULL,
    description     TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL DEFAULT 'open'
                    CHECK(status IN ('open','in_progress','blocked','done','cancelled')),
    -- Thread assigned to work on this task. NULL = unassigned / main.
    assigned_thread TEXT,
    -- Sub-tasking: a task may be spawned by another task.
    parent_task_id  INTEGER REFERENCES tasks_tasks(id) ON DELETE CASCADE,
    -- Creator provenance. Exactly one of these is set.
    created_by_thread TEXT,
    created_by_user   INTEGER,
    -- Game-like fields.
    reward_xp   INTEGER NOT NULL DEFAULT 0,
    progress    INTEGER NOT NULL DEFAULT 0,   -- 0..100
    note        TEXT    NOT NULL DEFAULT '',  -- latest status note from agent
    -- Timestamps.
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_tasks_tasks_instance_status
    ON tasks_tasks(instance_id, status, id);
CREATE INDEX IF NOT EXISTS idx_tasks_tasks_parent
    ON tasks_tasks(parent_task_id);
