// Package sqlite 的表结构测试绕过 API 直接读写迁移 0002 建立的表，验证 CHECK、唯一约束、部分唯一索引与触发器。
package sqlite

import (
	"bytes"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// columns 是直接插入的一行：列名到值。
type columns map[string]any

// with 返回把列 name 设为 value 的副本，原行不变。
func (c columns) with(name string, value any) columns {
	out := maps.Clone(c)
	out[name] = value
	return out
}

// without 返回去掉列 name 的副本，用来构造缺少必填列的插入。
func (c columns) without(name string) columns {
	out := maps.Clone(c)
	delete(out, name)
	return out
}

// insert 把这一行插入 table，列按名称排序使语句确定，值全部以绑定参数传入；表名与列名只来自测试代码中的常量。
func (c columns) insert(t *testing.T, db *sql.DB, table string) error {
	t.Helper()
	names := slices.Sorted(maps.Keys(c))
	args := make([]any, len(names))
	for i, name := range names {
		args[i] = c[name]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	_, err := db.ExecContext(t.Context(), fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(names, ", "), placeholders), args...)
	return err
}

// notificationRow 返回任务 taskID 的第 n 条 PENDING 通知行；nid 与 Message-ID 由 n 派生，使各行满足唯一约束。
func notificationRow(taskID string, n int) columns {
	return columns{
		"task_id":          taskID,
		"event":            "turn_completed",
		"nid":              bytes.Repeat([]byte{byte(n)}, 12),
		"message_id":       fmt.Sprintf("<tc.%d@example.invalid>", n),
		"token_kid":        1,
		"token_expires_at": 2,
		"state":            "PENDING",
		"attempts":         0,
		"not_before":       0,
		"created_at":       1,
		"updated_at":       1,
	}
}

// insertNotification 直接插入 notificationRow(taskID, n) 并返回其 id，失败时终止测试。
func insertNotification(t *testing.T, db *sql.DB, taskID string, n int) int64 {
	t.Helper()
	if err := notificationRow(taskID, n).insert(t, db, "notifications"); err != nil {
		t.Fatalf("插入通知失败: %v", err)
	}
	var id int64
	if err := db.QueryRowContext(t.Context(), "SELECT id FROM notifications WHERE message_id = ?", fmt.Sprintf("<tc.%d@example.invalid>", n)).Scan(&id); err != nil {
		t.Fatalf("读取通知 id 失败: %v", err)
	}
	return id
}

// requireExec 执行 query；want 为空时要求成功，否则要求错误文本含 want，用来区分 CHECK、唯一约束与触发器的拒绝。
func requireExec(t *testing.T, db *sql.DB, name, want, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), query, args...)
	requireResult(t, name, want, err)
}

// requireResult 断言 err 符合 want：want 为空时 err 须为 nil，否则 err 的文本须含 want。
func requireResult(t *testing.T, name, want string, err error) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: 返回错误 %v; want 成功", name, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: err = %v; want 含 %q 的错误", name, err, want)
	}
}

