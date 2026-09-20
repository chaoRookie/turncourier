// Package sqlite 的入站回复测试用临时目录中的真实 SQLite 数据库验证去重、冲突、拒绝原因、字段校验、原子性，
// fail 与 close 对排队回复的拒绝、其他事件不改动排队回复，以及正文的加密入队、校验、正文密钥检查与磁盘残留。
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/security/payload"
	"github.com/chaoRookie/turncourier/internal/security/token"
	"github.com/chaoRookie/turncourier/internal/task"
)

// botAccount 是测试使用的合成机器人邮箱地址。
const botAccount = "bot@example.invalid"

var (
	// digestA 是合成正文 A 的键控摘要。
	digestA = bodyDigest([]byte("synthetic reply A"))
	// digestB 是合成正文 B 的键控摘要。
	digestB = bodyDigest([]byte("synthetic reply B"))
)

// bodyDigest 用 kid 1 的固定测试令牌密钥计算 body 的键控摘要（token.Key.BodyDigest），与 4b 入站流水线的算法相同；
// 密钥字节在运行时由 bytes.Repeat 构造，源码中没有密钥字面量。kid 1、32 字节的密钥不会构造失败，失败时 panic。
func bodyDigest(body []byte) [32]byte {
	key, err := token.NewKey(1, bytes.Repeat([]byte{0xc3}, token.KeyLen))
	if err != nil {
		panic(err)
	}
	return key.BodyDigest(body)
}

// withBody 返回正文换为 body、摘要换为其键控摘要的 in。
func withBody(in InboundReply, body string) InboundReply {
	in.Body = []byte(body)
	in.BodyDigest = bodyDigest(in.Body)
	return in
}

// inbound 返回任务 taskID 的一条合成入站回复：INBOX、uv=7、uid=1、<a@example.invalid>、正文 A 及其摘要；测试按需修改字段。
func inbound(taskID string) InboundReply {
	return withBody(InboundReply{
		TaskID:      taskID,
		Account:     botAccount,
		Folder:      "INBOX",
		UIDValidity: 7,
		UID:         1,
		MessageID:   "<a@example.invalid>",
	}, "synthetic reply A")
}

// recordReply 记录入站回复，失败时终止测试。
func recordReply(t *testing.T, s *Store, in InboundReply) RecordResult {
	t.Helper()
	result, err := s.RecordReply(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordReply 返回错误: %v", err)
	}
	return result
}

// mustGetReply 按 seq 读取回复，失败时终止测试。
func mustGetReply(t *testing.T, s *Store, seq int64) Reply {
	t.Helper()
	reply, err := getReply(t.Context(), s.db, seq)
	if err != nil {
		t.Fatalf("读取回复 %d 失败: %v", seq, err)
	}
	return reply
}

// requireRows 断言 inbound_messages 与 replies 的行数，用于验证去重、冲突与失败的操作没有写入。
func requireRows(t *testing.T, s *Store, inbound, replies int) {
	t.Helper()
	if got := countRows(t, s.db, "inbound_messages"); got != inbound {
		t.Errorf("inbound_messages 行数 = %d; want %d", got, inbound)
	}
	if got := countRows(t, s.db, "replies"); got != replies {
		t.Errorf("replies 行数 = %d; want %d", got, replies)
	}
}

// TestRecordReplyDeduplication 以 RUNNING 任务为基础验证清单表格中的去重与冲突判定：
// 新邮件入队为 QUEUED；同一邮件以相同或新的 uid、新的 UIDVALIDITY 再次出现时返回原回复且不写入；
// 同一 Message-ID 摘要不同、同一 uid 的 Message-ID 不同或同一邮件指向另一任务时返回 ErrMessageConflict 且不写入。
// 最后验证去重只在同一账户内生效：另一账户的相同标识作为新邮件入队。
func TestRecordReplyDeduplication(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	running := startTask(t, store)
	other := startTask(t, store)

	first, err := store.RecordReply(t.Context(), inbound(running.ID))
	want := RecordResult{Reply: Reply{Seq: 1, TaskID: running.ID, State: queue.Queued, CreatedAt: clock.UTC(), UpdatedAt: clock.UTC()}}
	if err != nil || first != want {
		t.Fatalf("新邮件: RecordReply = %+v, %v; want %+v, nil", first, err, want)
	}
	requireRows(t, store, 1, 1)
	// 推进时钟：若重复邮件被再次写入，时间会与第一次不同。
	*clock = clock.Add(time.Minute)

	duplicates := []struct {
		name   string
		modify func(*InboundReply)
	}{
		{"同 uv/uid、同 Message-ID、同摘要", func(*InboundReply) {}},
		{"不同 uid、同 Message-ID、同摘要", func(in *InboundReply) { in.UID = 2 }},
		{"UIDVALIDITY 变为 8", func(in *InboundReply) { in.UIDValidity = 8 }},
	}
	for _, tt := range duplicates {
		in := inbound(running.ID)
		tt.modify(&in)
		got, err := store.RecordReply(t.Context(), in)
		if err != nil || got != (RecordResult{Reply: first.Reply, Duplicate: true}) {
			t.Errorf("%s: RecordReply = %+v, %v; want 原回复且 Duplicate=true", tt.name, got, err)
		}
		requireRows(t, store, 1, 1)
	}

	conflicts := []struct {
		name   string
		modify func(*InboundReply)
	}{
		{"同 uv/uid、同 Message-ID、摘要 B", func(in *InboundReply) { in.BodyDigest = digestB }},
		{"新 uid、同 Message-ID、摘要 B", func(in *InboundReply) { in.UID, in.BodyDigest = 3, digestB }},
		{"同 uv/uid、不同 Message-ID", func(in *InboundReply) { in.MessageID = "<b@example.invalid>" }},
		{"同一邮件指向另一任务", func(in *InboundReply) { in.TaskID = other.ID }},
	}
	for _, tt := range conflicts {
		in := inbound(running.ID)
		tt.modify(&in)
		if got, err := store.RecordReply(t.Context(), in); !errors.Is(err, ErrMessageConflict) {
			t.Errorf("%s: RecordReply = %+v, %v; want ErrMessageConflict", tt.name, got, err)
		}
		requireRows(t, store, 1, 1)
	}

	in := inbound(running.ID)
	in.Account = "other-bot@example.invalid"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.Seq != 2 || got.Reply.State != queue.Queued {
		t.Errorf("另一账户: RecordReply = %+v; want 新入队 seq 2", got)
	}
	requireRows(t, store, 2, 2)
}

