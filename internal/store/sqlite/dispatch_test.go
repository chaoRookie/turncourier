// Package sqlite 的派发测试用临时目录中的真实 SQLite 数据库验证回复的 FIFO 派发、确认、不确定与未送达的处理、
// 崩溃后的在途回复恢复、两个存储实例的并发派发，以及数据库对同一任务至多一条在途回复的兜底。
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/task"
)

// enqueueReplies 为任务记录 n 条新回复并返回各自的快照；uid 与 Message-ID 按已有入站记录数递增，不与已有邮件重复。
func enqueueReplies(t *testing.T, s *Store, taskID string, n int) []Reply {
	t.Helper()
	var replies []Reply
	for range n {
		uid := countRows(t, s.db, "inbound_messages") + 1
		in := inbound(taskID)
		in.UID = uint32(uid)
		in.MessageID = fmt.Sprintf("<%d@example.invalid>", uid)
		replies = append(replies, recordReply(t, s, in).Reply)
	}
	return replies
}

// completedTask 创建并启动任务、结束第一个回合，再为它记录 n 条回复；返回 COMPLETED 任务与回复的快照。
func completedTask(t *testing.T, s *Store, n int) (Task, []Reply) {
	t.Helper()
	current := applyEvents(t, s, startTask(t, s), task.TurnCompleted)
	return current, enqueueReplies(t, s, current.ID, n)
}

// claim 派发任务的下一条回复，失败时终止测试。
func claim(t *testing.T, s *Store, taskID string) (Reply, Task) {
	t.Helper()
	reply, current, err := s.ClaimNextReply(t.Context(), taskID)
	if err != nil {
		t.Fatalf("ClaimNextReply 返回错误: %v", err)
	}
	return reply, current
}

// mustMarkUncertain 把在途回复标为不确定，失败时终止测试。
func mustMarkUncertain(t *testing.T, s *Store, seq int64) (Reply, Task) {
	t.Helper()
	reply, current, err := s.MarkReplyUncertain(t.Context(), seq)
	if err != nil {
		t.Fatalf("MarkReplyUncertain 返回错误: %v", err)
	}
	return reply, current
}

// allReplies 按序号返回全部回复快照，用于断言失败的操作没有改动回复队列。
func allReplies(t *testing.T, s *Store) []Reply {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "SELECT seq FROM replies ORDER BY seq")
	if err != nil {
		t.Fatalf("读取回复序号失败: %v", err)
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("读取回复序号失败: %v", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("读取回复序号失败: %v", err)
	}
	var replies []Reply
	for _, seq := range seqs {
		replies = append(replies, mustGetReply(t, s, seq))
	}
	return replies
}

// requireRejected 调用 op 并断言错误包装 want，且回复队列、任务与事件记录都没有改动。
func requireRejected(t *testing.T, s *Store, name string, want error, taskID string, op func() (Reply, Task, error)) {
	t.Helper()
	replies, current, events := allReplies(t, s), mustGetTask(t, s, taskID), countRows(t, s.db, "task_events")
	if reply, got, err := op(); !errors.Is(err, want) {
		t.Errorf("%s = %+v, %+v, %v; want %v", name, reply, got, err, want)
	}
	if after := allReplies(t, s); !slices.Equal(after, replies) {
		t.Errorf("%s 改动了回复队列: %+v; want %+v", name, after, replies)
	}
	requireUnchanged(t, s, current, events)
}

// requireNoDispatch 断言对任务调用 ClaimNextReply 返回 ErrNoDispatchableReply，且没有改动任何数据。
func requireNoDispatch(t *testing.T, s *Store, taskID string) {
	t.Helper()
	requireRejected(t, s, "ClaimNextReply", ErrNoDispatchableReply, taskID, func() (Reply, Task, error) {
		return s.ClaimNextReply(t.Context(), taskID)
	})
}

// requireEvents 断言任务的事件记录依次为 want，每条格式为「事件 来源->目标」。
func requireEvents(t *testing.T, s *Store, taskID string, want ...string) {
	t.Helper()
	events, err := s.TaskEvents(t.Context(), taskID)
	if err != nil {
		t.Fatalf("TaskEvents 返回错误: %v", err)
	}
	var got []string
	for _, event := range events {
		got = append(got, fmt.Sprintf("%s %s->%s", event.Event, event.From, event.To))
	}
	if !slices.Equal(got, want) {
		t.Errorf("任务事件 = %q; want %q", got, want)
	}
}

