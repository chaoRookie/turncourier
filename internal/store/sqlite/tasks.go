// Package sqlite 持久化任务与任务事件；状态变化只由 task 状态机决定，写入以版本号做乐观并发控制。任务的邮件触发可被持久地暂停（D8）。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/task"
)

// Agent 是任务使用的 Agent 类型。
type Agent string

const (
	AgentCodex  Agent = "codex"
	AgentClaude Agent = "claude"
)

// Task 是任务的持久化快照；Version 用于乐观并发控制。
type Task struct {
	ID        string
	Owner     string
	Agent     Agent
	SessionID string
	State     task.State
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskEvent 是一条只含元数据的任务状态变化记录。
type TaskEvent struct {
	Seq    int64
	TaskID string
	Event  task.Event
	From   task.State
	To     task.State
	At     time.Time
}

var (
	// ErrNotFound 表示指定的任务、回复、通知、入站记录、收取游标、实例 ID 或密钥元数据不存在。
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict 表示调用方持有的任务版本已过期，需要重新读取后再决定。
	ErrVersionConflict = errors.New("task version conflict")
	// ErrInvalidArgument 表示输入不符合存储的校验规则（任务 ID、Message-ID、账户、文件夹、UIDVALIDITY、limit、原因等的形式或取值范围）：
	// 重试也不会成功，也与数据库故障无关。全部接口的输入校验错误都包装它，调用方据此把校验失败与数据库故障区分开，例如以来信内容为参数
	// 的查询返回它时按找不到处理，而其他存储错误都是基础设施错误。除 CreateNotification 对任务 owner 的检查外，校验都在查询或开始事务
	// 之前进行；错误文本不回显 Message-ID、地址、账户与文件夹。它不叫 ErrInvalidInput：payload 包已有同名哨兵，调用方两者都要处理。
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrInvalidSessionID 表示 Agent 会话 ID 为空、过长或含不允许的字符；它是输入校验错误，同时满足 errors.Is(err, ErrInvalidArgument)。
	ErrInvalidSessionID error = &argumentError{err: errors.New("invalid agent session id")}
)

// argumentError 是一条输入校验错误：Error 返回原文，Unwrap 同时给出 ErrInvalidArgument 与原错误。调用方因此能按 ErrInvalidArgument
// 区分校验失败与数据库故障，而原有的错误文本与原错误所包装的哨兵（例如 task.ErrInvalidTransition）都保持不变。
type argumentError struct {
	err error
}

// Error 返回校验错误的原文。
func (e *argumentError) Error() string { return e.err.Error() }

// Unwrap 返回 ErrInvalidArgument 与原错误，供 errors.Is 与 errors.As 逐一匹配。
func (e *argumentError) Unwrap() []error { return []error{ErrInvalidArgument, e.err} }

// invalidArgument 按 fmt.Errorf 的规则构造一条输入校验错误：文本与 fmt.Errorf(format, args...) 相同，errors.Is 另外匹配
// ErrInvalidArgument，format 中的 %w 照常包装原错误。format 只来自本包的常量，args 不得含 Message-ID、地址、账户与文件夹。
func invalidArgument(format string, args ...any) error {
	return &argumentError{err: fmt.Errorf(format, args...)}
}

// checkTaskID 检查任务 ID 是否为 10 位小写 Crockford base32（taskIDPattern）；不合法时返回输入校验错误，文本不回显取值——
// 以来信内容为参数时，它可能是任意文本。
func checkTaskID(id string) error {
	if !taskIDPattern.MatchString(id) {
		return invalidArgument("task id must be 10 lowercase Crockford base32 characters")
	}
	return nil
}

// maxIDAttempts 是创建任务时生成 ID 的总尝试次数：首次生成加上主键冲突后的两次重试。
const maxIDAttempts = 3

// sessionIDPattern 是 Agent 会话 ID 的合法形式：1–200 个字母、数字或 . _ : -；Go 正则的 $ 只匹配文本末尾，末尾换行同样被拒绝。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// manualEvents 是 ApplyTaskEvent 接受的事件。start 须与会话 ID 一起写入，reply_dispatched、delivery_unknown、
// delivery_confirmed 须与回复队列的变化在同一事务中完成，只能经对应的存储操作执行。
var manualEvents = map[task.Event]bool{
	task.TurnCompleted:     true,
	task.InputRequested:    true,
	task.ApprovalRequested: true,
	task.ApprovalResolved:  true,
	task.Fail:              true,
	task.Close:             true,
}

// rejectReasons 列出会让任务的排队回复一并被拒绝的事件，以及记录在回复上的拒绝原因。
var rejectReasons = map[task.Event]string{
	task.Fail:  "task_failed",
	task.Close: "task_closed",
}

// rowQuerier 是 *sql.DB 与 *sql.Tx 共有的单行查询方法，使任务读取在事务内外都能复用。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CreateTask 以 CREATED 状态创建任务；ID 主键冲突时换新 ID 重试，连同首次生成最多尝试 3 次，
// 连续 3 次冲突即报错。Agent 类型未知时在开始事务前返回包装 ErrInvalidArgument 的错误。
func (s *Store) CreateTask(ctx context.Context, agent Agent) (Task, error) {
	if agent != AgentCodex && agent != AgentClaude {
		return Task{}, invalidArgument("unknown agent %q", agent)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	now := s.now().UnixMilli()
	for range maxIDAttempts {
		id, err := newTaskID(s.random)
		if err != nil {
			return Task{}, err
		}
		// 只有主键冲突被 DO NOTHING 吸收，表现为受影响行数为 0；其他约束违反仍然报错。
		result, err := tx.ExecContext(ctx,
			"INSERT INTO tasks (id, agent, state, version, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?) ON CONFLICT (id) DO NOTHING",
			id, string(agent), string(task.Created), now, now)
		if err != nil {
			return Task{}, fmt.Errorf("cannot create task: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return Task{}, fmt.Errorf("cannot create task: %w", err)
		}
		if inserted == 0 {
			continue
		}
		created, err := getTask(ctx, tx, id)
		if err != nil {
			return Task{}, err
		}
		if err := tx.Commit(); err != nil {
			return Task{}, fmt.Errorf("cannot commit task: %w", err)
		}
		return created, nil
	}
	return Task{}, fmt.Errorf("cannot allocate a unique task id after %d attempts", maxIDAttempts)
}

// GetTask 按 ID 读取任务。
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return getTask(ctx, s.db, id)
}

// StartTask 记录 Agent 会话 ID 并执行 start 事件；会话 ID 一经写入不可修改。会话 ID 不合法时在开始事务前返回 ErrInvalidSessionID
// （它同时满足 errors.Is(err, ErrInvalidArgument)）。
func (s *Store) StartTask(ctx context.Context, id string, version int64, sessionID string) (Task, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return Task{}, ErrInvalidSessionID
	}
	return s.applyEvent(ctx, id, version, task.Start, sessionID)
}

// ApplyTaskEvent 执行 turn_completed、input_requested、approval_requested、approval_resolved、fail、close 之一。
// start、reply_dispatched、delivery_unknown、delivery_confirmed 必须由对应的存储操作原子完成，这里与未知事件一起直接拒绝：
// 错误同时包装 task.ErrInvalidTransition 与 ErrInvalidArgument（输入校验，先于开始事务）。
// fail 与 close 在同一事务中把该任务所有 QUEUED 回复改为 REJECTED（原因分别为 task_failed、task_closed），提交后执行检查点。
// close 还在同一事务中把该任务的 PENDING 通知改为 ABANDONED(task_closed)；SENDING 与 UNCERTAIN 通知的结果尚未确定，保持不变，
// 之后确认未投递时由 RequeueNotification 或 ResolveUncertainNotification 改为 ABANDONED(task_closed)。fail 不影响通知。
func (s *Store) ApplyTaskEvent(ctx context.Context, id string, version int64, event task.Event) (Task, error) {
	if !manualEvents[event] {
		return Task{}, invalidArgument("%w: event %q must be applied by its dedicated store operation", task.ErrInvalidTransition, event)
	}
	return s.applyEvent(ctx, id, version, event, "")
}

// applyEvent 在一个事务中读取任务、比较版本、用 task.Next 计算新状态，再以版本条件更新任务并追加事件记录。
// 版本比较先于状态机校验：持有过期版本的调用方看到的状态已不可信，应得到 ErrVersionConflict 后重新读取。
// sessionID 只由 start 传入。
func (s *Store) applyEvent(ctx context.Context, id string, version int64, event task.Event, sessionID string) (Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getTask(ctx, tx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Version != version {
		return Task{}, fmt.Errorf("%w: task %q is at version %d, not %d", ErrVersionConflict, id, current.Version, version)
	}
	next, err := task.Next(current.State, event)
	if err != nil {
		return Task{}, err
	}
	now := s.now().UnixMilli()
	if err := writeTaskTransition(ctx, tx, current, event, next, sessionID, now); err != nil {
		return Task{}, err
	}
	if reason, ok := rejectReasons[event]; ok {
		rejected, err := queue.Next(queue.Queued, queue.Reject)
		if err != nil {
			return Task{}, err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE replies SET state = ?, reject_reason = ?, updated_at = ? WHERE task_id = ? AND state = ?",
			string(rejected), reason, now, id, string(queue.Queued)); err != nil {
			return Task{}, fmt.Errorf("cannot reject queued replies: %w", err)
		}
	}
	if event == task.Close {
		if _, err := abandonPending(ctx, tx, abandonTaskClosed, now, "task_id = ?", id); err != nil {
			return Task{}, err
		}
	}
	updated, err := getTask(ctx, tx, id)
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("cannot commit task event: %w", err)
	}
	// 被拒绝的排队回复与被放弃的通知，其正文已由触发器在同一事务中删除。
	if _, ok := rejectReasons[event]; ok {
		truncateWAL(ctx, s.db)
	}
	return updated, nil
}

// writeTaskTransition 在事务 tx 中以读取时的版本为条件把任务更新为 next，并追加一条 event 事件记录；
// 新状态由调用方经 task 状态机算出。sessionID 为空时不改动会话 ID，coalesce 保留已写入的值，使其一经写入不可修改。
// IMMEDIATE 事务已在读取前取得写锁，版本条件是防止并发覆盖的最后一道保证，受影响行数不为 1 时返回 ErrVersionConflict。
func writeTaskTransition(ctx context.Context, tx *sql.Tx, current Task, event task.Event, next task.State, sessionID string, now int64) error {
	result, err := tx.ExecContext(ctx,
		"UPDATE tasks SET state = ?, version = version + 1, updated_at = ?, session_id = coalesce(session_id, ?) WHERE id = ? AND version = ?",
		string(next), now, sql.NullString{String: sessionID, Valid: sessionID != ""}, current.ID, current.Version)
	if err != nil {
		return fmt.Errorf("cannot update task: %w", err)
	}
	if updated, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("cannot update task: %w", err)
	} else if updated != 1 {
		return fmt.Errorf("%w: task %q changed during update", ErrVersionConflict, current.ID)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO task_events (task_id, event, from_state, to_state, created_at) VALUES (?, ?, ?, ?, ?)",
		current.ID, string(event), string(current.State), string(next), now); err != nil {
		return fmt.Errorf("cannot record task event: %w", err)
	}
	return nil
}