// TestRecordReplyTaskNotFound 验证任务不存在时返回 ErrNotFound 且不写入任何记录。
func TestRecordReplyTaskNotFound(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	if got, err := store.RecordReply(t.Context(), inbound("0000000000")); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordReply = %+v, %v; want ErrNotFound", got, err)
	}
	requireRows(t, store, 0, 0)
}

// TestRecordReplyByTaskState 验证入队结果取决于任务状态：接受回复的状态入队为 QUEUED；
// FAILED、CLOSED、CREATED 记录为 REJECTED，原因分别为 task_failed、task_closed、task_not_accepting。
// 同一邮件再次记录时返回 Duplicate=true 与原回复，不按任务的新状态重新判定。
func TestRecordReplyByTaskState(t *testing.T) {
	tests := []struct {
		state  task.State
		want   queue.State
		reason string
	}{
		{task.Running, queue.Queued, ""},
		{task.WaitingInput, queue.Queued, ""},
		{task.WaitingApproval, queue.Queued, ""},
		{task.Completed, queue.Queued, ""},
		{task.DeliveryUncertain, queue.Queued, ""},
		{task.Failed, queue.Rejected, "task_failed"},
		{task.Closed, queue.Rejected, "task_closed"},
		{task.Created, queue.Rejected, "task_not_accepting"},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			store, clock := openTaskStore(t, nil)
			current := createTask(t, store)
			// 直接设置任务状态：DELIVERY_UNCERTAIN 需要回复队列配合才能经 API 进入，其余状态与之统一处理。
			if _, err := store.db.ExecContext(t.Context(), "UPDATE tasks SET state = ? WHERE id = ?", string(tt.state), current.ID); err != nil {
				t.Fatalf("设置任务状态失败: %v", err)
			}
			got := recordReply(t, store, inbound(current.ID))
			want := RecordResult{Reply: Reply{Seq: 1, TaskID: current.ID, State: tt.want, RejectReason: tt.reason, CreatedAt: clock.UTC(), UpdatedAt: clock.UTC()}}
			if got != want {
				t.Fatalf("RecordReply = %+v; want %+v", got, want)
			}
			if stored := mustGetReply(t, store, 1); stored != want.Reply {
				t.Errorf("存储的回复 = %+v; want %+v", stored, want.Reply)
			}

			if _, err := store.db.ExecContext(t.Context(), "UPDATE tasks SET state = 'RUNNING' WHERE id = ?", current.ID); err != nil {
				t.Fatalf("设置任务状态失败: %v", err)
			}
			if again := recordReply(t, store, inbound(current.ID)); again != (RecordResult{Reply: want.Reply, Duplicate: true}) {
				t.Errorf("再次记录 = %+v; want 原回复且 Duplicate=true", again)
			}
			requireRows(t, store, 1, 1)
		})
	}
}

