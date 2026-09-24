// Package sqlite 的任务持久化测试用临时目录中的真实 SQLite 数据库验证创建、启动、事件、版本冲突、失败回滚与事件记录，
// 邮件触发的持久暂停，以及 4b 新增接口的输入校验都包装 ErrInvalidArgument。
package sqlite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/task"
)

// openTaskStore 打开使用可调时钟、给定随机源与 kid 1 测试正文密钥的存储，并登记 (token, 1) 与 (payload, 1)；
// random 为 nil 时使用 crypto/rand。返回的指针指向注入时钟的当前读数，测试修改它来推进时间；时钟位于 UTC+8，
// 用来验证存储统一换算为 UTC。
func openTaskStore(t *testing.T, random io.Reader) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 18, 16, 30, 0, 123_000_000, time.FixedZone("UTC+8", 8*60*60))
	store, err := Open(t.Context(), dataDir(t), Options{Now: func() time.Time { return now }, Random: random, PayloadKey: newTestPayloadKey(t, 1)})
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	registerTestKeys(t, store)
	return store, &now
}

// createTask 创建一个 codex 任务，失败时终止测试。
func createTask(t *testing.T, s *Store) Task {
	t.Helper()
	created, err := s.CreateTask(t.Context(), AgentCodex)
	if err != nil {
		t.Fatalf("CreateTask 返回错误: %v", err)
	}
	return created
}

// startTask 创建任务并以合成会话 ID 启动，返回 RUNNING 状态的快照，失败时终止测试。
func startTask(t *testing.T, s *Store) Task {
	t.Helper()
	created := createTask(t, s)
	started, err := s.StartTask(t.Context(), created.ID, created.Version, "thread-synthetic-0001")
	if err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	return started
}

// applyEvents 依次对任务执行事件，每次使用上一次返回的版本，返回最后的快照；任一事件失败时终止测试。
func applyEvents(t *testing.T, s *Store, current Task, events ...task.Event) Task {
	t.Helper()
	for _, event := range events {
		next, err := s.ApplyTaskEvent(t.Context(), current.ID, current.Version, event)
		if err != nil {
			t.Fatalf("ApplyTaskEvent(%s) 返回错误: %v", event, err)
		}
		current = next
	}
	return current
}

// mustGetTask 按 ID 读取任务，失败时终止测试。
func mustGetTask(t *testing.T, s *Store, id string) Task {
	t.Helper()
	got, err := s.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask 返回错误: %v", err)
	}
	return got
}

// requireUnchanged 断言任务快照与 task_events 行数和操作前一致，用于验证失败的操作没有改动数据库。
func requireUnchanged(t *testing.T, s *Store, before Task, events int) {
	t.Helper()
	if after := mustGetTask(t, s, before.ID); after != before {
		t.Errorf("任务被改动: %+v; want %+v", after, before)
	}
	if got := countRows(t, s.db, "task_events"); got != events {
		t.Errorf("task_events 行数 = %d; want %d", got, events)
	}
}

// TestCreateTask 验证新任务为 CREATED、版本 1、属主 local，ID 来自注入的随机源，时间来自注入时钟并以 UTC 毫秒存储。
func TestCreateTask(t *testing.T) {
	for _, agent := range []Agent{AgentCodex, AgentClaude} {
		t.Run(string(agent), func(t *testing.T) {
			store, clock := openTaskStore(t, bytes.NewReader([]byte{0, 0, 0, 0, 0, 0, 0}))
			created, err := store.CreateTask(t.Context(), agent)
			want := Task{ID: "0000000000", Owner: "local", Agent: agent, State: task.Created, Version: 1, CreatedAt: clock.UTC(), UpdatedAt: clock.UTC()}
			if err != nil || created != want {
				t.Fatalf("CreateTask = %+v, %v; want %+v, nil", created, err, want)
			}
			if got := mustGetTask(t, store, created.ID); got != want {
				t.Errorf("GetTask = %+v; want %+v", got, want)
			}
			var createdAt, updatedAt int64
			if err := store.db.QueryRowContext(t.Context(), "SELECT created_at, updated_at FROM tasks").Scan(&createdAt, &updatedAt); err != nil {
				t.Fatalf("读取时间列失败: %v", err)
			}
			if createdAt != clock.UnixMilli() || updatedAt != clock.UnixMilli() {
				t.Errorf("存储的时间 = %d, %d; want %d", createdAt, updatedAt, clock.UnixMilli())
			}
			if events, err := store.TaskEvents(t.Context(), created.ID); err != nil || len(events) != 0 {
				t.Errorf("TaskEvents = %v, %v; want 空, nil", events, err)
			}
		})
	}
}