// TestClaimNextReplyFIFO 验证派发按入队顺序进行且同一任务同时至多一条在途回复：COMPLETED 任务派发最早的回复，
// 回复进入 DISPATCHING 并记下派发前状态，任务经 reply_dispatched 进入 RUNNING；任务 RUNNING 期间即使回复已确认也不再派发，
// 确认不改动任务；回合结束后才派发下一条。另一任务更早入队的回复不会被取走。
func TestClaimNextReplyFIFO(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	_, others := completedTask(t, store, 1)
	completed, replies := completedTask(t, store, 3)
	*clock = clock.Add(time.Minute)

	reply, current, err := store.ClaimNextReply(t.Context(), completed.ID)
	wantReply := replies[0]
	wantReply.State, wantReply.ResumeState, wantReply.UpdatedAt = queue.Dispatching, task.Completed, clock.UTC()
	wantTask := completed
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.Running, completed.Version+1, clock.UTC()
	if err != nil || reply != wantReply || current != wantTask {
		t.Fatalf("ClaimNextReply = %+v, %+v, %v; want %+v, %+v, nil", reply, current, err, wantReply, wantTask)
	}
	if got := mustGetReply(t, store, reply.Seq); got != wantReply {
		t.Errorf("存储的回复 = %+v; want %+v", got, wantReply)
	}
	if got := mustGetTask(t, store, completed.ID); got != wantTask {
		t.Errorf("存储的任务 = %+v; want %+v", got, wantTask)
	}
	requireNoDispatch(t, store, completed.ID)

	*clock = clock.Add(time.Minute)
	acked, afterAck, err := store.AcknowledgeReply(t.Context(), reply.Seq)
	wantReply.State, wantReply.UpdatedAt = queue.Acknowledged, clock.UTC()
	if err != nil || acked != wantReply || afterAck != wantTask {
		t.Fatalf("AcknowledgeReply = %+v, %+v, %v; want %+v, %+v, nil", acked, afterAck, err, wantReply, wantTask)
	}
	requireNoDispatch(t, store, completed.ID)

	applyEvents(t, store, afterAck, task.TurnCompleted)
	if next, _ := claim(t, store, completed.ID); next.Seq != replies[1].Seq || next.ResumeState != task.Completed {
		t.Errorf("回合结束后派发 = %+v; want seq %d、ResumeState COMPLETED", next, replies[1].Seq)
	}
	for _, queued := range []Reply{replies[2], others[0]} {
		if got := mustGetReply(t, store, queued.Seq); got != queued {
			t.Errorf("回复 %d = %+v; want 保持 %+v", queued.Seq, got, queued)
		}
	}
	requireEvents(t, store, completed.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING")
}

// TestClaimNextReplyWaitingInput 验证 WAITING_INPUT 任务派发后回复记下派发前状态 WAITING_INPUT；
// 派发时即确认未发出而放回队列时，回复按原序号回到 QUEUED 并清除派发前状态，任务经 reply_unsent 恢复为 WAITING_INPUT。
func TestClaimNextReplyWaitingInput(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	waiting := applyEvents(t, store, startTask(t, store), task.InputRequested)
	queued := enqueueReplies(t, store, waiting.ID, 1)[0]
	reply, running := claim(t, store, waiting.ID)
	if reply.Seq != queued.Seq || reply.ResumeState != task.WaitingInput || running.State != task.Running {
		t.Fatalf("ClaimNextReply = %+v, %+v; want seq %d、ResumeState WAITING_INPUT、任务 RUNNING", reply, running, queued.Seq)
	}

	*clock = clock.Add(time.Minute)
	requeued, resumed, err := store.RequeueUnsentReply(t.Context(), reply.Seq)
	wantReply := queued
	wantReply.UpdatedAt = clock.UTC()
	wantTask := running
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.WaitingInput, running.Version+1, clock.UTC()
	if err != nil || requeued != wantReply || resumed != wantTask {
		t.Fatalf("RequeueUnsentReply = %+v, %+v, %v; want %+v, %+v, nil", requeued, resumed, err, wantReply, wantTask)
	}
	if again, _ := claim(t, store, waiting.ID); again.Seq != queued.Seq {
		t.Errorf("重新派发 = %+v; want 原序号 %d", again, queued.Seq)
	}
	requireEvents(t, store, waiting.ID,
		"start CREATED->RUNNING", "input_requested RUNNING->WAITING_INPUT", "reply_dispatched WAITING_INPUT->RUNNING",
		"reply_unsent RUNNING->WAITING_INPUT", "reply_dispatched WAITING_INPUT->RUNNING")
}

// TestClaimNextReplyNotDispatchable 验证 CanDispatchReply 为 false 的任务状态拒绝派发且不改动数据。
// 任务状态用 SQL 直接设置：FAILED 与 CLOSED 经 API 进入时会拒绝排队回复，这里保留一条 QUEUED 回复，
// 证明拒绝派发的原因是任务状态而不是空队列。
func TestClaimNextReplyNotDispatchable(t *testing.T) {
	for _, state := range []task.State{task.Running, task.WaitingApproval, task.DeliveryUncertain, task.Failed, task.Closed, task.Created} {
		t.Run(string(state), func(t *testing.T) {
			store, _ := openTaskStore(t, nil)
			current := startTask(t, store)
			enqueueReplies(t, store, current.ID, 1)
			if _, err := store.db.ExecContext(t.Context(), "UPDATE tasks SET state = ? WHERE id = ?", string(state), current.ID); err != nil {
				t.Fatalf("设置任务状态失败: %v", err)
			}
			requireNoDispatch(t, store, current.ID)
		})
	}
}

// TestClaimNextReplyWithoutQueuedReply 验证任务不存在时返回 ErrNotFound；任务可派发但没有 QUEUED 回复
// （从未收到回复，或回复都已确认或被拒绝）时返回 ErrNoDispatchableReply。
func TestClaimNextReplyWithoutQueuedReply(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	if reply, current, err := store.ClaimNextReply(t.Context(), "0000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务不存在: ClaimNextReply = %+v, %+v, %v; want ErrNotFound", reply, current, err)
	}
	empty, _ := completedTask(t, store, 0)
	requireNoDispatch(t, store, empty.ID)

	finished, _ := completedTask(t, store, 1)
	reply, _ := claim(t, store, finished.ID)
	_, running, err := store.AcknowledgeReply(t.Context(), reply.Seq)
	if err != nil {
		t.Fatalf("AcknowledgeReply 返回错误: %v", err)
	}
	applyEvents(t, store, running, task.TurnCompleted)
	requireNoDispatch(t, store, finished.ID)

	// 任务 COMPLETED 但唯一的回复已被拒绝；经 API 拒绝回复须关闭或失败任务，这里用 SQL 构造。
	rejected, _ := completedTask(t, store, 1)
	if _, err := store.db.ExecContext(t.Context(),
		"UPDATE replies SET state = 'REJECTED', reject_reason = 'task_closed' WHERE task_id = ?", rejected.ID); err != nil {
		t.Fatalf("构造 REJECTED 回复失败: %v", err)
	}
	requireNoDispatch(t, store, rejected.ID)
}