// TestRecordReplyValidation 验证字段校验在事务开始前拒绝非法输入：错误文本含 "invalid inbound reply"，
// 以区别于数据库 CHECK 约束与任务查询的报错，且不写入任何记录。长度按字符而不是字节计，边界上的合法值可以写入。
// 上下文已取消时仍返回校验错误，证明校验先于开始事务。
func TestRecordReplyValidation(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	tests := []struct {
		name   string
		modify func(*InboundReply)
	}{
		{"Account 为空", func(in *InboundReply) { in.Account = "" }},
		{"Account 少于 3 字符", func(in *InboundReply) { in.Account = "a@" }},
		{"Account 超过 254 字符", func(in *InboundReply) { in.Account = strings.Repeat("a", 243) + "@example.com" }},
		{"Account 含空格", func(in *InboundReply) { in.Account = "bot @example.invalid" }},
		{"Account 含制表符", func(in *InboundReply) { in.Account = "bot\t@example.invalid" }},
		{"Account 末尾换行", func(in *InboundReply) { in.Account = botAccount + "\n" }},
		{"Account 含回车", func(in *InboundReply) { in.Account = "bot\r@example.invalid" }},
		{"Account 含全角空格", func(in *InboundReply) { in.Account = "bot\u3000@example.invalid" }},
		{"Account 少于 3 个非 ASCII 字符", func(in *InboundReply) { in.Account = "中@" }},
		{"Account 含 NUL", func(in *InboundReply) { in.Account = "bot\x00@example.invalid" }},
		{"Account 含非法 UTF-8", func(in *InboundReply) { in.Account = "bot\xff@example.invalid" }},
		{"Folder 为空", func(in *InboundReply) { in.Folder = "" }},
		{"Folder 超过 255 字符", func(in *InboundReply) { in.Folder = strings.Repeat("f", 256) }},
		{"Folder 含 NUL", func(in *InboundReply) { in.Folder = "IN\x00BOX" }},
		{"Folder 含非法 UTF-8", func(in *InboundReply) { in.Folder = "IN\xffBOX" }},
		{"Folder 含非法 UTF-8 续字节", func(in *InboundReply) { in.Folder = "IN\x80BOX" }},
		{"UIDValidity 为 0", func(in *InboundReply) { in.UIDValidity = 0 }},
		{"UID 为 0", func(in *InboundReply) { in.UID = 0 }},
		{"MessageID 为空", func(in *InboundReply) { in.MessageID = "" }},
		{"MessageID 少于 3 字符", func(in *InboundReply) { in.MessageID = "<a" }},
		{"MessageID 少于 3 个非 ASCII 字符", func(in *InboundReply) { in.MessageID = "<é" }},
		{"MessageID 含 NUL", func(in *InboundReply) { in.MessageID = "<\x00>" }},
		{"MessageID 含非法 UTF-8", func(in *InboundReply) { in.MessageID = "<\xff>" }},
		{"MessageID 超过 998 字符", func(in *InboundReply) { in.MessageID = "<" + strings.Repeat("a", 997) + ">" }},
		{"任务不存在时仍先校验字段", func(in *InboundReply) { in.TaskID, in.UID = "missing", 0 }},
	}
	for _, tt := range tests {
		in := inbound(running.ID)
		tt.modify(&in)
		got, err := store.RecordReply(t.Context(), in)
		if err == nil || !strings.Contains(err.Error(), "invalid inbound reply") {
			t.Errorf("%s: RecordReply = %+v, %v; want 含 \"invalid inbound reply\" 的错误", tt.name, got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := inbound(running.ID)
	in.UID = 0
	if got, err := store.RecordReply(ctx, in); err == nil || !strings.Contains(err.Error(), "invalid inbound reply") || errors.Is(err, context.Canceled) {
		t.Errorf("上下文已取消: RecordReply = %+v, %v; want 含 \"invalid inbound reply\" 的错误而不是 context.Canceled", got, err)
	}
	requireRows(t, store, 0, 0)

	in = inbound(running.ID)
	in.Account = strings.Repeat("a", 242) + "@example.com"
	in.MessageID = "<a>"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("边界值: RecordReply = %+v; want 新入队", got)
	}
	in = inbound(running.ID)
	in.UIDValidity, in.UID = 4294967295, 4294967295
	in.MessageID = "<" + strings.Repeat("b", 996) + ">"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("最大 UID 与 998 字符 Message-ID: RecordReply = %+v; want 新入队", got)
	}
	in = inbound(running.ID)
	in.Account = "a@b"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("3 字符 Account: RecordReply = %+v; want 新入队", got)
	}
	// 非 ASCII 字符占多个字节：254 与 998 个字符超过同样数目的字节，仍在表约束允许的范围内。
	in = inbound(running.ID)
	in.Account = strings.Repeat("é", 242) + "@example.com"
	in.MessageID = "<" + strings.Repeat("é", 996) + ">"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("254 与 998 个非 ASCII 字符: RecordReply = %+v; want 新入队", got)
	}
	// IMAP 文件夹名可以含空格；255 个非 ASCII 字符超过 255 字节，仍在表约束允许的范围内。
	in = inbound(running.ID)
	in.Folder = "Sent Messages"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("含空格的 Folder: RecordReply = %+v; want 新入队", got)
	}
	in = inbound(running.ID)
	in.Folder, in.UID, in.MessageID = strings.Repeat("文", 255), 2, "<c@example.invalid>"
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("255 个字符的 Folder: RecordReply = %+v; want 新入队", got)
	}
	requireRows(t, store, 6, 6)
}

// TestRecordReplyMatchesMailboxRule 验证账户与文件夹在 RecordReply 与游标接口中按同一规则判定：
// 同一取值要么两处都接受，要么两处都拒绝。不一致时同一个文件夹名可以记录回复却无法推进游标，
// 或者非法 UTF-8 的取值只在其中一处被拦下，另一处要到执行 SQL 之后才被 CHECK 约束拒绝。
func TestRecordReplyMatchesMailboxRule(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	tests := []struct {
		name    string
		account string
		folder  string
	}{
		{"合法取值", botAccount, "Sent Messages"},
		{"账户含非法 UTF-8", "bot\xff@example.invalid", "INBOX"},
		{"文件夹含非法 UTF-8", botAccount, "IN\xffBOX"},
		{"文件夹含非法 UTF-8 续字节", botAccount, "IN\x80BOX"},
		{"文件夹为空", botAccount, ""},
		{"文件夹超过 255 字符", botAccount, strings.Repeat("f", 256)},
		{"账户含空格", "bot @example.invalid", "INBOX"},
	}
	for _, tt := range tests {
		cursorErr := store.AdvanceCursor(t.Context(), tt.account, tt.folder, Cursor{UIDValidity: 7, LastUID: 1})
		in := inbound(running.ID)
		in.Account, in.Folder = tt.account, tt.folder
		_, replyErr := store.RecordReply(t.Context(), in)
		if (cursorErr == nil) != (replyErr == nil) {
			t.Errorf("%s: AdvanceCursor = %v，RecordReply = %v; want 两处判定一致", tt.name, cursorErr, replyErr)
		}
	}
}