// TestCreateTaskRejectsUnknownAgent 验证未知或大小写不符的 Agent 类型在写库前被 Go 端校验拒绝，
// 错误文本须含 "unknown agent" 并包装 ErrInvalidArgument，以区别于 agent 列 CHECK 约束的报错；且不写入任何任务。
func TestCreateTaskRejectsUnknownAgent(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	for _, agent := range []Agent{"", "gemini", "Codex", "claude "} {
		if got, err := store.CreateTask(t.Context(), agent); err == nil || !strings.Contains(err.Error(), "unknown agent") || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreateTask(%q) = %+v, %v; want 包装 ErrInvalidArgument、含 \"unknown agent\" 的错误", agent, got, err)
		}
	}
	if got := countRows(t, store.db, "tasks"); got != 0 {
		t.Errorf("tasks 行数 = %d; want 0", got)
	}
}

// TestCreateTaskRetriesIDCollision 验证 ID 与已有任务冲突时换新 ID 重试，一共最多尝试 3 次：
// 第 2、3 次给出新 ID 时成功；连续 3 次冲突时报错，随机源中排在后面的新 ID 不会被读取，也不写入任何任务。
func TestCreateTaskRetriesIDCollision(t *testing.T) {
	existing := []byte{0, 0, 0, 0, 0, 0, 0} // "0000000000"
	fresh := []byte{0, 0, 0, 0, 0, 0, 0x40} // "0000000001"
	tests := []struct {
		name       string
		duplicates int
		wantErr    bool
	}{
		{"第 2 次成功", 1, false},
		{"第 3 次成功", 2, false},
		{"连续 3 次冲突", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			random := new(bytes.Buffer)
			store, _ := openTaskStore(t, random)
			random.Write(existing)
			createTask(t, store)
			for range tt.duplicates {
				random.Write(existing)
			}
			random.Write(fresh)

			got, err := store.CreateTask(t.Context(), AgentCodex)
			if !tt.wantErr {
				if err != nil || got.ID != "0000000001" {
					t.Errorf("CreateTask = %+v, %v; want ID 0000000001", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateTask = %+v, nil; want 错误", got)
			}
			if random.Len() != len(fresh) {
				t.Errorf("随机源剩余 %d 字节; want %d（第 4 个 ID 不应被读取）", random.Len(), len(fresh))
			}
			if rows := countRows(t, store.db, "tasks"); rows != 1 {
				t.Errorf("tasks 行数 = %d; want 1", rows)
			}
		})
	}
}

// TestCreateTaskRandomFailure 验证随机源读取失败时返回错误，不写入任何任务。
func TestCreateTaskRandomFailure(t *testing.T) {
	store, _ := openTaskStore(t, new(bytes.Buffer))
	if got, err := store.CreateTask(t.Context(), AgentCodex); err == nil {
		t.Errorf("CreateTask = %+v, nil; want 错误", got)
	}
	if rows := countRows(t, store.db, "tasks"); rows != 0 {
		t.Errorf("tasks 行数 = %d; want 0", rows)
	}
}

// TestGetTaskNotFound 验证读取不存在的任务返回 ErrNotFound。
func TestGetTaskNotFound(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	if _, err := store.GetTask(t.Context(), "0000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask 错误 = %v; want ErrNotFound", err)
	}
}

