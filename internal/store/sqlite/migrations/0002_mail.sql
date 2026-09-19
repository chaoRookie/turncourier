-- 0002：邮件闭环（Phase 4a）。0001 已发布，不修改。
-- inbound_messages.body_sha256 的列名沿用 0001，自本版本起保存键控摘要 HMAC-SHA256（phase-04.md Task 8）；
-- 本迁移之前写入的行只可能来自测试或内部调用，保留原值不改写。

-- 入站记录增加文件夹：UID 只在同一文件夹内唯一（RFC 3501），唯一键改为 (account, folder, uid_validity, uid)；
-- 按 Message-ID 的唯一键不变，同一封信在 INBOX 与 Junk 的两份副本仍判为重复。迁移在事务中执行，不能关闭外键，
-- 而 replies 以外键引用 inbound_messages，因此两张表一起重建：先删子表再删父表，删除时已没有引用旧父表的行。
-- 这一段必须位于本文件开头，在任何引用这两张表的触发器之前。
CREATE TABLE inbound_messages_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT NOT NULL CHECK (length(message_id) BETWEEN 3 AND 998),
    body_sha256   BLOB NOT NULL CHECK (length(body_sha256) = 32),
    task_id       TEXT NOT NULL REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, folder, uid_validity, uid),
    UNIQUE (account, message_id)
) STRICT;

INSERT INTO inbound_messages_new (id, account, folder, uid_validity, uid, message_id, body_sha256, task_id, received_at)
SELECT id, account, 'INBOX', uid_validity, uid, message_id, body_sha256, task_id, received_at FROM inbound_messages;

CREATE TABLE replies_new (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    inbound_id     INTEGER NOT NULL UNIQUE REFERENCES inbound_messages_new (id),
    task_id        TEXT NOT NULL REFERENCES tasks (id),
    state          TEXT NOT NULL CHECK (state IN ('QUEUED', 'DISPATCHING', 'ACKNOWLEDGED', 'UNCERTAIN', 'REJECTED')),
    resume_state   TEXT CHECK (resume_state IS NULL OR resume_state IN ('COMPLETED', 'WAITING_INPUT')),
    reject_reason  TEXT CHECK (reject_reason IS NULL OR reject_reason IN ('task_not_accepting', 'task_failed', 'task_closed')),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

INSERT INTO replies_new (seq, inbound_id, task_id, state, resume_state, reject_reason, created_at, updated_at)
SELECT seq, inbound_id, task_id, state, resume_state, reject_reason, created_at, updated_at FROM replies;

-- 保留 AUTOINCREMENT 序列：显式插入只会把新表的序列设为现有最大 id；旧表从未有过行时 sqlite_sequence 中没有它的记录，不复制。
DELETE FROM sqlite_sequence WHERE name IN ('inbound_messages_new', 'replies_new');
INSERT INTO sqlite_sequence (name, seq)
SELECT name || '_new', seq FROM sqlite_sequence WHERE name IN ('inbound_messages', 'replies');

DROP TABLE replies;
DROP TABLE inbound_messages;
ALTER TABLE inbound_messages_new RENAME TO inbound_messages;
ALTER TABLE replies_new RENAME TO replies;

CREATE UNIQUE INDEX replies_one_in_flight ON replies (task_id) WHERE state IN ('DISPATCHING', 'UNCERTAIN');
CREATE INDEX replies_by_task_state ON replies (task_id, state, seq);

CREATE TABLE instance (
    singleton    INTEGER PRIMARY KEY CHECK (singleton = 1),
    instance_id  TEXT NOT NULL CHECK (length(instance_id) = 16 AND instance_id NOT GLOB '*[^0-9abcdefghjkmnpqrstvwxyz]*'),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE crypto_keys (
    purpose     TEXT NOT NULL CHECK (purpose IN ('token', 'payload')),
    kid         INTEGER NOT NULL CHECK (kid BETWEEN 1 AND 255),
    state       TEXT NOT NULL CHECK (state IN ('active', 'retired', 'destroyed')),
    key_check   BLOB NOT NULL CHECK (length(key_check) = 8),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (purpose, kid)
) STRICT;

CREATE UNIQUE INDEX crypto_keys_one_active ON crypto_keys (purpose) WHERE state = 'active';

CREATE TABLE notifications (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id               TEXT NOT NULL REFERENCES tasks (id),
    event                 TEXT NOT NULL CHECK (event IN ('turn_completed', 'waiting_input', 'waiting_approval', 'failed')),
    nid                   BLOB NOT NULL UNIQUE CHECK (length(nid) = 12),
    message_id            TEXT NOT NULL UNIQUE CHECK (length(message_id) BETWEEN 3 AND 998),
    delivered_message_id  TEXT UNIQUE CHECK (delivered_message_id IS NULL OR length(delivered_message_id) BETWEEN 3 AND 998),
    token_kid             INTEGER NOT NULL CHECK (token_kid BETWEEN 1 AND 255),
    token_expires_at      INTEGER NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('PENDING', 'SENDING', 'SENT', 'UNCERTAIN', 'ABANDONED')),
    abandon_reason        TEXT CHECK (abandon_reason IS NULL OR abandon_reason IN ('rejected', 'task_closed', 'expired', 'manual')),
    attempts              INTEGER NOT NULL CHECK (attempts >= 0),
    not_before            INTEGER NOT NULL,
    sent_at               INTEGER,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    CHECK (token_expires_at > created_at),
    CHECK ((state = 'ABANDONED') = (abandon_reason IS NOT NULL)),
    CHECK ((state = 'SENT') = (sent_at IS NOT NULL)),
    CHECK (delivered_message_id IS NULL OR state = 'SENT')
) STRICT;

-- 串行发送：全局至多一条 SENDING。
CREATE UNIQUE INDEX notifications_one_sending ON notifications (state) WHERE state = 'SENDING';
CREATE INDEX notifications_pending ON notifications (not_before, id) WHERE state = 'PENDING';
CREATE INDEX notifications_by_task ON notifications (task_id, id);

-- 令牌的 MAC 输入与我方 Message-ID 一经写入不可修改；实际投递的 Message-ID 只能写入一次。
CREATE TRIGGER notifications_binding_immutable
BEFORE UPDATE OF task_id, event, nid, message_id, token_kid, token_expires_at, created_at ON notifications
BEGIN SELECT RAISE(ABORT, 'notification binding is immutable'); END;

CREATE TRIGGER notifications_delivered_id_once
BEFORE UPDATE OF delivered_message_id ON notifications
WHEN OLD.delivered_message_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'delivered message id is already recorded'); END;