// TestRecordReplySameUIDInDifferentFolders 验证 UID 只在同一文件夹内唯一：INBOX 与 Junk 中 UIDVALIDITY 与 UID 都相同、
// Message-ID 与摘要不同的两封邮件都作为新回复入队，得到不同的序号。按 UID 查找时不含文件夹的实现会把第二封判为冲突。
func TestRecordReplySameUIDInDifferentFolders(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	first := recordReply(t, store, inbound(running.ID))
	junk := inbound(running.ID)
	junk = withBody(junk, "synthetic reply B")
	junk.Folder, junk.MessageID = "Junk", "<b@example.invalid>"
	second, err := store.RecordReply(t.Context(), junk)
	if err != nil || second.Duplicate || second.Reply.State != queue.Queued || second.Reply.Seq == first.Reply.Seq {
		t.Errorf("Junk 中同一 UID 的另一封邮件: RecordReply = %+v, %v; want 新入队且序号不同于 %d", second, err, first.Reply.Seq)
	}
	if first.Duplicate || first.Reply.State != queue.Queued {
		t.Errorf("INBOX 中的邮件: RecordReply = %+v; want 新入队", first)
	}
	requireRows(t, store, 2, 2)
}

// TestRecordReplySameMessageInTwoFolders 验证同一封信在 INBOX 与 Junk 各有一份时按 Message-ID 判为重复：
// 先以 (INBOX, uv 7, uid 1)、再以 (Junk, uv 7, uid 5) 记录同一 Message-ID、摘要与任务，第二次返回第一次的回复且
// Duplicate 为 true，不新增任何行；从 Junk 取回的同一 Message-ID 摘要不同时返回 ErrMessageConflict。
// 按 Message-ID 查找时加上文件夹的实现会漏掉已有记录，第二封因 UNIQUE (account, message_id) 插入失败。
func TestRecordReplySameMessageInTwoFolders(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	first := recordReply(t, store, inbound(running.ID))
	junk := inbound(running.ID)
	junk.Folder, junk.UID = "Junk", 5
	if got, err := store.RecordReply(t.Context(), junk); err != nil || got != (RecordResult{Reply: first.Reply, Duplicate: true}) {
		t.Errorf("Junk 中的副本: RecordReply = %+v, %v; want 原回复且 Duplicate=true", got, err)
	}
	requireRows(t, store, 1, 1)
	junk.BodyDigest = digestB
	if got, err := store.RecordReply(t.Context(), junk); !errors.Is(err, ErrMessageConflict) {
		t.Errorf("Junk 中同一 Message-ID 摘要不同: RecordReply = %+v, %v; want ErrMessageConflict", got, err)
	}
	requireRows(t, store, 1, 1)
}

// TestRecordReplyAtomic 用测试内创建的触发器让回复队列项写入失败，验证入站记录随事务一起回滚；
// 删除触发器后同一邮件可以正常入队，证明失败没有留下使其被误判为重复的记录。
func TestRecordReplyAtomic(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	if _, err := store.db.ExecContext(t.Context(),
		"CREATE TRIGGER boom BEFORE INSERT ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END;"); err != nil {
		t.Fatalf("创建触发器失败: %v", err)
	}
	if got, err := store.RecordReply(t.Context(), inbound(running.ID)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("RecordReply = %+v, %v; want 触发器错误", got, err)
	}
	requireRows(t, store, 0, 0)

	if _, err := store.db.ExecContext(t.Context(), "DROP TRIGGER boom"); err != nil {
		t.Fatalf("删除触发器失败: %v", err)
	}
	if got := recordReply(t, store, inbound(running.ID)); got.Duplicate || got.Reply.State != queue.Queued {
		t.Errorf("删除触发器后: RecordReply = %+v; want 新入队", got)
	}
	requireRows(t, store, 1, 1)
}

// TestRecordReplyRejectsUnknownReplyState 验证读取到不在已知集合的回复状态或派发前任务状态时返回错误，而不是静默接受。
// 表上的 CHECK 约束本会拒绝未知状态，测试临时关闭它，模拟被外部工具改坏的数据库。
func TestRecordReplyRejectsUnknownReplyState(t *testing.T) {
	for _, tt := range []struct {
		update string
		want   string
	}{
		{"UPDATE replies SET state = 'queued'", "queued"},
		{"UPDATE replies SET resume_state = 'completed'", "completed"},
	} {
		store, _ := openTaskStore(t, nil)
		running := startTask(t, store)
		recordReply(t, store, inbound(running.ID))
		for _, query := range []string{"PRAGMA ignore_check_constraints = ON", tt.update, "PRAGMA ignore_check_constraints = OFF"} {
			if _, err := store.db.ExecContext(t.Context(), query); err != nil {
				t.Fatalf("执行 %q 失败: %v", query, err)
			}
		}
		if got, err := store.RecordReply(t.Context(), inbound(running.ID)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: RecordReply = %+v, %v; want 含 %q 的未知状态错误", tt.update, got, err, tt.want)
		}
	}
}