// TaskEvents 按发生顺序返回任务的状态变化记录。
// 任务不存在时返回 ErrNotFound；记录中的状态名不在已知集合时返回错误，不静默接受。
func (s *Store) TaskEvents(ctx context.Context, id string) ([]TaskEvent, error) {
	if _, err := getTask(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, task_id, event, from_state, to_state, created_at FROM task_events WHERE task_id = ? ORDER BY seq", id)
	if err != nil {
		return nil, fmt.Errorf("cannot read task events: %w", err)
	}
	defer rows.Close()
	var events []TaskEvent
	for rows.Next() {
		var event TaskEvent
		var at int64
		if err := rows.Scan(&event.Seq, &event.TaskID, &event.Event, &event.From, &event.To, &at); err != nil {
			return nil, fmt.Errorf("cannot read task events: %w", err)
		}
		if !event.From.Valid() || !event.To.Valid() {
			return nil, fmt.Errorf("task event %d has unknown state %q -> %q", event.Seq, event.From, event.To)
		}
		event.At = time.UnixMilli(at).UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read task events: %w", err)
	}
	return events, nil
}

// getTask 通过 q 读取任务快照，时间换算为 UTC；任务不存在时返回 ErrNotFound，
// 状态名不在已知集合时返回错误而不是静默接受。
func getTask(ctx context.Context, q rowQuerier, id string) (Task, error) {
	var t Task
	var sessionID sql.NullString
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx,
		"SELECT id, owner, agent, session_id, state, version, created_at, updated_at FROM tasks WHERE id = ?", id).
		Scan(&t.ID, &t.Owner, &t.Agent, &sessionID, &t.State, &t.Version, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("cannot read task: %w", err)
	}
	if !t.State.Valid() {
		return Task{}, fmt.Errorf("task %q has unknown state %q", id, t.State)
	}
	t.SessionID = sessionID.String
	t.CreatedAt = time.UnixMilli(createdAt).UTC()
	t.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return t, nil
}