// TestInFlightReplyAfterTurnCompleted 验证任务回到可派发状态、但上一条回复仍在途（DISPATCHING 或 UNCERTAIN）时不派发，
// 返回 ErrNoDispatchableReply 而不是违反唯一索引的数据库错误。任务不是 RUNNING 时标记不确定不改动任务；
// 核对为已送达时回复 ACKNOWLEDGED、任务不执行 delivery_confirmed；核对为未送达时任务不处于派发后的状态，
// task.ResumeAfterUnsent 拒绝恢复，回复保持不变。在途回复处理完毕后才派发下一条。
func TestInFlightReplyAfterTurnCompleted(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	completed, replies := completedTask(t, store, 2)
	reply, running := claim(t, store, completed.ID)
	finished := applyEvents(t, store, running, task.TurnCompleted)
	requireNoDispatch(t, store, completed.ID)

	*clock = clock.Add(time.Minute)
	uncertain, current := mustMarkUncertain(t, store, reply.Seq)
	wantReply := reply
	wantReply.State, wantReply.UpdatedAt = queue.Uncertain, clock.UTC()
	if uncertain != wantReply || current != finished {
		t.Fatalf("MarkReplyUncertain = %+v, %+v; want %+v, %+v", uncertain, current, wantReply, finished)
	}
	requireNoDispatch(t, store, completed.ID)
	requireRejected(t, store, "ResolveUncertainReply(false)", task.ErrInvalidTransition, completed.ID, func() (Reply, Task, error) {
		return store.ResolveUncertainReply(t.Context(), reply.Seq, false)
	})

	*clock = clock.Add(time.Minute)
	acked, current, err := store.ResolveUncertainReply(t.Context(), reply.Seq, true)
	wantReply.State, wantReply.UpdatedAt = queue.Acknowledged, clock.UTC()
	if err != nil || acked != wantReply || current != finished {
		t.Fatalf("ResolveUncertainReply(true) = %+v, %+v, %v; want %+v, %+v, nil", acked, current, err, wantReply, finished)
	}
	if next, _ := claim(t, store, completed.ID); next.Seq != replies[1].Seq {
		t.Errorf("在途回复处理后派发 = %+v; want seq %d", next, replies[1].Seq)
	}
	requireEvents(t, store, completed.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING")
}

// TestMarkReplyUncertainAndConfirmDelivery 验证派发后无法确认送达时回复进入 UNCERTAIN、任务经 delivery_unknown
// 进入 DELIVERY_UNCERTAIN，此时不再派发；本地核对为已送达时回复 ACKNOWLEDGED，任务经 delivery_confirmed 回到 RUNNING。
func TestMarkReplyUncertainAndConfirmDelivery(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	completed, replies := completedTask(t, store, 2)
	reply, running := claim(t, store, completed.ID)

	*clock = clock.Add(time.Minute)
	uncertain, current, err := store.MarkReplyUncertain(t.Context(), reply.Seq)
	wantReply := reply
	wantReply.State, wantReply.UpdatedAt = queue.Uncertain, clock.UTC()
	wantTask := running
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.DeliveryUncertain, running.Version+1, clock.UTC()
	if err != nil || uncertain != wantReply || current != wantTask {
		t.Fatalf("MarkReplyUncertain = %+v, %+v, %v; want %+v, %+v, nil", uncertain, current, err, wantReply, wantTask)
	}
	requireNoDispatch(t, store, completed.ID)

	*clock = clock.Add(time.Minute)
	acked, current, err := store.ResolveUncertainReply(t.Context(), reply.Seq, true)
	wantReply.State, wantReply.UpdatedAt = queue.Acknowledged, clock.UTC()
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.Running, wantTask.Version+1, clock.UTC()
	if err != nil || acked != wantReply || current != wantTask {
		t.Fatalf("ResolveUncertainReply(true) = %+v, %+v, %v; want %+v, %+v, nil", acked, current, err, wantReply, wantTask)
	}
	if got := mustGetTask(t, store, completed.ID); got != wantTask {
		t.Errorf("存储的任务 = %+v; want %+v", got, wantTask)
	}
	if got := mustGetReply(t, store, replies[1].Seq); got != replies[1] {
		t.Errorf("回复 %d = %+v; want 保持 %+v", replies[1].Seq, got, replies[1])
	}
	requireEvents(t, store, completed.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"delivery_unknown RUNNING->DELIVERY_UNCERTAIN", "delivery_confirmed DELIVERY_UNCERTAIN->RUNNING")
}

// TestResolveUncertainReplyNotDelivered 验证本地核对为未送达时回复按原序号回到 QUEUED 并清除派发前状态，
// 任务经 reply_unsent 恢复为 COMPLETED；再次派发仍得到同一序号，排在后入队的回复之前。
func TestResolveUncertainReplyNotDelivered(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	completed, replies := completedTask(t, store, 2)
	reply, _ := claim(t, store, completed.ID)
	_, uncertainTask := mustMarkUncertain(t, store, reply.Seq)

	*clock = clock.Add(time.Minute)
	requeued, current, err := store.ResolveUncertainReply(t.Context(), reply.Seq, false)
	wantReply := replies[0]
	wantReply.UpdatedAt = clock.UTC()
	wantTask := uncertainTask
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.Completed, uncertainTask.Version+1, clock.UTC()
	if err != nil || requeued != wantReply || current != wantTask {
		t.Fatalf("ResolveUncertainReply(false) = %+v, %+v, %v; want %+v, %+v, nil", requeued, current, err, wantReply, wantTask)
	}
	if again, _ := claim(t, store, completed.ID); again.Seq != replies[0].Seq || again.ResumeState != task.Completed {
		t.Errorf("重新派发 = %+v; want 原序号 %d、ResumeState COMPLETED", again, replies[0].Seq)
	}
	requireEvents(t, store, completed.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"delivery_unknown RUNNING->DELIVERY_UNCERTAIN", "reply_unsent DELIVERY_UNCERTAIN->COMPLETED",
		"reply_dispatched COMPLETED->RUNNING")
}

// TestRequeueUnsentReply 验证派发时即确认未发出的回复按原序号回到 QUEUED，任务经 reply_unsent 恢复为派发前的 COMPLETED。
func TestRequeueUnsentReply(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	completed, replies := completedTask(t, store, 2)
	reply, running := claim(t, store, completed.ID)

	*clock = clock.Add(time.Minute)
	requeued, current, err := store.RequeueUnsentReply(t.Context(), reply.Seq)
	wantReply := replies[0]
	wantReply.UpdatedAt = clock.UTC()
	wantTask := running
	wantTask.State, wantTask.Version, wantTask.UpdatedAt = task.Completed, running.Version+1, clock.UTC()
	if err != nil || requeued != wantReply || current != wantTask {
		t.Fatalf("RequeueUnsentReply = %+v, %+v, %v; want %+v, %+v, nil", requeued, current, err, wantReply, wantTask)
	}
	if again, _ := claim(t, store, completed.ID); again.Seq != replies[0].Seq {
		t.Errorf("重新派发 = %+v; want 原序号 %d", again, replies[0].Seq)
	}
	requireEvents(t, store, completed.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"reply_unsent RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING")
}

// TestUnsentReplyRejectedWhenTaskStopped 验证在途回复确认未送达、而任务在此期间已关闭或失败时，
// 回复改为 REJECTED（原因 task_closed 或 task_failed）并保留派发前状态，任务状态、版本与事件记录都不变。
func TestUnsentReplyRejectedWhenTaskStopped(t *testing.T) {
	tests := []struct {
		name      string
		uncertain bool
		stop      task.Event
		reason    string
	}{
		{"RequeueUnsentReply 任务已关闭", false, task.Close, "task_closed"},
		{"RequeueUnsentReply 任务已失败", false, task.Fail, "task_failed"},
		{"ResolveUncertainReply 任务已关闭", true, task.Close, "task_closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, clock := openTaskStore(t, nil)
			completed, _ := completedTask(t, store, 1)
			reply, current := claim(t, store, completed.ID)
			if tt.uncertain {
				reply, current = mustMarkUncertain(t, store, reply.Seq)
			}
			stopped := applyEvents(t, store, current, tt.stop)
			events := countRows(t, store.db, "task_events")

			*clock = clock.Add(time.Minute)
			var got Reply
			var err error
			if tt.uncertain {
				got, current, err = store.ResolveUncertainReply(t.Context(), reply.Seq, false)
			} else {
				got, current, err = store.RequeueUnsentReply(t.Context(), reply.Seq)
			}
			want := reply
			want.State, want.RejectReason, want.UpdatedAt = queue.Rejected, tt.reason, clock.UTC()
			if err != nil || got != want || current != stopped {
				t.Fatalf("got %+v, %+v, %v; want %+v, %+v, nil", got, current, err, want, stopped)
			}
			requireUnchanged(t, store, stopped, events)
		})
	}
}