// TestFailAndCloseRejectQueuedReplies 补测 Task 7 遗留项：fail 与 close 在同一事务中把该任务全部 QUEUED 回复改为 REJECTED，
// 原因分别为 task_failed、task_closed，更新时间来自事件发生时的时钟；已处于 DISPATCHING 的回复与其他任务的回复保持不变。
// 派发 API 在 Task 9 实现，DISPATCHING 行用 SQL 直接构造。
func TestFailAndCloseRejectQueuedReplies(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	failing := startTask(t, store)
	closing := startTask(t, store)
	uid := uint32(0)
	// enqueue 为任务记录 n 条新回复，返回各自的快照。
	enqueue := func(taskID string, n int) []Reply {
		var replies []Reply
		for range n {
			uid++
			in := withBody(inbound(taskID), fmt.Sprintf("synthetic reply %d", uid))
			in.UID = uid
			in.MessageID = fmt.Sprintf("<%d@example.invalid>", uid)
			replies = append(replies, recordReply(t, store, in).Reply)
		}
		return replies
	}
	failingReplies := enqueue(failing.ID, 4)
	closingReplies := enqueue(closing.ID, 4)
	for _, reply := range []Reply{failingReplies[0], closingReplies[0]} {
		if _, err := store.db.ExecContext(t.Context(),
			"UPDATE replies SET state = 'DISPATCHING', resume_state = 'COMPLETED' WHERE seq = ?", reply.Seq); err != nil {
			t.Fatalf("构造 DISPATCHING 回复失败: %v", err)
		}
	}
	dispatching := []Reply{mustGetReply(t, store, failingReplies[0].Seq), mustGetReply(t, store, closingReplies[0].Seq)}
	// requireReplies 断言回复与期望快照一致。
	requireReplies := func(want ...Reply) {
		t.Helper()
		for _, reply := range want {
			if got := mustGetReply(t, store, reply.Seq); got != reply {
				t.Errorf("回复 %d = %+v; want %+v", reply.Seq, got, reply)
			}
		}
	}
	// rejected 返回回复被拒绝后的期望快照。
	rejected := func(reply Reply, reason string) Reply {
		reply.State = queue.Rejected
		reply.RejectReason = reason
		reply.UpdatedAt = clock.UTC()
		return reply
	}

	*clock = clock.Add(time.Minute)
	applyEvents(t, store, failing, task.Fail)
	requireReplies(dispatching[0], dispatching[1])
	for _, reply := range failingReplies[1:] {
		requireReplies(rejected(reply, "task_failed"))
	}
	requireReplies(closingReplies[1:]...)

	*clock = clock.Add(time.Minute)
	applyEvents(t, store, closing, task.Close)
	requireReplies(dispatching[0], dispatching[1])
	for _, reply := range closingReplies[1:] {
		requireReplies(rejected(reply, "task_closed"))
	}
}

// TestNonTerminalEventsKeepQueuedReplies 验证 fail、close 之外、会在有排队回复时发生的任务事件不拒绝也不改写排队回复：
// 任务依次经历 approval_requested、approval_resolved、input_requested，再由派发、标记不确定、核对为未送达、
// 派发时即确认未发出、核对为已送达产生 reply_dispatched、delivery_unknown、reply_unsent、delivery_confirmed，最后 turn_completed；
// 每一步都在推进过的时钟下执行，之后排在队首之后的两条回复仍为 QUEUED，拒绝原因与更新时间都保持入队时的值。
// start 不在其中：CREATED 任务收到的回复直接记为 REJECTED，启动前不会有排队回复。
func TestNonTerminalEventsKeepQueuedReplies(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	current := startTask(t, store)
	replies := enqueueReplies(t, store, current.ID, 3)
	head, waiting := replies[0].Seq, replies[1:]
	// requireWaiting 断言排在队首之后的回复与入队时的快照完全一致，再推进时钟，使下一步若改写回复会留下不同的更新时间。
	requireWaiting := func(after string) {
		t.Helper()
		for _, reply := range waiting {
			if got := mustGetReply(t, store, reply.Seq); got != reply {
				t.Errorf("%s 之后回复 %d = %+v; want 保持 %+v", after, reply.Seq, got, reply)
			}
		}
		*clock = clock.Add(time.Minute)
	}

	requireWaiting("入队")
	for _, event := range []task.Event{task.ApprovalRequested, task.ApprovalResolved, task.InputRequested} {
		current = applyEvents(t, store, current, event)
		requireWaiting(string(event))
	}
	claim(t, store, current.ID)
	requireWaiting("reply_dispatched")
	mustMarkUncertain(t, store, head)
	requireWaiting("delivery_unknown")
	if _, _, err := store.ResolveUncertainReply(t.Context(), head, false); err != nil {
		t.Fatalf("ResolveUncertainReply(false) 返回错误: %v", err)
	}
	requireWaiting("reply_unsent（核对为未送达）")
	claim(t, store, current.ID)
	requireWaiting("再次 reply_dispatched")
	if _, _, err := store.RequeueUnsentReply(t.Context(), head); err != nil {
		t.Fatalf("RequeueUnsentReply 返回错误: %v", err)
	}
	requireWaiting("reply_unsent（派发时即确认未发出）")
	claim(t, store, current.ID)
	mustMarkUncertain(t, store, head)
	requireWaiting("第三次派发并标记不确定")
	_, current, err := store.ResolveUncertainReply(t.Context(), head, true)
	if err != nil {
		t.Fatalf("ResolveUncertainReply(true) 返回错误: %v", err)
	}
	requireWaiting("delivery_confirmed")
	applyEvents(t, store, current, task.TurnCompleted)
	requireWaiting("turn_completed")
	requireEvents(t, store, current.ID,
		"start CREATED->RUNNING",
		"approval_requested RUNNING->WAITING_APPROVAL",
		"approval_resolved WAITING_APPROVAL->RUNNING",
		"input_requested RUNNING->WAITING_INPUT",
		"reply_dispatched WAITING_INPUT->RUNNING",
		"delivery_unknown RUNNING->DELIVERY_UNCERTAIN",
		"reply_unsent DELIVERY_UNCERTAIN->WAITING_INPUT",
		"reply_dispatched WAITING_INPUT->RUNNING",
		"reply_unsent RUNNING->WAITING_INPUT",
		"reply_dispatched WAITING_INPUT->RUNNING",
		"delivery_unknown RUNNING->DELIVERY_UNCERTAIN",
		"delivery_confirmed DELIVERY_UNCERTAIN->RUNNING",
		"turn_completed RUNNING->COMPLETED",
	)
}