// TasksWithQueuedReplies 返回有 QUEUED 回复、且任务状态可以派发（task.CanDispatchReply：COMPLETED 或 WAITING_INPUT）的任务 ID，
// 按各自最早 QUEUED 回复的序号升序，供派发循环逐个调用 ClaimNextReply；没有时返回空。可否派发由 task 状态机判定，不在 SQL 中
// 另写一份规则；任务状态不在已知集合时返回错误而不是静默跳过。结果集在一个查询中读完，期间不发出其他查询：存储只有一个连接。
func (s *Store) TasksWithQueuedReplies(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT r.task_id, t.state FROM replies r JOIN tasks t ON t.id = r.task_id WHERE r.state = ? GROUP BY r.task_id, t.state ORDER BY min(r.seq)",
		string(queue.Queued))
	if err != nil {
		return nil, fmt.Errorf("cannot read replies: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		var state task.State
		if err := rows.Scan(&id, &state); err != nil {
			return nil, fmt.Errorf("cannot read replies: %w", err)
		}
		if !state.Valid() {
			return nil, fmt.Errorf("task %q has unknown state %q", id, state)
		}
		if task.CanDispatchReply(state) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read replies: %w", err)
	}
	return ids, nil
}

// PauseReason 是任务的邮件触发被暂停的原因（D8 的回环刹车）：同一任务在滚动 1 小时或 24 小时内接受的回复达到上限。
type PauseReason string