// TestSchemaConstraints 按顺序直接插入 0002 各表的行，验证 CHECK、NOT NULL、唯一约束与部分唯一索引：
// 每组非法行旁都有只差一处的合法行作对照，证明失败只来自被测的那条约束。
func TestSchemaConstraints(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	queued := recordReply(t, store, inbound(running.ID)).Reply.Seq
	pending := insertNotification(t, store.db, running.ID, 100)
	// 记录回复需要已登记的密钥，并会为 QUEUED 回复写入正文；清空这两张表，使下面的直接插入从空表开始。
	for _, table := range []string{"crypto_keys", "reply_payloads"} {
		requireExec(t, store.db, "清空 "+table, "", "DELETE FROM "+table)
	}

	const (
		checkFailed   = "CHECK constraint failed"
		notNullFailed = "NOT NULL constraint failed: inbound_messages.folder"
	)
	instance := columns{"singleton": 1, "instance_id": "0123456789abcdef", "created_at": 0}
	cryptoKey := columns{"purpose": "token", "kid": 1, "state": "active", "key_check": bytes.Repeat([]byte{0x5a}, 8), "created_at": 0, "updated_at": 0}
	notificationPayload := columns{"notification_id": pending, "key_id": 1, "sealed": bytes.Repeat([]byte{0xa5}, 30)}
	replyPayload := columns{"seq": queued, "key_id": 1, "sealed": bytes.Repeat([]byte{0xa5}, 30)}
	cursor := columns{"account": botAccount, "folder": "INBOX", "uid_validity": 1, "last_uid": 0, "updated_at": 0}
	rejection := columns{"account": botAccount, "folder": "INBOX", "uid_validity": 1, "uid": 1, "reason": "synthetic", "received_at": 0}
	inboundRow := columns{"account": botAccount, "folder": "INBOX", "uid_validity": 7, "uid": 100, "message_id": "<direct-1@example.invalid>",
		"body_sha256": digestA[:], "task_id": running.ID, "received_at": 0}

	tests := []struct {
		name  string
		table string
		row   columns
		want  string // 为空表示插入应当成功，否则为错误文本应含的内容
	}{
		{"instance_id 为 15 个字符", "instance", instance.with("instance_id", "0123456789abcde"), checkFailed},
		{"instance_id 含 u", "instance", instance.with("instance_id", "0123456789abcdeu"), checkFailed},
		{"合法的实例行", "instance", instance, ""},
		{"instance 第二行（singleton 为 2）", "instance", instance.with("singleton", 2), checkFailed},

		{"kid 为 0", "crypto_keys", cryptoKey.with("kid", 0), checkFailed},
		{"kid 为 256", "crypto_keys", cryptoKey.with("kid", 256), checkFailed},
		{"key_check 为 7 字节", "crypto_keys", cryptoKey.with("key_check", bytes.Repeat([]byte{0x5a}, 7)), checkFailed},
		{"合法的 active 密钥", "crypto_keys", cryptoKey, ""},
		{"同一用途的 retired 密钥", "crypto_keys", cryptoKey.with("kid", 2).with("state", "retired"), ""},
		{"同一用途第二条 active", "crypto_keys", cryptoKey.with("kid", 3), "UNIQUE constraint failed: crypto_keys.purpose"},
		{"另一用途的 active 密钥", "crypto_keys", cryptoKey.with("purpose", "payload"), ""},

		{"通知正文 29 字节", "notification_payloads", notificationPayload.with("sealed", bytes.Repeat([]byte{0xa5}, 29)), checkFailed},
		{"通知正文 30 字节", "notification_payloads", notificationPayload, ""},
		{"回复正文 29 字节", "reply_payloads", replyPayload.with("sealed", bytes.Repeat([]byte{0xa5}, 29)), checkFailed},
		{"回复正文 30 字节", "reply_payloads", replyPayload, ""},

		{"abandon_reason 为 bogus", "notifications", notificationRow(running.ID, 1).with("state", "ABANDONED").with("abandon_reason", "bogus"), checkFailed},
		{"abandon_reason 为 expired", "notifications", notificationRow(running.ID, 1).with("state", "ABANDONED").with("abandon_reason", "expired"), ""},
		{"nid 为 11 字节", "notifications", notificationRow(running.ID, 2).with("nid", bytes.Repeat([]byte{2}, 11)), checkFailed},
		{"状态为 BOGUS", "notifications", notificationRow(running.ID, 2).with("state", "BOGUS"), checkFailed},
		{"PENDING 带 abandon_reason", "notifications", notificationRow(running.ID, 2).with("abandon_reason", "expired"), checkFailed},
		{"SENT 但 sent_at 为空", "notifications", notificationRow(running.ID, 2).with("state", "SENT"), checkFailed},
		{"SENT 且有 sent_at", "notifications", notificationRow(running.ID, 2).with("state", "SENT").with("sent_at", 1), ""},
		{"PENDING 带 delivered_message_id", "notifications", notificationRow(running.ID, 3).with("delivered_message_id", "<q.3@example.invalid>"), checkFailed},
		{"token_expires_at 等于 created_at", "notifications", notificationRow(running.ID, 3).with("token_expires_at", 1), checkFailed},
		{"第一条 SENDING", "notifications", notificationRow(running.ID, 3).with("state", "SENDING"), ""},
		{"第二条 SENDING", "notifications", notificationRow(running.ID, 4).with("state", "SENDING"), "UNIQUE constraint failed: notifications.state"},

		{"last_uid 为 -1", "fetch_cursors", cursor.with("last_uid", -1), checkFailed},
		{"last_uid 为 0", "fetch_cursors", cursor, ""},

		{"reason 为 Bad Reason", "inbound_rejections", rejection.with("reason", "Bad Reason"), checkFailed},
		{"reason 为 41 个字符", "inbound_rejections", rejection.with("reason", strings.Repeat("a", 41)), checkFailed},
		{"reason 为空字符串", "inbound_rejections", rejection.with("reason", ""), checkFailed},
		{"reason 为 40 个字符", "inbound_rejections", rejection.with("reason", strings.Repeat("a", 40)), ""},
		{"合法的 reason", "inbound_rejections", rejection.with("uid", 2), ""},

		{"入站记录不带 folder", "inbound_messages", inboundRow.without("folder"), notNullFailed},
		{"folder 为空字符串", "inbound_messages", inboundRow.with("folder", ""), checkFailed},
		{"folder 为 256 个字符", "inbound_messages", inboundRow.with("folder", strings.Repeat("f", 256)), checkFailed},
		{"合法的入站记录", "inbound_messages", inboundRow, ""},
		{"同一 (account, folder, uid_validity, uid) 的第二行", "inbound_messages", inboundRow.with("message_id", "<direct-2@example.invalid>"),
			"UNIQUE constraint failed: inbound_messages.account, inbound_messages.folder, inbound_messages.uid_validity, inbound_messages.uid"},
		{"同一 (account, uid_validity, uid) 但 folder 不同", "inbound_messages", inboundRow.with("folder", "Junk").with("message_id", "<direct-3@example.invalid>"), ""},
	}
	for _, tt := range tests {
		requireResult(t, tt.name, tt.want, tt.row.insert(t, store.db, tt.table))
	}
}