// replyCanary 是合成回复正文中的金丝雀文本，用来断言正文不以明文落盘、不出现在错误文本中。
const replyCanary = "REPLY-CANARY"

// replyPayloadOf 返回回复 seq 的正文行中的 key_id 与密文；没有正文行时终止测试。
func replyPayloadOf(t *testing.T, s *Store, seq int64) (int, []byte) {
	t.Helper()
	var keyID int
	var sealed []byte
	if err := s.db.QueryRowContext(t.Context(), "SELECT key_id, sealed FROM reply_payloads WHERE seq = ?", seq).Scan(&keyID, &sealed); err != nil {
		t.Fatalf("读取回复 %d 的正文行失败: %v", seq, err)
	}
	return keyID, sealed
}

// replyPayloadCount 返回回复 seq 的正文行数：QUEUED、DISPATCHING 与 UNCERTAIN 时为 1，进入 ACKNOWLEDGED 或 REJECTED 后为 0。
func replyPayloadCount(t *testing.T, s *Store, seq int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM reply_payloads WHERE seq = ?", seq).Scan(&n); err != nil {
		t.Fatalf("统计回复 %d 的正文行失败: %v", seq, err)
	}
	return n
}

// TestRecordReplyEncryptsBody 验证新邮件入队时正文只以密文落盘：reply_payloads 有一行，key_id 为正文密钥的 kid，
// 密文不含金丝雀，用 (KindReply, 任务, 序号) 能还原正文；inbound_messages.body_sha256 等于传入的键控摘要，而不是正文的 SHA-256。
func TestRecordReplyEncryptsBody(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	in := withBody(inbound(running.ID), "synthetic reply "+replyCanary)
	got := recordReply(t, store, in)
	if got.Duplicate || got.Reply.State != queue.Queued {
		t.Fatalf("RecordReply = %+v; want 新入队 QUEUED", got)
	}
	if n := countRows(t, store.db, "reply_payloads"); n != 1 {
		t.Errorf("reply_payloads 行数 = %d; want 1", n)
	}
	keyID, sealed := replyPayloadOf(t, store, got.Reply.Seq)
	if keyID != 1 || len(sealed) != len(in.Body)+payload.Overhead {
		t.Errorf("正文行 key_id = %d、密文 %d 字节; want 1、%d 字节", keyID, len(sealed), len(in.Body)+payload.Overhead)
	}
	if bytes.Contains(sealed, []byte(replyCanary)) {
		t.Error("密文含正文金丝雀")
	}
	if opened, err := newTestPayloadKey(t, 1).Open(payload.KindReply, running.ID, got.Reply.Seq, sealed); err != nil || !bytes.Equal(opened, in.Body) {
		t.Errorf("payload.Open(KindReply, %s, %d) = %q, %v; want 原正文", running.ID, got.Reply.Seq, opened, err)
	}
	var digest []byte
	if err := store.db.QueryRowContext(t.Context(),
		"SELECT i.body_sha256 FROM inbound_messages i JOIN replies r ON r.inbound_id = i.id WHERE r.seq = ?", got.Reply.Seq).Scan(&digest); err != nil {
		t.Fatalf("读取 body_sha256 失败: %v", err)
	}
	if !bytes.Equal(digest, in.BodyDigest[:]) {
		t.Errorf("body_sha256 = %x; want 传入的键控摘要 %x", digest, in.BodyDigest)
	}
	if plain := sha256.Sum256(in.Body); bytes.Equal(digest, plain[:]) {
		t.Error("body_sha256 等于正文的 SHA-256; want 键控摘要")
	}
}