// TestReplyOperationsRejectInvalidState 验证按序号操作的方法只接受各自的来源状态：对其他状态的回复返回
// queue.ErrInvalidTransition（包括队列状态机允许、但不属于该方法的来源状态，例如对 UNCERTAIN 回复调用 AcknowledgeReply），
// 序号不存在时返回 ErrNotFound；两种情况都不改动任何数据。
func TestReplyOperationsRejectInvalidState(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	queuedTask, queued := completedTask(t, store, 1)
	dispatchingTask, _ := completedTask(t, store, 1)
	dispatching, _ := claim(t, store, dispatchingTask.ID)
	uncertainTask, _ := completedTask(t, store, 1)
	uncertain, _ := claim(t, store, uncertainTask.ID)
	uncertain, _ = mustMarkUncertain(t, store, uncertain.Seq)
	ackedTask, _ := completedTask(t, store, 1)
	acked, _ := claim(t, store, ackedTask.ID)
	if _, _, err := store.AcknowledgeReply(t.Context(), acked.Seq); err != nil {
		t.Fatalf("AcknowledgeReply 返回错误: %v", err)
	}
	rejectedTask, rejected := completedTask(t, store, 1)
	applyEvents(t, store, rejectedTask, task.Close)
	tasks := map[int64]string{
		queued[0].Seq: queuedTask.ID, dispatching.Seq: dispatchingTask.ID, uncertain.Seq: uncertainTask.ID,
		acked.Seq: ackedTask.ID, rejected[0].Seq: rejectedTask.ID,
	}

	operations := []struct {
		name string
		from queue.State
		call func(seq int64) (Reply, Task, error)
	}{
		{"AcknowledgeReply", queue.Dispatching, func(seq int64) (Reply, Task, error) { return store.AcknowledgeReply(t.Context(), seq) }},
		{"MarkReplyUncertain", queue.Dispatching, func(seq int64) (Reply, Task, error) { return store.MarkReplyUncertain(t.Context(), seq) }},
		{"RequeueUnsentReply", queue.Dispatching, func(seq int64) (Reply, Task, error) { return store.RequeueUnsentReply(t.Context(), seq) }},
		{"ResolveUncertainReply(true)", queue.Uncertain, func(seq int64) (Reply, Task, error) {
			return store.ResolveUncertainReply(t.Context(), seq, true)
		}},
		{"ResolveUncertainReply(false)", queue.Uncertain, func(seq int64) (Reply, Task, error) {
			return store.ResolveUncertainReply(t.Context(), seq, false)
		}},
	}
	for _, op := range operations {
		for seq, taskID := range tasks {
			if state := mustGetReply(t, store, seq).State; state != op.from {
				requireRejected(t, store, fmt.Sprintf("%s(%s 回复)", op.name, state), queue.ErrInvalidTransition, taskID,
					func() (Reply, Task, error) { return op.call(seq) })
			}
		}
		requireRejected(t, store, op.name+"(不存在的序号)", ErrNotFound, queuedTask.ID,
			func() (Reply, Task, error) { return op.call(999) })
	}
}