// TestGetTaskRejectsUnknownState 验证数据库中的状态名不在已知集合时返回错误，而不是静默接受。
// 表上的 CHECK 约束本会拒绝未知状态，测试临时关闭它，模拟被外部工具改坏的数据库。
func TestGetTaskRejectsUnknownState(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	for _, query := range []string{
		"PRAGMA ignore_check_constraints = ON",
		"INSERT INTO tasks (id, agent, state, version, created_at, updated_at) VALUES ('0000000000', 'codex', 'running', 1, 0, 0)",
		"PRAGMA ignore_check_constraints = OFF",
	} {
		if _, err := store.db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("执行 %q 失败: %v", query, err)
		}
	}
	_, err := store.GetTask(t.Context(), "0000000000")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "running") {
		t.Errorf("GetTask 错误 = %v; want 未知状态错误", err)
	}
}

// TestStartTask 验证启动后任务为 RUNNING、版本 2、会话 ID 已保存，更新时间来自注入时钟而创建时间不变。
func TestStartTask(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	created := createTask(t, store)
	*clock = clock.Add(time.Second)
	started, err := store.StartTask(t.Context(), created.ID, created.Version, "thread-synthetic-0001")
	want := created
	want.State = task.Running
	want.Version = 2
	want.SessionID = "thread-synthetic-0001"
	want.UpdatedAt = clock.UTC()
	if err != nil || started != want {
		t.Fatalf("StartTask = %+v, %v; want %+v, nil", started, err, want)
	}
	if got := mustGetTask(t, store, created.ID); got != want {
		t.Errorf("GetTask = %+v; want %+v", got, want)
	}
}

// TestStartTaskSessionID 验证会话 ID 须为 1–200 个字母、数字或 . _ : -，否则返回 ErrInvalidSessionID（它同时满足
// errors.Is(err, ErrInvalidArgument)，文本仍为原样）且不改动数据库；恰好 200 个字符且包含每类允许字符的会话 ID 可以写入。
func TestStartTaskSessionID(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	created := createTask(t, store)
	for _, sessionID := range []string{"", strings.Repeat("a", 201), "thread 1", "thread-1\n", "thread/1", "会话-1"} {
		if _, err := store.StartTask(t.Context(), created.ID, created.Version, sessionID); !errors.Is(err, ErrInvalidSessionID) ||
			!errors.Is(err, ErrInvalidArgument) || err.Error() != "invalid agent session id" {
			t.Errorf("StartTask(%q) 错误 = %v; want 同时包装 ErrInvalidArgument 的 ErrInvalidSessionID", sessionID, err)
		}
	}
	requireUnchanged(t, store, created, 0)

	valid := strings.Repeat("aZ09._:-", 25)
	started, err := store.StartTask(t.Context(), created.ID, created.Version, valid)
	if err != nil || started.SessionID != valid {
		t.Errorf("StartTask = %+v, %v; want 会话 ID 为 200 个字符", started, err)
	}
}

// TestStartTaskErrors 验证任务不存在、版本过期与重复启动的错误，失败时数据库不变。
// 版本比较先于状态机校验：持有过期版本的调用方总是得到 ErrVersionConflict，而不是基于过时状态的非法转移错误。
func TestStartTaskErrors(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	if _, err := store.StartTask(t.Context(), "missing", 1, "thread-synthetic-0001"); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务不存在: 错误 = %v; want ErrNotFound", err)
	}
	created := createTask(t, store)
	for _, version := range []int64{0, 2} {
		if _, err := store.StartTask(t.Context(), created.ID, version, "thread-synthetic-0001"); !errors.Is(err, ErrVersionConflict) {
			t.Errorf("版本 %d: 错误 = %v; want ErrVersionConflict", version, err)
		}
	}
	requireUnchanged(t, store, created, 0)

	started, err := store.StartTask(t.Context(), created.ID, created.Version, "thread-synthetic-0001")
	if err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	if _, err := store.StartTask(t.Context(), started.ID, started.Version, "thread-synthetic-0002"); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("再次启动: 错误 = %v; want task.ErrInvalidTransition", err)
	}
	if _, err := store.StartTask(t.Context(), started.ID, created.Version, "thread-synthetic-0002"); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("以过期版本再次启动: 错误 = %v; want ErrVersionConflict", err)
	}
	requireUnchanged(t, store, started, 1)
}