CREATE TABLE notification_payloads (
    notification_id  INTEGER PRIMARY KEY REFERENCES notifications (id),
    key_id           INTEGER NOT NULL CHECK (key_id BETWEEN 1 AND 255),
    sealed           BLOB NOT NULL CHECK (length(sealed) BETWEEN 30 AND 1048605)
) STRICT;

CREATE TABLE reply_payloads (
    seq      INTEGER PRIMARY KEY REFERENCES replies (seq),
    key_id   INTEGER NOT NULL CHECK (key_id BETWEEN 1 AND 255),
    sealed   BLOB NOT NULL CHECK (length(sealed) BETWEEN 30 AND 1048605)
) STRICT;

-- 只有待处理的正文落盘：通知在 PENDING 时写入，回复在 QUEUED 时写入；密文不可改写。
CREATE TRIGGER notification_payloads_pending_only
BEFORE INSERT ON notification_payloads
WHEN (SELECT state FROM notifications WHERE id = NEW.notification_id) IS NOT 'PENDING'
BEGIN SELECT RAISE(ABORT, 'notification payload requires a PENDING notification'); END;

CREATE TRIGGER notification_payloads_immutable
BEFORE UPDATE ON notification_payloads
BEGIN SELECT RAISE(ABORT, 'payloads are immutable'); END;

CREATE TRIGGER reply_payloads_queued_only
BEFORE INSERT ON reply_payloads
WHEN (SELECT state FROM replies WHERE seq = NEW.seq) IS NOT 'QUEUED'
BEGIN SELECT RAISE(ABORT, 'reply payload requires a QUEUED reply'); END;

CREATE TRIGGER reply_payloads_immutable
BEFORE UPDATE ON reply_payloads
BEGIN SELECT RAISE(ABORT, 'payloads are immutable'); END;

-- 进入终态的同一事务中删除正文；UNCERTAIN 保留，因为核对为未送达时要按原序号放回。
CREATE TRIGGER notifications_drop_payload
AFTER UPDATE OF state ON notifications
WHEN NEW.state IN ('SENT', 'ABANDONED')
BEGIN DELETE FROM notification_payloads WHERE notification_id = NEW.id; END;

CREATE TRIGGER replies_drop_payload
AFTER UPDATE OF state ON replies
WHEN NEW.state IN ('ACKNOWLEDGED', 'REJECTED')
BEGIN DELETE FROM reply_payloads WHERE seq = NEW.seq; END;

CREATE TABLE fetch_cursors (
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    last_uid      INTEGER NOT NULL CHECK (last_uid BETWEEN 0 AND 4294967295),
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (account, folder)
) STRICT;

CREATE TABLE inbound_rejections (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT CHECK (message_id IS NULL OR length(message_id) BETWEEN 3 AND 998),
    sender        TEXT CHECK (sender IS NULL OR length(sender) BETWEEN 3 AND 254),
    reason        TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 40 AND reason NOT GLOB '*[^a-z_]*'),
    task_id       TEXT REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, folder, uid_validity, uid)
) STRICT;

CREATE INDEX inbound_rejections_by_time ON inbound_rejections (received_at, id);