// TestPayloadTriggers 验证正文表的触发器：只能为 QUEUED 回复与 PENDING 通知写入正文，正文不可修改；
// 回复变为 ACKNOWLEDGED 或 REJECTED、通知变为 SENT 或 ABANDONED 时正文在同一语句中被删除，
// 中间状态 DISPATCHING、UNCERTAIN 与 SENDING、UNCERTAIN 保留正文。
func TestPayloadTriggers(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	db := store.db
	sealed := bytes.Repeat([]byte{0xa5}, 30)
	// requirePayload 断言 table 中键为 id 的正文行存在与否。
	requirePayload := func(step, table, key string, id int64, want bool) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = ?", table, key), id).Scan(&n); err != nil {
			t.Fatalf("读取 %s 失败: %v", table, err)
		}
		if (n == 1) != want {
			t.Errorf("%s: %s 中 %d 的正文行数 = %d; want 存在 = %v", step, table, id, n, want)
		}
	}

	replies := enqueueReplies(t, store, running.ID, 2)
	acknowledged, rejected := replies[0].Seq, replies[1].Seq
	// 记录回复时已写入正文；先删除，再由下面的直接插入验证触发器。
	requireExec(t, db, "删除入队时写入的正文", "", "DELETE FROM reply_payloads")
	for _, seq := range []int64{acknowledged, rejected} {
		requireExec(t, db, "为 QUEUED 回复写入正文", "", "INSERT INTO reply_payloads (seq, key_id, sealed) VALUES (?, 1, ?)", seq, sealed)
	}
	requireExec(t, db, "更新回复正文", "payloads are immutable", "UPDATE reply_payloads SET key_id = 2 WHERE seq = ?", acknowledged)
	for _, step := range []struct {
		seq  int64
		set  string
		kept bool
	}{
		{acknowledged, "state = 'DISPATCHING', resume_state = 'COMPLETED'", true},
		{acknowledged, "state = 'UNCERTAIN'", true},
		{acknowledged, "state = 'ACKNOWLEDGED'", false},
		{rejected, "state = 'REJECTED', reject_reason = 'task_closed'", false},
	} {
		requireExec(t, db, "回复改为 "+step.set, "", "UPDATE replies SET "+step.set+" WHERE seq = ?", step.seq)
		requirePayload("回复改为 "+step.set, "reply_payloads", "seq", step.seq, step.kept)
	}
	requireExec(t, db, "为 ACKNOWLEDGED 回复写入正文", "reply payload requires a QUEUED reply",
		"INSERT INTO reply_payloads (seq, key_id, sealed) VALUES (?, 1, ?)", acknowledged, sealed)
	requireExec(t, db, "为不存在的序号写入正文", "reply payload requires a QUEUED reply",
		"INSERT INTO reply_payloads (seq, key_id, sealed) VALUES (999, 1, ?)", sealed)

	sent, abandoned := insertNotification(t, db, running.ID, 1), insertNotification(t, db, running.ID, 2)
	for _, id := range []int64{sent, abandoned} {
		requireExec(t, db, "为 PENDING 通知写入正文", "", "INSERT INTO notification_payloads (notification_id, key_id, sealed) VALUES (?, 1, ?)", id, sealed)
	}
	requireExec(t, db, "更新通知正文", "payloads are immutable", "UPDATE notification_payloads SET key_id = 2 WHERE notification_id = ?", sent)
	for _, step := range []struct {
		id   int64
		set  string
		kept bool
	}{
		{sent, "state = 'SENDING'", true},
		{sent, "state = 'UNCERTAIN'", true},
		{sent, "state = 'SENT', sent_at = 5", false},
		{abandoned, "state = 'ABANDONED', abandon_reason = 'manual'", false},
	} {
		requireExec(t, db, "通知改为 "+step.set, "", "UPDATE notifications SET "+step.set+" WHERE id = ?", step.id)
		requirePayload("通知改为 "+step.set, "notification_payloads", "notification_id", step.id, step.kept)
	}
	requireExec(t, db, "为 SENT 通知写入正文", "notification payload requires a PENDING notification",
		"INSERT INTO notification_payloads (notification_id, key_id, sealed) VALUES (?, 1, ?)", sent, sealed)
	requireExec(t, db, "为不存在的通知写入正文", "notification payload requires a PENDING notification",
		"INSERT INTO notification_payloads (notification_id, key_id, sealed) VALUES (999, 1, ?)", sealed)
}