// TestApplyTaskEvent 验证六个允许的事件各有一条成功路径：状态按状态机变化、版本加 1、更新时间来自注入时钟，
// 并追加一条记录了事件与前后状态的任务事件。
func TestApplyTaskEvent(t *testing.T) {
	tests := []struct {
		setup []task.Event
		event task.Event
		want  task.State
	}{
		{nil, task.TurnCompleted, task.Completed},
		{nil, task.InputRequested, task.WaitingInput},
		{nil, task.ApprovalRequested, task.WaitingApproval},
		{[]task.Event{task.ApprovalRequested}, task.ApprovalResolved, task.Running},
		{nil, task.Fail, task.Failed},
		{[]task.Event{task.TurnCompleted}, task.Close, task.Closed},
	}
	for _, tt := range tests {
		t.Run(string(tt.event), func(t *testing.T) {
			store, clock := openTaskStore(t, nil)
			before := applyEvents(t, store, startTask(t, store), tt.setup...)
			*clock = clock.Add(time.Minute)
			got, err := store.ApplyTaskEvent(t.Context(), before.ID, before.Version, tt.event)
			want := before
			want.State = tt.want
			want.Version = before.Version + 1
			want.UpdatedAt = clock.UTC()
			if err != nil || got != want {
				t.Fatalf("ApplyTaskEvent = %+v, %v; want %+v, nil", got, err, want)
			}
			if got := mustGetTask(t, store, before.ID); got != want {
				t.Errorf("GetTask = %+v; want %+v", got, want)
			}
			events, err := store.TaskEvents(t.Context(), before.ID)
			if err != nil || len(events) != 2+len(tt.setup) {
				t.Fatalf("TaskEvents = %+v, %v; want %d 条", events, err, 2+len(tt.setup))
			}
			last := events[len(events)-1]
			if last.Event != tt.event || last.From != before.State || last.To != tt.want || !last.At.Equal(want.UpdatedAt) {
				t.Errorf("最后一条事件 = %+v; want %s: %s -> %s at %v", last, tt.event, before.State, tt.want, want.UpdatedAt)
			}
		})
	}
}

// TestApplyTaskEventRejectsReservedEvents 验证 start、reply_dispatched、delivery_unknown、delivery_confirmed 与未知事件
// 返回同时包装 task.ErrInvalidTransition 与 ErrInvalidArgument 的错误且不改动数据库。每个保留事件都在状态机本会接受它的状态下调用，
// 证明拒绝来自 ApplyTaskEvent 本身的输入校验，而不是状态机；状态机拒绝的转移不是输入校验错误。
func TestApplyTaskEventRejectsReservedEvents(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	created := createTask(t, store)
	running := startTask(t, store)
	completed := applyEvents(t, store, startTask(t, store), task.TurnCompleted)
	// 进入 DELIVERY_UNCERTAIN 需要回复队列配合，这里绕过 API 直接改状态。
	uncertain := startTask(t, store)
	if _, err := store.db.ExecContext(t.Context(), "UPDATE tasks SET state = 'DELIVERY_UNCERTAIN' WHERE id = ?", uncertain.ID); err != nil {
		t.Fatalf("设置任务状态失败: %v", err)
	}
	uncertain = mustGetTask(t, store, uncertain.ID)
	events := countRows(t, store.db, "task_events")

	tests := []struct {
		current Task
		event   task.Event
	}{
		{created, task.Start},
		{completed, task.ReplyDispatched},
		{running, task.DeliveryUnknown},
		{uncertain, task.DeliveryConfirmed},
		{running, "bogus"},
	}
	for _, tt := range tests {
		_, err := store.ApplyTaskEvent(t.Context(), tt.current.ID, tt.current.Version, tt.event)
		if !errors.Is(err, task.ErrInvalidTransition) || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ApplyTaskEvent(%s) 错误 = %v; want 同时包装 task.ErrInvalidTransition 与 ErrInvalidArgument", tt.event, err)
		}
		requireUnchanged(t, store, tt.current, events)
	}
	if _, err := store.ApplyTaskEvent(t.Context(), created.ID, created.Version, task.TurnCompleted); !errors.Is(err, task.ErrInvalidTransition) ||
		errors.Is(err, ErrInvalidArgument) {
		t.Errorf("CREATED 上的 turn_completed: 错误 = %v; want 只包装 task.ErrInvalidTransition", err)
	}
}

