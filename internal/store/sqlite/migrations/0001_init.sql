-- 初始表结构：只保存任务、入站回复与回复队列的元数据和正文摘要，不含任何正文字段（D2）。
CREATE TABLE tasks (
    id          TEXT PRIMARY KEY CHECK (length(id) = 10),
    owner       TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL CHECK (agent IN ('codex', 'claude')),
    session_id  TEXT CHECK (session_id IS NULL OR length(session_id) BETWEEN 1 AND 200),
    state       TEXT NOT NULL CHECK (state IN ('CREATED', 'RUNNING', 'WAITING_INPUT', 'WAITING_APPROVAL', 'COMPLETED', 'FAILED', 'DELIVERY_UNCERTAIN', 'CLOSED')),
    version     INTEGER NOT NULL CHECK (version >= 1),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE task_events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    event       TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX task_events_by_task ON task_events (task_id, seq);

CREATE TABLE inbound_messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT NOT NULL CHECK (length(message_id) BETWEEN 3 AND 998),
    body_sha256   BLOB NOT NULL CHECK (length(body_sha256) = 32),
    task_id       TEXT NOT NULL REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, uid_validity, uid),
    UNIQUE (account, message_id)
) STRICT;

CREATE TABLE replies (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    inbound_id     INTEGER NOT NULL UNIQUE REFERENCES inbound_messages (id),
    task_id        TEXT NOT NULL REFERENCES tasks (id),
    state          TEXT NOT NULL CHECK (state IN ('QUEUED', 'DISPATCHING', 'ACKNOWLEDGED', 'UNCERTAIN', 'REJECTED')),
    resume_state   TEXT CHECK (resume_state IS NULL OR resume_state IN ('COMPLETED', 'WAITING_INPUT')),
    reject_reason  TEXT CHECK (reject_reason IS NULL OR reject_reason IN ('task_not_accepting', 'task_failed', 'task_closed')),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX replies_one_in_flight ON replies (task_id) WHERE state IN ('DISPATCHING', 'UNCERTAIN');
CREATE INDEX replies_by_task_state ON replies (task_id, state, seq);