const (
	PauseHourly PauseReason = "hourly"
	PauseDaily  PauseReason = "daily"
)

// PauseMail 按 D8 持久地暂停任务的邮件触发，每个任务至多一行：没有记录或已解除时写入原因、以当前时间为 paused_at 并清空 resumed_at；
// 正在暂停时不改动（原因与 paused_at 保持第一次暂停时的值）。任务 ID 或原因不合法时在开始事务前返回包装 ErrInvalidArgument 的错误；
// 任务不存在时返回 ErrNotFound。暂停只由本地的 ResumeMail 解除，从不到点自动恢复：环一旦出现就说明分类漏判，自动恢复只会让它再转起来。
func (s *Store) PauseMail(ctx context.Context, taskID string, reason PauseReason) error {
	if err := checkTaskID(taskID); err != nil {
		return fmt.Errorf("invalid mail pause: %w", err)
	}
	if reason != PauseHourly && reason != PauseDaily {
		return invalidArgument("invalid mail pause: reason must be hourly or daily")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	if _, err := getTask(ctx, tx, taskID); err != nil {
		return err
	}
	// 冲突时只有已解除（resumed_at 不为空）才改写；正在暂停时 WHERE 不成立，原行保持不变。
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO mail_pauses (task_id, reason, paused_at) VALUES (?, ?, ?) "+
			"ON CONFLICT (task_id) DO UPDATE SET reason = excluded.reason, paused_at = excluded.paused_at, resumed_at = NULL "+
			"WHERE mail_pauses.resumed_at IS NOT NULL",
		taskID, string(reason), s.now().UnixMilli()); err != nil {
		return fmt.Errorf("cannot pause mail: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cannot commit mail pause: %w", err)
	}
	return nil
}

// MailPaused 返回任务的邮件触发是否正在暂停，以及最近一次解除的时刻：从未解除或正在暂停时为零值（解除之后再次暂停会清空它）。
// 任务没有暂停记录（包括任务不存在）时返回 false 与零值。回环刹车只计解除时刻之后接受的回复（CountAcceptedRepliesSince），
// 否则同一窗口里的旧回复会让它立刻再次暂停。任务 ID 不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) MailPaused(ctx context.Context, taskID string) (paused bool, resumedAt time.Time, err error) {
	if err := checkTaskID(taskID); err != nil {
		return false, time.Time{}, fmt.Errorf("invalid mail pause query: %w", err)
	}
	var resumed sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT resumed_at FROM mail_pauses WHERE task_id = ?", taskID).Scan(&resumed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("cannot read mail pause: %w", err)
	}
	if !resumed.Valid {
		return true, time.Time{}, nil
	}
	return false, time.UnixMilli(resumed.Int64).UTC(), nil
}

// ResumeMail 解除任务的邮件暂停：正在暂停时以当前时间记下 resumed_at，时钟回拨到 paused_at 之前时取 paused_at，使表约束
// resumed_at >= paused_at 成立；没有暂停（从未暂停或已解除）时不改动，也不是错误。任务 ID 不合法时在开始前返回包装
// ErrInvalidArgument 的错误。4b 只提供存储接口，解除用的本地命令随常驻命令在后续阶段提供。
func (s *Store) ResumeMail(ctx context.Context, taskID string) error {
	if err := checkTaskID(taskID); err != nil {
		return fmt.Errorf("invalid mail resume: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE mail_pauses SET resumed_at = max(?, paused_at) WHERE task_id = ? AND resumed_at IS NULL",
		s.now().UnixMilli(), taskID); err != nil {
		return fmt.Errorf("cannot resume mail: %w", err)
	}
	return nil
}