// TestApplyTaskEventErrors 验证任务不存在、版本过期与非法转移的错误，失败时数据库不变；
// 版本过期与非法转移同时出现时报告版本冲突。
func TestApplyTaskEventErrors(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	if _, err := store.ApplyTaskEvent(t.Context(), "missing", 1, task.Close); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务不存在: 错误 = %v; want ErrNotFound", err)
	}
	running := startTask(t, store)
	for _, version := range []int64{running.Version - 1, running.Version + 1} {
		if _, err := store.ApplyTaskEvent(t.Context(), running.ID, version, task.TurnCompleted); !errors.Is(err, ErrVersionConflict) {
			t.Errorf("版本 %d: 错误 = %v; want ErrVersionConflict", version, err)
		}
	}
	if _, err := store.ApplyTaskEvent(t.Context(), running.ID, running.Version, task.ApprovalResolved); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("非法转移: 错误 = %v; want task.ErrInvalidTransition", err)
	}
	if _, err := store.ApplyTaskEvent(t.Context(), running.ID, running.Version-1, task.ApprovalResolved); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("过期版本且非法转移: 错误 = %v; want ErrVersionConflict", err)
	}
	requireUnchanged(t, store, running, 1)
}

// TestTaskEventsRollBackOnFailure 用测试内创建的触发器让 StartTask 与 ApplyTaskEvent（普通事件、close、fail）中的某一步写入失败，
// 或让带版本条件的任务更新不影响任何行，验证方法返回错误且整个事务回滚：任务、回复与事件记录都保持操作前的样子。
// 普通事件、close 与 fail 的场景都带有两条排队回复；只有 close 与 fail 会拒绝排队回复，因此只对它们注入回复更新失败。
func TestTaskEventsRollBackOnFailure(t *testing.T) {
	faults := []struct {
		name      string
		trigger   string
		want      string
		onReplies bool
	}{
		{"任务更新失败", "BEFORE UPDATE ON tasks BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom", false},
		{"任务更新未命中", "BEFORE UPDATE ON tasks BEGIN SELECT RAISE(IGNORE); END", "changed during update", false},
		{"事件写入失败", "BEFORE INSERT ON task_events BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom", false},
		{"回复拒绝失败", "BEFORE UPDATE ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom", true},
	}
	// applyEvent 返回的准备函数启动任务、记录两条排队回复，再给出对该任务执行 event 的操作。
	applyEvent := func(event task.Event) func(t *testing.T, s *Store) (string, func() error) {
		return func(t *testing.T, s *Store) (string, func() error) {
			running := startTask(t, s)
			enqueueReplies(t, s, running.ID, 2)
			return running.ID, func() error {
				_, err := s.ApplyTaskEvent(t.Context(), running.ID, running.Version, event)
				return err
			}
		}
	}
	// 每个操作的准备函数构造场景，返回任务 ID 与待执行的操作；rejects 表示该操作会拒绝排队回复。
	operations := []struct {
		name    string
		rejects bool
		prepare func(t *testing.T, s *Store) (string, func() error)
	}{
		{"StartTask", false, func(t *testing.T, s *Store) (string, func() error) {
			created := createTask(t, s)
			return created.ID, func() error {
				_, err := s.StartTask(t.Context(), created.ID, created.Version, "thread-synthetic-0001")
				return err
			}
		}},
		{"ApplyTaskEvent(turn_completed)", false, applyEvent(task.TurnCompleted)},
		{"ApplyTaskEvent(close)", true, applyEvent(task.Close)},
		{"ApplyTaskEvent(fail)", true, applyEvent(task.Fail)},
	}
	for _, op := range operations {
		for _, fault := range faults {
			if fault.onReplies && !op.rejects {
				continue
			}
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

// TestTaskEvents 验证 CREATED→RUNNING→COMPLETED→CLOSED 依次记录事件、前后状态与时间，按 seq 升序，
// 且只返回指定任务的记录：另一任务的事件夹在中间，使 seq 不连续。任务不存在时返回 ErrNotFound。
func TestTaskEvents(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	first := createTask(t, store)
	second := createTask(t, store)
	// tick 把时钟推进 1 秒并返回存储应记录的 UTC 时间。
	tick := func() time.Time {
		*clock = clock.Add(time.Second)
		return clock.UTC()
	}

	startedAt := tick()
	current, err := store.StartTask(t.Context(), first.ID, first.Version, "thread-synthetic-0001")
	if err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	tick()
	if _, err := store.StartTask(t.Context(), second.ID, second.Version, "thread-synthetic-0002"); err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	completedAt := tick()
	current = applyEvents(t, store, current, task.TurnCompleted)
	closedAt := tick()
	applyEvents(t, store, current, task.Close)

	want := []TaskEvent{
		{Seq: 1, TaskID: first.ID, Event: task.Start, From: task.Created, To: task.Running, At: startedAt},
		{Seq: 3, TaskID: first.ID, Event: task.TurnCompleted, From: task.Running, To: task.Completed, At: completedAt},
		{Seq: 4, TaskID: first.ID, Event: task.Close, From: task.Completed, To: task.Closed, At: closedAt},
	}
	got, err := store.TaskEvents(t.Context(), first.ID)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("TaskEvents = %+v, %v; want %+v, nil", got, err, want)
	}
	if _, err := store.TaskEvents(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务不存在: 错误 = %v; want ErrNotFound", err)
	}
}

// TestTaskEventsRejectsUnknownState 验证事件记录中的前后状态不在已知集合时返回错误，而不是静默接受。
func TestTaskEventsRejectsUnknownState(t *testing.T) {
	for _, states := range [][2]string{{"BOGUS", "RUNNING"}, {"CREATED", "BOGUS"}} {
		store, _ := openTaskStore(t, nil)
		created := createTask(t, store)
		_, err := store.db.ExecContext(t.Context(),
			"INSERT INTO task_events (task_id, event, from_state, to_state, created_at) VALUES (?, 'start', ?, ?, 0)",
			created.ID, states[0], states[1])
		if err != nil {
			t.Fatalf("插入事件失败: %v", err)
		}
		if events, err := store.TaskEvents(t.Context(), created.ID); err == nil || !strings.Contains(err.Error(), "BOGUS") {
			t.Errorf("%v: TaskEvents = %+v, %v; want 未知状态错误", states, events, err)
		}
	}
}

// TestMailPause 验证 D8 的持久暂停：从未暂停时 MailPaused 为 false 与零值，ResumeMail 不是错误也不写入；PauseMail 写入原因与
// paused_at，正在暂停时再次 PauseMail 不改动（原因与 paused_at 保持第一次的值）；ResumeMail 记下 resumed_at，此后 MailPaused 返回
// false 与解除时刻，再次 ResumeMail 不改动；解除之后再次 PauseMail 写入新的原因与 paused_at 并清空 resumed_at；时钟回拨到 paused_at
// 之前时 ResumeMail 以 paused_at 作为解除时刻，满足表约束；每个任务至多一行，任务之间互不影响；任务不存在时 PauseMail 返回
// ErrNotFound（不是 ErrInvalidArgument）且不写入。
func TestMailPause(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	ctx := t.Context()
	current := startTask(t, store)
	other := startTask(t, store)
	// ms 把时刻格式化为 mail_pauses 中保存的 UTC 毫秒文本。
	ms := func(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }
	// requirePaused 断言 MailPaused(current) 返回 paused 与 resumedAt，且 mail_pauses 中该任务的行（原因|paused_at|resumed_at）为 row。
	requirePaused := func(step string, paused bool, resumedAt time.Time, row ...string) {
		t.Helper()
		gotPaused, gotResumed, err := store.MailPaused(ctx, current.ID)
		if err != nil || gotPaused != paused || !gotResumed.Equal(resumedAt) {
			t.Errorf("%s: MailPaused = %t, %v, %v; want %t, %v", step, gotPaused, gotResumed, err, paused, resumedAt)
		}
		if got := dumpRows(t, store.db, "SELECT reason, paused_at, resumed_at FROM mail_pauses WHERE task_id = ?", current.ID); !slices.Equal(got, row) {
			t.Errorf("%s: mail_pauses 行 = %q; want %q", step, got, row)
		}
	}
	// mustRun 执行暂停或解除，失败时终止测试。
	mustRun := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s 返回错误: %v", name, err)
		}
	}

	requirePaused("从未暂停", false, time.Time{})
	mustRun("从未暂停时 ResumeMail", store.ResumeMail(ctx, current.ID))
	requirePaused("从未暂停时解除", false, time.Time{})

	pausedAt := clock.UTC()
	mustRun("PauseMail(hourly)", store.PauseMail(ctx, current.ID, PauseHourly))
	requirePaused("暂停", true, time.Time{}, "hourly|"+ms(pausedAt)+"|<nil>")
	*clock = clock.Add(time.Minute)
	mustRun("再次 PauseMail(daily)", store.PauseMail(ctx, current.ID, PauseDaily))
	requirePaused("正在暂停时再次暂停", true, time.Time{}, "hourly|"+ms(pausedAt)+"|<nil>")

	*clock = clock.Add(time.Minute)
	resumedAt := clock.UTC()
	mustRun("ResumeMail", store.ResumeMail(ctx, current.ID))
	requirePaused("解除", false, resumedAt, "hourly|"+ms(pausedAt)+"|"+ms(resumedAt))
	*clock = clock.Add(time.Minute)
	mustRun("再次 ResumeMail", store.ResumeMail(ctx, current.ID))
	requirePaused("没有暂停时再次解除", false, resumedAt, "hourly|"+ms(pausedAt)+"|"+ms(resumedAt))

	*clock = clock.Add(time.Minute)
	repausedAt := clock.UTC()
	mustRun("解除后 PauseMail(daily)", store.PauseMail(ctx, current.ID, PauseDaily))
	requirePaused("解除之后再次暂停", true, time.Time{}, "daily|"+ms(repausedAt)+"|<nil>")

	*clock = repausedAt.Add(-time.Hour)
	mustRun("时钟回拨后 ResumeMail", store.ResumeMail(ctx, current.ID))
	requirePaused("时钟回拨后解除", false, repausedAt, "daily|"+ms(repausedAt)+"|"+ms(repausedAt))

	if paused, at, err := store.MailPaused(ctx, other.ID); err != nil || paused || !at.IsZero() {
		t.Errorf("另一任务 MailPaused = %t, %v, %v; want false、零值", paused, at, err)
	}
	if err := store.PauseMail(ctx, "0000000000", PauseHourly); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidArgument) {
		t.Errorf("任务不存在: PauseMail = %v; want ErrNotFound", err)
	}
	if got := countRows(t, store.db, "mail_pauses"); got != 1 {
		t.Errorf("mail_pauses 行数 = %d; want 1", got)
	}
}