// TestNotificationBindingTriggers 验证令牌的 MAC 输入与我方 Message-ID 一经写入不可修改（即使写回原值），
// 而实际投递的 Message-ID 只能写入一次。
func TestNotificationBindingTriggers(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	id := insertNotification(t, store.db, running.ID, 1)
	requireExec(t, store.db, "改写 nid", "notification binding is immutable",
		"UPDATE notifications SET nid = ? WHERE id = ?", bytes.Repeat([]byte{9}, 12), id)
	requireExec(t, store.db, "改写 token_expires_at", "notification binding is immutable",
		"UPDATE notifications SET token_expires_at = 99 WHERE id = ?", id)
	for _, column := range []string{"task_id", "event", "nid", "message_id", "token_kid", "token_expires_at", "created_at"} {
		requireExec(t, store.db, "把 "+column+" 写回原值", "notification binding is immutable",
			fmt.Sprintf("UPDATE notifications SET %s = %s WHERE id = ?", column, column), id)
	}
	requireExec(t, store.db, "发送成功", "", "UPDATE notifications SET state = 'SENT', sent_at = 5, attempts = 1, updated_at = 5 WHERE id = ?", id)
	requireExec(t, store.db, "第一次写入实际投递的 Message-ID", "",
		"UPDATE notifications SET delivered_message_id = '<q.1@example.invalid>' WHERE id = ?", id)
	requireExec(t, store.db, "第二次写入实际投递的 Message-ID", "delivered message id is already recorded",
		"UPDATE notifications SET delivered_message_id = '<q.2@example.invalid>' WHERE id = ?", id)
}
