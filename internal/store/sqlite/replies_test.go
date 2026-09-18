// Package sqlite 的入站回复测试用临时目录中的真实 SQLite 数据库验证去重、冲突、拒绝原因、字段校验、原子性，
// 以及 fail 与 close 对排队回复的拒绝。
package sqlite

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/task"
)

// botAccount 是测试使用的合成机器人邮箱地址。
const botAccount = "bot@example.invalid"

var (
	// digestA 是合成正文 A 的 SHA-256 摘要。
	digestA = sha256.Sum256([]byte("synthetic reply A"))
	// digestB 是合成正文 B 的 SHA-256 摘要。
	digestB = sha256.Sum256([]byte("synthetic reply B"))
)

// inbound 返回任务 taskID 的一条合成入站回复：uv=7、uid=1、<a@example.invalid>、摘要 A；测试按需修改字段。
func inbound(taskID string) InboundReply {
	return InboundReply{
		TaskID:      taskID,
		Account:     botAccount,
		UIDValidity: 7,
		UID:         1,
		MessageID:   "<a@example.invalid>",
		BodySHA256:  digestA,
	}
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
		{"同 uv/uid、同 Message-ID、摘要 B", func(in *InboundReply) { in.BodySHA256 = digestB }},
		{"新 uid、同 Message-ID、摘要 B", func(in *InboundReply) { in.UID, in.BodySHA256 = 3, digestB }},
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
// 以区别于数据库 CHECK 约束与任务查询的报错，且不写入任何记录。边界上的合法值可以写入。
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
		{"UIDValidity 为 0", func(in *InboundReply) { in.UIDValidity = 0 }},
		{"UID 为 0", func(in *InboundReply) { in.UID = 0 }},
		{"MessageID 为空", func(in *InboundReply) { in.MessageID = "" }},
		{"MessageID 少于 3 字符", func(in *InboundReply) { in.MessageID = "<a" }},
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
	requireRows(t, store, 0, 0)

	in := inbound(running.ID)
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
	requireRows(t, store, 2, 2)
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

// TestRecordReplyRejectsUnknownReplyState 验证读取到不在已知集合的回复状态时返回错误，而不是静默接受。
// 表上的 CHECK 约束本会拒绝未知状态，测试临时关闭它，模拟被外部工具改坏的数据库。
func TestRecordReplyRejectsUnknownReplyState(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	recordReply(t, store, inbound(running.ID))
	for _, query := range []string{
		"PRAGMA ignore_check_constraints = ON",
		"UPDATE replies SET state = 'queued'",
		"PRAGMA ignore_check_constraints = OFF",
	} {
		if _, err := store.db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("执行 %q 失败: %v", query, err)
		}
	}
	if got, err := store.RecordReply(t.Context(), inbound(running.ID)); err == nil || !strings.Contains(err.Error(), "queued") {
		t.Errorf("RecordReply = %+v, %v; want 未知状态错误", got, err)
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
			in := inbound(taskID)
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
