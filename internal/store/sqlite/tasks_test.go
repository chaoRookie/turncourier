// Package sqlite 的任务持久化测试用临时目录中的真实 SQLite 数据库验证创建、启动、事件、版本冲突与事件记录。
package sqlite

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/task"
)

// openTaskStore 打开使用可调时钟与给定随机源的存储；random 为 nil 时使用 crypto/rand。
// 返回的指针指向注入时钟的当前读数，测试修改它来推进时间；时钟位于 UTC+8，用来验证存储统一换算为 UTC。
func openTaskStore(t *testing.T, random io.Reader) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 18, 16, 30, 0, 123_000_000, time.FixedZone("UTC+8", 8*60*60))
	store, err := Open(t.Context(), dataDir(t), Options{Now: func() time.Time { return now }, Random: random})
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	t.Cleanup(func() { store.Close() })
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

// TestCreateTaskRejectsUnknownAgent 验证未知或大小写不符的 Agent 类型被拒绝，且不写入任何任务。
func TestCreateTaskRejectsUnknownAgent(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	for _, agent := range []Agent{"", "gemini", "Codex", "claude "} {
		if got, err := store.CreateTask(t.Context(), agent); err == nil {
			t.Errorf("CreateTask(%q) = %+v, nil; want 错误", agent, got)
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

// TestStartTaskSessionID 验证会话 ID 须为 1–200 个字母、数字或 . _ : -，否则返回 ErrInvalidSessionID 且不改动数据库；
// 恰好 200 个字符且包含每类允许字符的会话 ID 可以写入。
func TestStartTaskSessionID(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	created := createTask(t, store)
	for _, sessionID := range []string{"", strings.Repeat("a", 201), "thread 1", "thread-1\n", "thread/1", "会话-1"} {
		if _, err := store.StartTask(t.Context(), created.ID, created.Version, sessionID); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("StartTask(%q) 错误 = %v; want ErrInvalidSessionID", sessionID, err)
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
// 返回 task.ErrInvalidTransition 且不改动数据库。每个保留事件都在状态机本会接受它的状态下调用，
// 证明拒绝来自 ApplyTaskEvent 本身，而不是状态机。
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
		if !errors.Is(err, task.ErrInvalidTransition) {
			t.Errorf("ApplyTaskEvent(%s) 错误 = %v; want task.ErrInvalidTransition", tt.event, err)
		}
		requireUnchanged(t, store, tt.current, events)
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
