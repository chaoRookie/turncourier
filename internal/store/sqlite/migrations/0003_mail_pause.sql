-- 0003：邮件触发的持久暂停与被拒记录的 Message-ID 索引（Phase 4b）。0001 与 0002 已发布，不修改。

-- D8 的回环刹车：每个任务至多一行。resumed_at 为空表示正在暂停；解除后保留解除时刻，刹车只计此后接受的回复。
CREATE TABLE mail_pauses (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id),
    reason      TEXT NOT NULL CHECK (reason IN ('hourly', 'daily')),
    paused_at   INTEGER NOT NULL,
    resumed_at  INTEGER CHECK (resumed_at IS NULL OR resumed_at >= paused_at)
) STRICT;

-- UIDVALIDITY 重置后按 Message-ID 认出早已拒绝的来信（4b Task 9 第 0 步）。
CREATE INDEX inbound_rejections_by_message ON inbound_rejections (account, folder, message_id) WHERE message_id IS NOT NULL;