// TestRecoverInFlightAfterCrash 在派发后不做任何收尾直接关闭存储，模拟进程崩溃，重新打开后验证：
// RecoverInFlight 把全部 DISPATCHING 回复改为 UNCERTAIN，其中任务为 RUNNING 的经 delivery_unknown 进入 DELIVERY_UNCERTAIN，
// 已离开 RUNNING 的任务保持不变；按序号返回全部 UNCERTAIN 回复（含崩溃前已标记的）。再次调用结果与数据都不变，
// 且任何回复都不会被自动重新派发。
func TestRecoverInFlightAfterCrash(t *testing.T) {
	dir := dataDir(t)
	first := openStore(t, dir)
	if recovered, err := first.RecoverInFlight(t.Context()); err != nil || len(recovered) != 0 {
		t.Fatalf("空库 RecoverInFlight = %+v, %v; want 空, nil", recovered, err)
	}
	dispatched, dispatchedReplies := completedTask(t, first, 2)
	claim(t, first, dispatched.ID)
	finished, finishedReplies := completedTask(t, first, 1)
	_, running := claim(t, first, finished.ID)
	applyEvents(t, first, running, task.TurnCompleted)
	marked, _ := completedTask(t, first, 1)
	markedReply, _ := claim(t, first, marked.ID)
	mustMarkUncertain(t, first, markedReply.Seq)
	if err := first.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	second := openStore(t, dir)
	finishedBefore, markedBefore := mustGetTask(t, second, finished.ID), mustGetTask(t, second, marked.ID)
	recovered, err := second.RecoverInFlight(t.Context())
	if err != nil {
		t.Fatalf("RecoverInFlight 返回错误: %v", err)
	}
	var got []string
	for _, reply := range recovered {
		if stored := mustGetReply(t, second, reply.Seq); stored != reply {
			t.Errorf("返回的回复 %+v 与存储的 %+v 不一致", reply, stored)
		}
		got = append(got, fmt.Sprintf("%d %s %s %s", reply.Seq, reply.TaskID, reply.State, reply.ResumeState))
	}
	want := []string{
		fmt.Sprintf("%d %s UNCERTAIN COMPLETED", dispatchedReplies[0].Seq, dispatched.ID),
		fmt.Sprintf("%d %s UNCERTAIN COMPLETED", finishedReplies[0].Seq, finished.ID),
		fmt.Sprintf("%d %s UNCERTAIN COMPLETED", markedReply.Seq, marked.ID),
	}
	if !slices.Equal(got, want) {
		t.Errorf("RecoverInFlight = %q; want %q", got, want)
	}
	if got := mustGetTask(t, second, dispatched.ID).State; got != task.DeliveryUncertain {
		t.Errorf("RUNNING 任务恢复后为 %s; want DELIVERY_UNCERTAIN", got)
	}
	for _, before := range []Task{finishedBefore, markedBefore} {
		if got := mustGetTask(t, second, before.ID); got != before {
			t.Errorf("任务 %s 恢复后 = %+v; want 保持 %+v", before.ID, got, before)
		}
	}
	if got := mustGetReply(t, second, dispatchedReplies[1].Seq); got != dispatchedReplies[1] {
		t.Errorf("排队回复 = %+v; want 保持 %+v", got, dispatchedReplies[1])
	}
	requireEvents(t, second, dispatched.ID,
		"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING",
		"delivery_unknown RUNNING->DELIVERY_UNCERTAIN")

	replies, events := allReplies(t, second), countRows(t, second.db, "task_events")
	again, err := second.RecoverInFlight(t.Context())
	if err != nil || !slices.Equal(again, recovered) {
		t.Errorf("再次 RecoverInFlight = %+v, %v; want %+v, nil", again, err, recovered)
	}
	if after := allReplies(t, second); !slices.Equal(after, replies) {
		t.Errorf("再次 RecoverInFlight 改动了回复队列: %+v; want %+v", after, replies)
	}
	if got := countRows(t, second.db, "task_events"); got != events {
		t.Errorf("再次 RecoverInFlight 后 task_events 行数 = %d; want %d", got, events)
	}
	for _, id := range []string{dispatched.ID, finished.ID, marked.ID} {
		requireNoDispatch(t, second, id)
	}
}