// TestRecordReplyUsesCurrentPayloadKey 验证正文行记录加密所用密钥的 kid：payload 的 active 密钥换为 kid 2 后，
// 新回复的正文行 key_id 为 2，并能用 kid 2 的密钥派发出原正文。
func TestRecordReplyUsesCurrentPayloadKey(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	for _, query := range []string{
		"UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'",
		"INSERT INTO crypto_keys (purpose, kid, state, key_check, created_at, updated_at) SELECT 'payload', 2, 'active', key_check, 0, 0 FROM crypto_keys WHERE purpose = 'payload'",
	} {
		if _, err := store.db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("%s 失败: %v", query, err)
		}
	}
	store.payloadKey = newTestPayloadKey(t, 2)
	completed := applyEvents(t, store, startTask(t, store), task.TurnCompleted)
	in := withBody(inbound(completed.ID), "synthetic reply for kid 2")
	queued := recordReply(t, store, in).Reply
	if keyID, _ := replyPayloadOf(t, store, queued.Seq); keyID != 2 {
		t.Errorf("正文行 key_id = %d; want 2", keyID)
	}
	if reply, _, body, err := store.ClaimNextReply(t.Context(), completed.ID); err != nil || reply.Seq != queued.Seq || !bytes.Equal(body, in.Body) {
		t.Errorf("ClaimNextReply = %+v, %q, %v; want 回复 %d 与原正文", reply, body, err, queued.Seq)
	}
}

// TestRecordReplyWritesNoPayloadForRejectedOrDuplicate 验证只有入队为 QUEUED 的新邮件写正文：任务 CLOSED 时记为
// REJECTED(task_closed) 且没有正文行；同一邮件再次记录返回 Duplicate，不增加也不改写正文行。
func TestRecordReplyWritesNoPayloadForRejectedOrDuplicate(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	closed := applyEvents(t, store, startTask(t, store), task.Close)
	rejected := recordReply(t, store, inbound(closed.ID))
	if rejected.Reply.State != queue.Rejected || rejected.Reply.RejectReason != "task_closed" {
		t.Fatalf("CLOSED 任务: RecordReply = %+v; want REJECTED(task_closed)", rejected)
	}
	if n := countRows(t, store.db, "reply_payloads"); n != 0 {
		t.Errorf("REJECTED 之后 reply_payloads 行数 = %d; want 0", n)
	}

	running := startTask(t, store)
	in := withBody(inbound(running.ID), "synthetic reply to be repeated")
	in.UID, in.MessageID = 2, "<b@example.invalid>"
	first := recordReply(t, store, in)
	payloads := dumpRows(t, store.db, "SELECT seq, key_id, hex(sealed) FROM reply_payloads ORDER BY seq")
	if again := recordReply(t, store, in); !again.Duplicate || again.Reply != first.Reply {
		t.Fatalf("再次记录 = %+v; want 原回复且 Duplicate=true", again)
	}
	if after := dumpRows(t, store.db, "SELECT seq, key_id, hex(sealed) FROM reply_payloads ORDER BY seq"); len(after) != 1 || !slices.Equal(after, payloads) {
		t.Errorf("重复记录后正文行 = %q; want 不变的 1 行 %q", after, payloads)
	}
}

// TestRecordReplyBodyValidation 验证正文在开始事务前校验：为空、超过 payload.MaxPlaintext 或不是合法 UTF-8 时返回含
// "invalid inbound reply" 的错误（上下文已取消时同样如此），错误文本不含正文，不写入任何行；1 字节、多字节 UTF-8 与恰为上限的正文可以入队，
// 上限正文的密文恰为表约束允许的最大长度。
func TestRecordReplyBodyValidation(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tt := range []struct {
		name string
		body []byte
	}{
		{"Body 为 nil", nil},
		{"Body 为空", []byte{}},
		{"Body 超过上限", bytes.Repeat([]byte("a"), payload.MaxPlaintext+1)},
		{"Body 不是合法 UTF-8", []byte(replyCanary + "\xff")},
		{"Body 含截断的多字节字符", []byte(replyCanary + "\xe4\xb8")},
	} {
		in := inbound(running.ID)
		in.Body = tt.body
		got, err := store.RecordReply(ctx, in)
		if err == nil || !strings.Contains(err.Error(), "invalid inbound reply") || errors.Is(err, context.Canceled) {
			t.Errorf("%s: RecordReply = %+v, %v; want 含 \"invalid inbound reply\" 的错误而不是 context.Canceled", tt.name, got, err)
		}
		if err != nil && strings.Contains(err.Error(), replyCanary) {
			t.Errorf("%s: 错误文本含正文: %v", tt.name, err)
		}
	}
	requireRows(t, store, 0, 0)

	for i, body := range [][]byte{[]byte("x"), []byte("中文回复"), bytes.Repeat([]byte("a"), payload.MaxPlaintext)} {
		in := withBody(inbound(running.ID), string(body))
		in.UID, in.MessageID = uint32(i+1), fmt.Sprintf("<boundary-%d@example.invalid>", i)
		got := recordReply(t, store, in)
		if got.Duplicate || got.Reply.State != queue.Queued {
			t.Errorf("%d 字节正文: RecordReply = %+v; want 新入队", len(body), got)
		}
		if _, sealed := replyPayloadOf(t, store, got.Reply.Seq); len(sealed) != len(body)+payload.Overhead {
			t.Errorf("%d 字节正文的密文 = %d 字节; want %d", len(body), len(sealed), len(body)+payload.Overhead)
		}
	}
}