// TestNewInterfacesRejectInvalidArguments 验证 4b 新增接口的每项输入校验：不合法的任务 ID（空、9 个字符、含 u、含大写字母、
// 形似地址）、Message-ID（invalidMessageIDs 中的每一个）、账户与文件夹（按 checkMailbox 的规则）、为 0 的 UIDVALIDITY 与 UID、
// 越界的 limit 与未知的暂停原因都返回包装 ErrInvalidArgument 的错误；上下文已取消时同样如此，证明校验先于访问数据库；
// 错误文本不回显取值，校验失败不写入任何行。
func TestNewInterfacesRejectInvalidArguments(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	running := startTask(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	const validMessageID = "<a@example.invalid>"
	// call 是一次应当被校验拒绝的调用；value 是其中不合法的取值，用来断言错误文本不回显它。
	type call struct {
		name  string
		value string
		run   func() error
	}
	var calls []call
	for _, id := range []string{"", "000000000", "000000000u", "ABCDEFGHJK", "attacker@example.invalid"} {
		calls = append(calls,
			call{"LatestAttemptedNotification", id, func() error { _, err := store.LatestAttemptedNotification(ctx, id, time.Time{}); return err }},
			call{"LatestSentNotification", id, func() error { _, err := store.LatestSentNotification(ctx, id); return err }},
			call{"ThreadReferences", id, func() error { _, err := store.ThreadReferences(ctx, id, 100, 20); return err }},
			call{"CountAcceptedRepliesSince", id, func() error { _, err := store.CountAcceptedRepliesSince(ctx, id, time.Time{}); return err }},
			call{"PauseMail", id, func() error { return store.PauseMail(ctx, id, PauseHourly) }},
			call{"MailPaused", id, func() error { _, _, err := store.MailPaused(ctx, id); return err }},
			call{"ResumeMail", id, func() error { return store.ResumeMail(ctx, id) }},
		)
	}
	for _, id := range invalidMessageIDs {
		calls = append(calls,
			call{"NotificationByMessageID", id, func() error { _, err := store.NotificationByMessageID(ctx, id); return err }},
			call{"NotificationByDeliveredID", id, func() error { _, err := store.NotificationByDeliveredID(ctx, id); return err }},
			call{"ResolveUncertainAsDelivered", id, func() error { _, err := store.ResolveUncertainAsDelivered(ctx, 1, id); return err }},
			call{"InboundByMessageID", id, func() error { _, err := store.InboundByMessageID(ctx, botAccount, id); return err }},
			call{"RejectedBeforeReset", id, func() error { _, err := store.RejectedBeforeReset(ctx, botAccount, "INBOX", 7, id); return err }},
		)
	}
	for _, account := range []string{"", "a@", strings.Repeat("a", 243) + "@example.com", "bot @example.invalid", "bot\x00@example.invalid", "bot\xff@example.invalid"} {
		calls = append(calls,
			call{"InboundByMessageID", account, func() error { _, err := store.InboundByMessageID(ctx, account, validMessageID); return err }},
			call{"RejectionExists", account, func() error { _, err := store.RejectionExists(ctx, account, "INBOX", 7, 1); return err }},
			call{"RejectedBeforeReset", account, func() error {
				_, err := store.RejectedBeforeReset(ctx, account, "INBOX", 7, validMessageID)
				return err
			}},
		)
	}
	for _, folder := range []string{"", strings.Repeat("f", 256), "IN\x00BOX", "IN\xffBOX"} {
		calls = append(calls,
			call{"RejectionExists", folder, func() error { _, err := store.RejectionExists(ctx, botAccount, folder, 7, 1); return err }},
			call{"RejectedBeforeReset", folder, func() error {
				_, err := store.RejectedBeforeReset(ctx, botAccount, folder, 7, validMessageID)
				return err
			}},
		)
	}
	calls = append(calls,
		call{"RejectionExists（UIDVALIDITY 为 0）", "", func() error { _, err := store.RejectionExists(ctx, botAccount, "INBOX", 0, 1); return err }},
		call{"RejectionExists（UID 为 0）", "", func() error { _, err := store.RejectionExists(ctx, botAccount, "INBOX", 7, 0); return err }},
		call{"RejectedBeforeReset（UIDVALIDITY 为 0）", "", func() error {
			_, err := store.RejectedBeforeReset(ctx, botAccount, "INBOX", 0, validMessageID)
			return err
		}},
		call{"ThreadReferences（limit 0）", "", func() error { _, err := store.ThreadReferences(ctx, running.ID, 100, 0); return err }},
		call{"ThreadReferences（limit -1）", "", func() error { _, err := store.ThreadReferences(ctx, running.ID, 100, -1); return err }},
		call{"ThreadReferences（limit 21）", "", func() error { _, err := store.ThreadReferences(ctx, running.ID, 100, 21); return err }},
		call{"PauseMail（原因为 weekly）", "weekly", func() error { return store.PauseMail(ctx, running.ID, "weekly") }},
		call{"PauseMail（原因为空）", "", func() error { return store.PauseMail(ctx, running.ID, "") }},
		call{"PauseMail（原因为 HOURLY）", "HOURLY", func() error { return store.PauseMail(ctx, running.ID, "HOURLY") }},
	)
	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, ErrInvalidArgument) || errors.Is(err, context.Canceled) {
			t.Errorf("%s(%q): err = %v; want 包装 ErrInvalidArgument 的校验错误", c.name, c.value, err)
			continue
		}
		if len(c.value) >= 3 && strings.Contains(err.Error(), c.value) {
			t.Errorf("%s: 错误文本 %q 回显了取值 %q", c.name, err, c.value)
		}
	}
	for _, table := range []string{"mail_pauses", "notifications"} {
		if got := countRows(t, store.db, table); got != 0 {
			t.Errorf("%s 行数 = %d; want 0", table, got)
		}
	}
}