// TestClaimNextReplyAcrossProcesses 用两个独立打开同一数据目录的存储模拟两个进程：每轮 20 个 goroutine
// 同时派发同一任务的回复，恰好 1 个成功，其余都返回 ErrNoDispatchableReply，不泄漏 SQLITE_BUSY 等错误；循环 20 轮。
func TestClaimNextReplyAcrossProcesses(t *testing.T) {
	dir := dataDir(t)
	stores := []*Store{openStore(t, dir), openStore(t, dir)}
	for round := range 20 {
		completed, replies := completedTask(t, stores[round%2], 1)
		start := make(chan struct{})
		errs := make(chan error, 20)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				<-start
				_, _, err := stores[i%2].ClaimNextReply(t.Context(), completed.ID)
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		succeeded := 0
		for err := range errs {
			if err == nil {
				succeeded++
			} else if !errors.Is(err, ErrNoDispatchableReply) {
				t.Errorf("第 %d 轮: ClaimNextReply 返回意外错误: %v", round, err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("第 %d 轮: %d 次派发成功; want 恰好 1 次", round, succeeded)
		}
		if got := mustGetReply(t, stores[(round+1)%2], replies[0].Seq).State; got != queue.Dispatching {
			t.Errorf("第 %d 轮: 回复状态 = %s; want DISPATCHING", round, got)
		}
		requireEvents(t, stores[(round+1)%2], completed.ID,
			"start CREATED->RUNNING", "turn_completed RUNNING->COMPLETED", "reply_dispatched COMPLETED->RUNNING")
	}
}

// TestInFlightUniqueIndex 绕过 API 直接插入同一任务的第二条在途回复（DISPATCHING 或 UNCERTAIN），
// 验证 replies_one_in_flight 部分唯一索引拒绝写入；同一任务再插入一条 QUEUED 回复可以成功，证明失败只来自在途状态。
func TestInFlightUniqueIndex(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	completed, _ := completedTask(t, store, 1)
	claim(t, store, completed.ID)
	for i, state := range []queue.State{queue.Dispatching, queue.Uncertain, queue.Queued} {
		// 每次先插入独立的入站记录，使新回复满足 inbound_id 的外键与唯一约束。
		var inboundID int64
		if err := store.db.QueryRowContext(t.Context(),
			"INSERT INTO inbound_messages (account, uid_validity, uid, message_id, body_sha256, task_id, received_at) VALUES (?, 7, ?, ?, ?, ?, 0) RETURNING id",
			botAccount, 100+i, fmt.Sprintf("<direct-%d@example.invalid>", i), digestA[:], completed.ID).Scan(&inboundID); err != nil {
			t.Fatalf("插入入站记录失败: %v", err)
		}
		_, err := store.db.ExecContext(t.Context(),
			"INSERT INTO replies (inbound_id, task_id, state, resume_state, created_at, updated_at) VALUES (?, ?, ?, 'COMPLETED', 0, 0)",
			inboundID, completed.ID, string(state))
		if state == queue.Queued {
			if err != nil {
				t.Errorf("插入 QUEUED 回复失败: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: replies.task_id") {
			t.Errorf("插入第二条 %s 回复: err = %v; want 违反 replies_one_in_flight", state, err)
		}
	}
}

// TestReplyOperationsRollBackOnFailure 用测试内创建的触发器让派发、标记不确定、核对、放回队列与恢复中的某一步写入失败，
// 或让带状态、版本条件的更新不影响任何行，验证方法返回错误且整个事务回滚：回复、任务与事件记录都保持操作前的样子。
func TestReplyOperationsRollBackOnFailure(t *testing.T) {
	faults := []struct {
		name    string
		trigger string
		want    string
	}{
		{"回复更新失败", "BEFORE UPDATE ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom"},
		{"回复更新未命中", "BEFORE UPDATE ON replies BEGIN SELECT RAISE(IGNORE); END", "changed during update"},
		{"任务更新失败", "BEFORE UPDATE ON tasks BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom"},
		{"任务更新未命中", "BEFORE UPDATE ON tasks BEGIN SELECT RAISE(IGNORE); END", "changed during update"},
		{"事件写入失败", "BEFORE INSERT ON task_events BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom"},
	}
	// 每个操作的准备函数构造一个会同时写回复、任务与事件的场景，返回任务 ID 与待执行的操作。
	operations := []struct {
		name    string
		prepare func(t *testing.T, s *Store) (string, func() error)
	}{
		{"ClaimNextReply", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			return completed.ID, func() error { _, _, err := s.ClaimNextReply(t.Context(), completed.ID); return err }
		}},
		{"MarkReplyUncertain", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			reply, _ := claim(t, s, completed.ID)
			return completed.ID, func() error { _, _, err := s.MarkReplyUncertain(t.Context(), reply.Seq); return err }
		}},
		{"RequeueUnsentReply", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			reply, _ := claim(t, s, completed.ID)
			return completed.ID, func() error { _, _, err := s.RequeueUnsentReply(t.Context(), reply.Seq); return err }
		}},
		{"ResolveUncertainReply(true)", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			reply, _ := claim(t, s, completed.ID)
			mustMarkUncertain(t, s, reply.Seq)
			return completed.ID, func() error { _, _, err := s.ResolveUncertainReply(t.Context(), reply.Seq, true); return err }
		}},
		{"ResolveUncertainReply(false)", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			reply, _ := claim(t, s, completed.ID)
			mustMarkUncertain(t, s, reply.Seq)
			return completed.ID, func() error { _, _, err := s.ResolveUncertainReply(t.Context(), reply.Seq, false); return err }
		}},
		{"RecoverInFlight", func(t *testing.T, s *Store) (string, func() error) {
			completed, _ := completedTask(t, s, 1)
			claim(t, s, completed.ID)
			return completed.ID, func() error { _, err := s.RecoverInFlight(t.Context()); return err }
		}},
	}
	for _, op := range operations {
		for _, fault := range faults {
			t.Run(op.name+"/"+fault.name, func(t *testing.T) {
				store, _ := openTaskStore(t, nil)
				taskID, run := op.prepare(t, store)
				if _, err := store.db.ExecContext(t.Context(), "CREATE TRIGGER fault "+fault.trigger); err != nil {
					t.Fatalf("创建触发器失败: %v", err)
				}
				replies, before, events := allReplies(t, store), mustGetTask(t, store, taskID), countRows(t, store.db, "task_events")
				if err := run(); err == nil || !strings.Contains(err.Error(), fault.want) {
					t.Errorf("err = %v; want 含 %q 的错误", err, fault.want)
				}
				if after := allReplies(t, store); !slices.Equal(after, replies) {
					t.Errorf("回复队列未回滚: %+v; want %+v", after, replies)
				}
				requireUnchanged(t, store, before, events)
			})
		}
	}
}

// TestReplyOperationsCanceledContext 验证上下文已取消时各方法在开始事务时返回 context.Canceled，不改动任何数据。
func TestReplyOperationsCanceledContext(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	completed, replies := completedTask(t, store, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() error{
		"ClaimNextReply":        func() error { _, _, err := store.ClaimNextReply(ctx, completed.ID); return err },
		"AcknowledgeReply":      func() error { _, _, err := store.AcknowledgeReply(ctx, replies[0].Seq); return err },
		"ResolveUncertainReply": func() error { _, _, err := store.ResolveUncertainReply(ctx, replies[0].Seq, true); return err },
		"RecoverInFlight":       func() error { _, err := store.RecoverInFlight(ctx); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v; want context.Canceled", name, err)
		}
	}
	if got := mustGetReply(t, store, replies[0].Seq); got != replies[0] {
		t.Errorf("回复 = %+v; want 保持 %+v", got, replies[0])
	}
	requireUnchanged(t, store, completed, 2)
}