// TestRecordReplyRequiresPayloadKey 验证没有可用的正文密钥时不记录回复：PayloadKey 为 nil 时在开始事务前返回
// ErrPayloadKeyUnavailable（上下文已取消时同样如此）；正文密钥的 kid 未登记或已不是 active 时在事务内返回同一错误。
// 三种情况对入队为 QUEUED 的新邮件、发往 CLOSED 任务而将被记为 REJECTED 的新邮件与已记录过的同一邮件都成立，且不写入任何行。
func TestRecordReplyRequiresPayloadKey(t *testing.T) {
	tests := []struct {
		name     string
		canceled bool
		setup    func(t *testing.T, s *Store)
	}{
		{"没有正文密钥", true, func(t *testing.T, s *Store) { s.payloadKey = nil }},
		{"kid 未登记", false, func(t *testing.T, s *Store) { s.payloadKey = newTestPayloadKey(t, 2) }},
		{"kid 已不是 active", false, func(t *testing.T, s *Store) {
			if _, err := s.db.ExecContext(t.Context(), "UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'"); err != nil {
				t.Fatalf("修改密钥状态失败: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := openTaskStore(t, nil)
			running := startTask(t, store)
			recorded := inbound(running.ID)
			recordReply(t, store, recorded)
			fresh := withBody(inbound(running.ID), "synthetic reply without key")
			fresh.UID, fresh.MessageID = 2, "<b@example.invalid>"
			closed := applyEvents(t, store, startTask(t, store), task.Close)
			rejected := withBody(inbound(closed.ID), "synthetic reply to a closed task")
			rejected.UID, rejected.MessageID = 3, "<c@example.invalid>"
			tt.setup(t, store)
			ctx := t.Context()
			if tt.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			for name, in := range map[string]InboundReply{"新邮件": fresh, "将被拒绝的邮件": rejected, "已记录的邮件": recorded} {
				if got, err := store.RecordReply(ctx, in); !errors.Is(err, ErrPayloadKeyUnavailable) {
					t.Errorf("%s: RecordReply = %+v, %v; want ErrPayloadKeyUnavailable", name, got, err)
				}
			}
			requireRows(t, store, 1, 1)
			if n := countRows(t, store.db, "reply_payloads"); n != 1 {
				t.Errorf("reply_payloads 行数 = %d; want 1", n)
			}
		})
	}
}

// TestRecordReplyPayloadAtomic 用测试内创建的触发器让正文行写入失败，验证入站记录与回复随事务一起回滚；
// 删除触发器后同一邮件可以正常入队并写入正文。
func TestRecordReplyPayloadAtomic(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	if _, err := store.db.ExecContext(t.Context(),
		"CREATE TRIGGER boom BEFORE INSERT ON reply_payloads BEGIN SELECT RAISE(ABORT, 'boom'); END;"); err != nil {
		t.Fatalf("创建触发器失败: %v", err)
	}
	if got, err := store.RecordReply(t.Context(), inbound(running.ID)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("RecordReply = %+v, %v; want 触发器错误", got, err)
	}
	requireRows(t, store, 0, 0)

	if _, err := store.db.ExecContext(t.Context(), "DROP TRIGGER boom"); err != nil {
		t.Fatalf("删除触发器失败: %v", err)
	}
	if got := recordReply(t, store, inbound(running.ID)); got.Duplicate || got.Reply.State != queue.Queued || replyPayloadCount(t, store, got.Reply.Seq) != 1 {
		t.Errorf("删除触发器后: RecordReply = %+v; want 新入队并写入正文", got)
	}
	requireRows(t, store, 1, 1)
}

// TestReplyPayloadResidue 以带金丝雀的正文完成「入队 → 派发 → 确认」，之后数据库文件与 WAL 文件中都找不到密文的任何
// 32 字节片段，也找不到明文金丝雀；正文取 64 字节与 64 KiB 两种大小（与 Task 7 的通知残留测试相同）。
func TestReplyPayloadResidue(t *testing.T) {
	for _, size := range residueSizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTaskStore(t, nil)
			completed := applyEvents(t, store, startTask(t, store), task.TurnCompleted)
			body := bytes.Repeat([]byte(replyCanary+" "), size/len(replyCanary)+1)[:size]
			queued := recordReply(t, store, withBody(inbound(completed.ID), string(body))).Reply
			_, sealed := replyPayloadOf(t, store, queued.Seq)
			reply, _, got, err := store.ClaimNextReply(t.Context(), completed.ID)
			if err != nil || reply.Seq != queued.Seq || !bytes.Equal(got, body) {
				t.Fatalf("ClaimNextReply = %+v, %d 字节, %v; want 回复 %d 与原正文", reply, len(got), err, queued.Seq)
			}
			if _, _, err := store.AcknowledgeReply(t.Context(), reply.Seq); err != nil {
				t.Fatalf("AcknowledgeReply 返回错误: %v", err)
			}
			if n := replyPayloadCount(t, store, reply.Seq); n != 0 {
				t.Fatalf("确认后正文行数 = %d; want 0", n)
			}
			assertAbsentOnDisk(t, storeDir(t, store), sealed, []byte(replyCanary))
		})
	}
}
