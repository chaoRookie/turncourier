// Package sqlite 在事务中完成入站回复的去重入队、派发、确认与崩溃后的在途回复恢复；待派发的正文只以密文落盘，
// 在分配序号的同一事务中加密、派发时在同一事务中解密，回复进入终态时由触发器删除；从不自动重新派发回复。
package sqlite

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/security/payload"
	"github.com/chaoRookie/turncourier/internal/task"
)

// InboundReply 是已通过发件人、线程与令牌校验的入站回复。Body 只以密文落盘。
type InboundReply struct {
	TaskID      string
	Account     string // 调用方已用 config.NormalizeAddress 规范化的机器人邮箱地址
	Folder      string // 取回该邮件的文件夹，4b 传入 imap.Batch.Folder；1–255 个字符（按字符计，与表约束一致），合法 UTF-8、不含 NUL，可以含空格
	UIDValidity uint32
	UID         uint32
	MessageID   string   // 与邮件头一致的原样字符串
	BodyDigest  [32]byte // token.Key.BodyDigest(Body)，写入 body_sha256 列（原字段名 BodySHA256）
	Body        []byte   // 解析出的新正文，1 字节到 payload.MaxPlaintext，须为合法 UTF-8
}

// Reply 是回复队列项的持久化快照；Seq 即本地入队顺序。
type Reply struct {
	Seq          int64
	TaskID       string
	State        queue.State
	ResumeState  task.State // 仅在派发后记录派发前的任务状态
	RejectReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RecordResult 说明 RecordReply 是新入队还是命中了已有记录。
type RecordResult struct {
	Reply     Reply
	Duplicate bool
}

// InboundRecord 是按 (账户, Message-ID) 找到的已记录入站邮件：所属任务、键控摘要、取回它的文件夹、UIDVALIDITY、UID 与对应回复的序号。
// 验证流水线据此在有效期、令牌新旧、暂停与任务状态之前去重（4b Task 9 第 11 步）。
type InboundRecord struct {
	TaskID      string
	BodyDigest  [32]byte
	Folder      string
	UIDValidity uint32
	UID         uint32
	ReplySeq    int64
}

// ErrMessageConflict 表示同一邮件标识对应了不同内容或不同 Message-ID，必须拒绝并告警。
var ErrMessageConflict = errors.New("inbound message conflict")

// ErrNoDispatchableReply 表示任务当前不可派发、已有在途回复或队列为空。
var ErrNoDispatchableReply = errors.New("no dispatchable reply")

// replyUnsent 是回复确认未送达 Agent、任务经 task.ResumeAfterUnsent 恢复派发前状态时记录的事件名。
// 恢复的目标取决于派发时记下的状态，因此不在 task 状态机的事件表中，也不能经 ApplyTaskEvent 执行。
const replyUnsent task.Event = "reply_unsent"

// eventsSinceDispatch 是在途回复处于各状态、且任务自派发以来没有发生其他事件时，该任务按发生顺序的最后几条事件。
// 回复标记不确定时任务仍为派发后的 RUNNING，才会在 reply_dispatched 之后紧跟 delivery_unknown，因此只看最新一条不够：
// 审批往返后回到 RUNNING 再标记不确定，最新一条同样是 delivery_unknown。
var eventsSinceDispatch = map[queue.State][]string{
	queue.Dispatching: {string(task.ReplyDispatched)},
	queue.Uncertain:   {string(task.ReplyDispatched), string(task.DeliveryUnknown)},
}

// stateRejectReasons 是不接受回复的任务状态对应的拒绝原因；未列出的状态记为 task_not_accepting。
var stateRejectReasons = map[task.State]string{
	task.Failed: "task_failed",
	task.Closed: "task_closed",
}

// knownInbound 是按唯一键找到的已有入站记录中参与去重判定的字段，以及它对应的回复序号。
type knownInbound struct {
	messageID string
	digest    []byte
	taskID    string
	seq       int64
}

// RecordReply 在同一事务中完成去重检查、写入入站记录与写入回复队列项。
// 任务处于 AcceptsReplies 为 true 的状态时入队为 QUEUED；否则记录为 REJECTED，原因为 task_not_accepting、task_failed 或 task_closed。
// 同一账户、同一文件夹中 (UIDVALIDITY, UID) 已有记录，或同一账户中 Message-ID 已有记录时，Message-ID、正文摘要与任务
// 都一致才算重复，返回原回复且不写入；任一不一致返回 ErrMessageConflict。按 Message-ID 的查找不含文件夹，
// 同一封信在 INBOX 与 Junk 各有一份时判为重复。重复邮件不按任务的当前状态重新判定。摘要只做字节比较，存储层不知道令牌密钥，
// 不校验摘要与正文是否对应。
// 开始事务前校验各字段与 Body（不合法时返回包装 ErrInvalidArgument 的错误）；PayloadKey 为空，或事务内发现正文密钥的 kid 未登记为 active 时，返回
// ErrPayloadKeyUnavailable，对重复邮件与将被拒绝的邮件同样如此（Keychain 读取失败时不处理回复）。入队为 QUEUED 时，
// 插入 replies 取得 seq 后用 (KindReply, TaskID, seq) 加密 Body 并插入 reply_payloads；记为 REJECTED 或命中重复时不写正文。
func (s *Store) RecordReply(ctx context.Context, in InboundReply) (RecordResult, error) {
	if err := in.validate(); err != nil {
		return RecordResult{}, err
	}
	if s.payloadKey == nil {
		return RecordResult{}, fmt.Errorf("%w: no payload key was loaded", ErrPayloadKeyUnavailable)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordResult{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getTask(ctx, tx, in.TaskID)
	if err != nil {
		return RecordResult{}, err
	}
	key, err := s.activePayloadKey(ctx, tx)
	if err != nil {
		return RecordResult{}, err
	}
	byUID, err := findInbound(ctx, tx, "account = ? AND folder = ? AND uid_validity = ? AND uid = ?", in.Account, in.Folder, in.UIDValidity, in.UID)
	if err != nil {
		return RecordResult{}, err
	}
	byMessageID, err := findInbound(ctx, tx, "account = ? AND message_id = ?", in.Account, in.MessageID)
	if err != nil {
		return RecordResult{}, err
	}
	for _, known := range []*knownInbound{byUID, byMessageID} {
		if known != nil && (known.messageID != in.MessageID || !bytes.Equal(known.digest, in.BodyDigest[:]) || known.taskID != in.TaskID) {
			return RecordResult{}, fmt.Errorf("%w: uid %d/%d or its message id is already recorded with a different message id, digest or task", ErrMessageConflict, in.UIDValidity, in.UID)
		}
	}
	// Message-ID 在账户内唯一，两个查找都命中且一致时是同一条记录。
	if known := cmp.Or(byUID, byMessageID); known != nil {
		reply, err := getReply(ctx, tx, known.seq)
		return RecordResult{Reply: reply, Duplicate: true}, err
	}

	state := queue.Queued
	var reason sql.NullString
	if !task.AcceptsReplies(current.State) {
		if state, err = queue.Next(queue.Queued, queue.Reject); err != nil {
			return RecordResult{}, err
		}
		reason = sql.NullString{String: cmp.Or(stateRejectReasons[current.State], "task_not_accepting"), Valid: true}
	}
	now := s.now().UnixMilli()
	var inboundID, seq int64
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO inbound_messages (account, folder, uid_validity, uid, message_id, body_sha256, task_id, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		in.Account, in.Folder, in.UIDValidity, in.UID, in.MessageID, in.BodyDigest[:], in.TaskID, now).Scan(&inboundID); err != nil {
		return RecordResult{}, fmt.Errorf("cannot record inbound message: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO replies (inbound_id, task_id, state, reject_reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING seq",
		inboundID, in.TaskID, string(state), reason, now, now).Scan(&seq); err != nil {
		return RecordResult{}, fmt.Errorf("cannot enqueue reply: %w", err)
	}
	if state == queue.Queued {
		sealed, err := key.Seal(payload.KindReply, in.TaskID, seq, in.Body)
		if err != nil {
			return RecordResult{}, fmt.Errorf("cannot seal reply: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO reply_payloads (seq, key_id, sealed) VALUES (?, ?, ?)", seq, key.ID(), sealed); err != nil {
			return RecordResult{}, fmt.Errorf("cannot store reply body: %w", err)
		}
	}
	reply, err := getReply(ctx, tx, seq)
	if err != nil {
		return RecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecordResult{}, fmt.Errorf("cannot commit reply: %w", err)
	}
	return RecordResult{Reply: reply}, nil
}

// validate 在事务开始前检查与表约束对应的长度、空白与取值范围，不合法时返回包装 ErrInvalidArgument 的错误；存储层不导入 config，
// 地址规范化由调用方负责。Account 与 Folder 经 checkMailbox 校验，与游标、被拒来信用同一条规则：同一个账户或文件夹名在这三处的判定
// 必须一致，否则一个文件夹名可以记录回复却推不动它的游标。MessageID 按 ValidMessageID 校验（长度按 Unicode 字符计，与表约束中
// SQLite 的 length() 一致，须为合法 UTF-8 且不含 NUL，理由见 checkMailbox）。
// IMAP 文件夹名可以含空格（例如 Sent Messages），因此 Folder 不按空白拒绝；取值由调用方决定，存储层不限定。
// Body 按字节计长度，须为 1 字节到 payload.MaxPlaintext 的合法 UTF-8；错误文本不含正文、地址与 Message-ID。
func (in InboundReply) validate() error {
	if err := checkMailbox(in.Account, in.Folder); err != nil {
		return fmt.Errorf("invalid inbound reply: %w", err)
	}
	switch {
	case in.UIDValidity == 0 || in.UID == 0:
		return invalidArgument("invalid inbound reply: uid validity and uid must be positive")
	case !ValidMessageID(in.MessageID):
		return invalidArgument("invalid inbound reply: message id must be 3-998 characters of valid UTF-8 without NUL")
	case len(in.Body) == 0 || len(in.Body) > payload.MaxPlaintext || !utf8.Valid(in.Body):
		return invalidArgument("invalid inbound reply: body must be 1 byte to 1 MiB of valid UTF-8")
	}
	return nil
}

// InboundByMessageID 按 (账户, Message-ID) 读取入站记录；查找不含文件夹，与 RecordReply 按 Message-ID 的去重一致（同一封信在 INBOX
// 与 Junk 各有一份时只有一条记录）。不存在时返回 ErrNotFound。账户按 checkAccount、Message-ID 按 ValidMessageID 校验，不合法时在查询前
// 返回包装 ErrInvalidArgument 的错误；错误文本不回显账户与 Message-ID。
func (s *Store) InboundByMessageID(ctx context.Context, account, messageID string) (InboundRecord, error) {
	if err := checkAccount(account); err != nil {
		return InboundRecord{}, fmt.Errorf("invalid inbound query: %w", err)
	}
	if err := checkMessageID(messageID); err != nil {
		return InboundRecord{}, fmt.Errorf("invalid inbound query: %w", err)
	}
	var record InboundRecord
	var digest []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT i.task_id, i.body_sha256, i.folder, i.uid_validity, i.uid, r.seq FROM inbound_messages i JOIN replies r ON r.inbound_id = i.id "+
			"WHERE i.account = ? AND i.message_id = ?", account, messageID).
		Scan(&record.TaskID, &digest, &record.Folder, &record.UIDValidity, &record.UID, &record.ReplySeq)
	if errors.Is(err, sql.ErrNoRows) {
		return InboundRecord{}, fmt.Errorf("inbound message: %w", ErrNotFound)
	}
	if err != nil {
		return InboundRecord{}, fmt.Errorf("cannot read inbound messages: %w", err)
	}
	// 表约束保证摘要恰为 32 字节。
	copy(record.BodyDigest[:], digest)
	return record, nil
}

// CountAcceptedRepliesSince 返回任务的回复中 created_at 不早于 since（按毫秒，恰在 since 的计入）、状态不是 REJECTED 的条数，即 D8 回环刹车
// 所计的「已接受入队」的回复；1 小时与 24 小时两个上限都用它，调用方取窗口起点与最近一次解除暂停的时刻（MailPaused）中较晚的一个作为
// since。任务 ID 不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) CountAcceptedRepliesSince(ctx context.Context, taskID string, since time.Time) (int, error) {
	if err := checkTaskID(taskID); err != nil {
		return 0, fmt.Errorf("invalid reply query: %w", err)
	}
	return s.count(ctx, "replies", "SELECT count(*) FROM replies WHERE task_id = ? AND state <> ? AND created_at >= ?",
		taskID, string(queue.Rejected), since.UnixMilli())
}

// findInbound 按 where 条件（某个唯一键）查找已有入站记录及其回复序号；where 只来自本文件的常量，未找到时返回 nil。
func findInbound(ctx context.Context, tx *sql.Tx, where string, args ...any) (*knownInbound, error) {
	var known knownInbound
	err := tx.QueryRowContext(ctx,
		"SELECT i.message_id, i.body_sha256, i.task_id, r.seq FROM inbound_messages i JOIN replies r ON r.inbound_id = i.id WHERE "+where, args...).
		Scan(&known.messageID, &known.digest, &known.taskID, &known.seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read inbound messages: %w", err)
	}
	return &known, nil
}

// ClaimNextReply 在一个事务中把任务的最早 QUEUED 回复改为 DISPATCHING，
// 记录派发前的任务状态，并执行 reply_dispatched 使任务进入 RUNNING，同时返回解密后的正文。
// 任务不存在时返回 ErrNotFound；任务不处于 CanDispatchReply 为 true 的状态、已有 DISPATCHING 或 UNCERTAIN 回复，
// 或没有 QUEUED 回复时返回 ErrNoDispatchableReply。IMMEDIATE 事务使多个进程的并发派发串行执行，至多一个成功。
// 解密在同一事务中、改动任何数据之前进行：正文行缺失返回 ErrPayloadMissing，没有密钥或 kid 不符返回 ErrPayloadKeyUnavailable，
// 密文无法解密返回包装 payload.ErrDecrypt 的错误；三种情况都不领取，回复停在队首，需要人工处理。与 ClaimNextNotification 一样，
// 读出正文失败时（包括这三种情况）返回该回复未改动的 QUEUED 快照与任务的当前快照（正文为 nil），而不是零值：派发循环据此发出
// 带回复序号的事件。
func (s *Store) ClaimNextReply(ctx context.Context, taskID string) (Reply, Task, []byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reply{}, Task{}, nil, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getTask(ctx, tx, taskID)
	if err != nil {
		return Reply{}, Task{}, nil, err
	}
	if !task.CanDispatchReply(current.State) {
		return Reply{}, Task{}, nil, fmt.Errorf("%w: task %q is %s", ErrNoDispatchableReply, taskID, current.State)
	}
	// 任务可派发时仍可能有在途回复（例如 Agent 已结束回合而回复尚未确认），必须先核对它，不能派发下一条。
	var inFlight int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM replies WHERE task_id = ? AND state IN (?, ?)",
		taskID, string(queue.Dispatching), string(queue.Uncertain)).Scan(&inFlight); err != nil {
		return Reply{}, Task{}, nil, fmt.Errorf("cannot read replies: %w", err)
	}
	if inFlight > 0 {
		return Reply{}, Task{}, nil, fmt.Errorf("%w: task %q has a reply in flight", ErrNoDispatchableReply, taskID)
	}
	var seq int64
	err = tx.QueryRowContext(ctx, "SELECT seq FROM replies WHERE task_id = ? AND state = ? ORDER BY seq LIMIT 1",
		taskID, string(queue.Queued)).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return Reply{}, Task{}, nil, fmt.Errorf("%w: task %q has no queued reply", ErrNoDispatchableReply, taskID)
	}
	if err != nil {
		return Reply{}, Task{}, nil, fmt.Errorf("cannot read replies: %w", err)
	}
	queued, err := getReply(ctx, tx, seq)
	if err != nil {
		return Reply{}, Task{}, nil, err
	}
	body, err := s.openPayload(ctx, tx, payload.KindReply, "reply",
		"SELECT key_id, sealed FROM reply_payloads WHERE seq = ?", queued.TaskID, queued.Seq)
	if err != nil {
		// 事务随后回滚，没有改动任何数据；返回的快照就是读取时的样子。
		return queued, current, nil, err
	}
	next, err := nextReply(queued, queue.Queued, queue.Claim)
	if err != nil {
		return Reply{}, Task{}, nil, err
	}
	next.ResumeState = current.State
	running, err := task.Next(current.State, task.ReplyDispatched)
	if err != nil {
		return Reply{}, Task{}, nil, err
	}
	now := s.now().UnixMilli()
	if err := updateReply(ctx, tx, queued, next, now); err != nil {
		return Reply{}, Task{}, nil, err
	}
	if err := writeTaskTransition(ctx, tx, current, task.ReplyDispatched, running, "", now); err != nil {
		return Reply{}, Task{}, nil, err
	}
	reply, dispatched, err := commitReply(ctx, tx, seq)
	if err != nil {
		return Reply{}, Task{}, nil, err
	}
	return reply, dispatched, body, nil
}

// AcknowledgeReply 在 Agent 确认收到后把 DISPATCHING 回复改为 ACKNOWLEDGED；任务状态不变。
// 回复处于其他状态时返回 queue.ErrInvalidTransition；UNCERTAIN 回复须经 ResolveUncertainReply 核对。
func (s *Store) AcknowledgeReply(ctx context.Context, seq int64) (Reply, Task, error) {
	return s.changeReply(ctx, seq, func(ctx context.Context, tx *sql.Tx, r Reply, _ Task, now int64) error {
		next, err := nextReply(r, queue.Dispatching, queue.Acknowledge)
		if err != nil {
			return err
		}
		return updateReply(ctx, tx, r, next, now)
	})
}

// MarkReplyUncertain 在无法确认 Agent 是否收到时把 DISPATCHING 改为 UNCERTAIN；
// 任务为 RUNNING 时同时执行 delivery_unknown。
func (s *Store) MarkReplyUncertain(ctx context.Context, seq int64) (Reply, Task, error) {
	return s.changeReply(ctx, seq, markUncertain)
}

// RequeueUnsentReply 在确定回复没有发给 Agent 时把 DISPATCHING 回复按原序号放回 QUEUED，
// 任务恢复到派发前状态；任务已不接受回复时改为 REJECTED。
// 任务仍接受回复、但自派发以来已发生其他事件（例如回合结束或审批往返）时，说明 Agent 已处理该回复，
// 返回包装 task.ErrInvalidTransition 的错误且不改动数据；此时应改用 AcknowledgeReply 确认，确认后不再阻塞后续派发。
func (s *Store) RequeueUnsentReply(ctx context.Context, seq int64) (Reply, Task, error) {
	return s.changeReply(ctx, seq, func(ctx context.Context, tx *sql.Tx, r Reply, t Task, now int64) error {
		return putBack(ctx, tx, r, queue.Dispatching, t, now)
	})
}

// ResolveUncertainReply 记录本地核对结果：delivered 为 true 时回复 ACKNOWLEDGED（任务已关闭或失败时同样如此），
// 任务处于 DELIVERY_UNCERTAIN 时执行 delivery_confirmed，其他状态不变。为 false 时按 RequeueUnsentReply 的规则处理：
// 任务已关闭或失败时回复改为 REJECTED、任务状态不变；否则须任务自派发以来只发生过标记不确定时的 delivery_unknown，
// 回复才按原序号回到 QUEUED、任务恢复派发前状态，不满足时返回包装 task.ErrInvalidTransition 的错误，应改为核对为已送达。
// 回复不处于 UNCERTAIN 时返回 queue.ErrInvalidTransition。
func (s *Store) ResolveUncertainReply(ctx context.Context, seq int64, delivered bool) (Reply, Task, error) {
	return s.changeReply(ctx, seq, func(ctx context.Context, tx *sql.Tx, r Reply, t Task, now int64) error {
		if !delivered {
			return putBack(ctx, tx, r, queue.Uncertain, t, now)
		}
		next, err := nextReply(r, queue.Uncertain, queue.Acknowledge)
		if err != nil {
			return err
		}
		if err := updateReply(ctx, tx, r, next, now); err != nil {
			return err
		}
		if t.State != task.DeliveryUncertain {
			return nil
		}
		running, err := task.Next(t.State, task.DeliveryConfirmed)
		if err != nil {
			return err
		}
		return writeTaskTransition(ctx, tx, t, task.DeliveryConfirmed, running, "", now)
	})
}

// RecoverInFlight 在进程启动时调用：把所有 DISPATCHING 回复改为 UNCERTAIN（对应 RUNNING 任务进入 DELIVERY_UNCERTAIN），
// 返回全部 UNCERTAIN 回复供本地核对；不会自动重新派发任何回复。
// 回复按序号升序返回；恢复后再次调用不改动任何数据，返回相同的结果。
// 它把全部 DISPATCHING 回复都当作崩溃遗留，因此只能由唯一的派发进程在开始派发之前调用，调用期间不得有其他进程持有在途回复，
// 否则另一个进程正在派发的回复会被误标为 UNCERTAIN；保证这一点的单实例锁属于尚未实现的后台服务。
func (s *Store) RecoverInFlight(ctx context.Context) ([]Reply, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	dispatching, err := repliesIn(ctx, tx, queue.Dispatching)
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	for _, reply := range dispatching {
		current, err := getTask(ctx, tx, reply.TaskID)
		if err != nil {
			return nil, err
		}
		if err := markUncertain(ctx, tx, reply, current, now); err != nil {
			return nil, err
		}
	}
	uncertain, err := repliesIn(ctx, tx, queue.Uncertain)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cannot commit recovery: %w", err)
	}
	return uncertain, nil
}

// changeReply 在一个事务中读取回复及其任务，交给 change 经状态机计算并写入变化，提交后返回二者的最新快照。
// 回复不存在时返回 ErrNotFound；change 返回错误时事务回滚，数据保持不变。各操作只接受非终态的来源状态，成功后回复若处于
// ACKNOWLEDGED 或 REJECTED，即是本次进入的终态，正文已由触发器删除，提交后执行检查点
// （AcknowledgeReply、ResolveUncertainReply、RequeueUnsentReply 都可能如此）。
func (s *Store) changeReply(ctx context.Context, seq int64, change func(ctx context.Context, tx *sql.Tx, r Reply, t Task, now int64) error) (Reply, Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reply{}, Task{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	reply, err := getReply(ctx, tx, seq)
	if err != nil {
		return Reply{}, Task{}, err
	}
	current, err := getTask(ctx, tx, reply.TaskID)
	if err != nil {
		return Reply{}, Task{}, err
	}
	if err := change(ctx, tx, reply, current, s.now().UnixMilli()); err != nil {
		return Reply{}, Task{}, err
	}
	updated, after, err := commitReply(ctx, tx, seq)
	if err != nil {
		return Reply{}, Task{}, err
	}
	if updated.State == queue.Acknowledged || updated.State == queue.Rejected {
		truncateWAL(ctx, s.db)
	}
	return updated, after, nil
}

// commitReply 重新读取回复及其任务的最新快照，然后提交事务 tx。
func commitReply(ctx context.Context, tx *sql.Tx, seq int64) (Reply, Task, error) {
	reply, err := getReply(ctx, tx, seq)
	if err != nil {
		return Reply{}, Task{}, err
	}
	current, err := getTask(ctx, tx, reply.TaskID)
	if err != nil {
		return Reply{}, Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reply{}, Task{}, fmt.Errorf("cannot commit reply: %w", err)
	}
	return reply, current, nil
}

// markUncertain 把 DISPATCHING 回复改为 UNCERTAIN；任务为 RUNNING 时经 task 状态机执行 delivery_unknown，其他状态保持不变。
func markUncertain(ctx context.Context, tx *sql.Tx, r Reply, t Task, now int64) error {
	next, err := nextReply(r, queue.Dispatching, queue.MarkUncertain)
	if err != nil {
		return err
	}
	if err := updateReply(ctx, tx, r, next, now); err != nil {
		return err
	}
	if t.State != task.Running {
		return nil
	}
	uncertain, err := task.Next(t.State, task.DeliveryUnknown)
	if err != nil {
		return err
	}
	return writeTaskTransition(ctx, tx, t, task.DeliveryUnknown, uncertain, "", now)
}

// putBack 处理确认未送达 Agent 的在途回复，回复须处于 from：任务已不接受回复时回复改为 REJECTED，
// 原因与 RecordReply 相同，任务不变。否则任务最新的事件必须恰为 eventsSinceDispatch[from]，
// 即自派发以来任务没有发生其他事件，回复才按原序号回到 QUEUED 并清除派发前状态，
// 任务经 task.ResumeAfterUnsent 恢复派发前状态并记录 reply_unsent 事件；任务已继续推进说明 Agent 已处理该回复，
// 放回队列会重复投递，因此返回包装 task.ErrInvalidTransition 的错误，由调用方把回复核对为已送达。
func putBack(ctx context.Context, tx *sql.Tx, r Reply, from queue.State, t Task, now int64) error {
	if !task.AcceptsReplies(t.State) {
		next, err := nextReply(r, from, queue.Reject)
		if err != nil {
			return err
		}
		next.RejectReason = cmp.Or(stateRejectReasons[t.State], "task_not_accepting")
		return updateReply(ctx, tx, r, next, now)
	}
	next, err := nextReply(r, from, queue.Requeue)
	if err != nil {
		return err
	}
	want := eventsSinceDispatch[from]
	var latest string
	if err := tx.QueryRowContext(ctx,
		"SELECT coalesce(group_concat(event, ' ' ORDER BY seq), '') FROM (SELECT seq, event FROM task_events WHERE task_id = ? ORDER BY seq DESC LIMIT ?)",
		t.ID, len(want)).Scan(&latest); err != nil {
		return fmt.Errorf("cannot read task events: %w", err)
	}
	if latest != strings.Join(want, " ") {
		return fmt.Errorf("%w: reply %d: 任务在派发后已继续推进，回复应核对为已送达", task.ErrInvalidTransition, r.Seq)
	}
	resumed, err := task.ResumeAfterUnsent(t.State, r.ResumeState)
	if err != nil {
		return err
	}
	next.ResumeState = ""
	if err := updateReply(ctx, tx, r, next, now); err != nil {
		return err
	}
	return writeTaskTransition(ctx, tx, t, replyUnsent, resumed, "", now)
}

// nextReply 确认回复处于该操作要求的来源状态 from，再用 queue.Next 计算 event 之后的状态，返回改写了状态的副本。
// 队列状态机允许、但不属于该操作的来源状态（例如对 UNCERTAIN 回复调用 AcknowledgeReply）同样按非法转移拒绝。
func nextReply(r Reply, from queue.State, event queue.Event) (Reply, error) {
	if r.State != from {
		return Reply{}, fmt.Errorf("%w: reply %d is %s, %s requires %s", queue.ErrInvalidTransition, r.Seq, r.State, event, from)
	}
	to, err := queue.Next(from, event)
	if err != nil {
		return Reply{}, err
	}
	r.State = to
	return r, nil
}

// updateReply 以读取时的状态为条件把回复改写为 next 的状态、派发前任务状态与拒绝原因。
// IMMEDIATE 事务已在读取前取得写锁，状态条件只作防御；受影响行数不为 1 时返回错误使事务回滚。
func updateReply(ctx context.Context, tx *sql.Tx, current, next Reply, now int64) error {
	result, err := tx.ExecContext(ctx,
		"UPDATE replies SET state = ?, resume_state = ?, reject_reason = ?, updated_at = ? WHERE seq = ? AND state = ?",
		string(next.State), sql.NullString{String: string(next.ResumeState), Valid: next.ResumeState != ""},
		sql.NullString{String: next.RejectReason, Valid: next.RejectReason != ""}, now, current.Seq, string(current.State))
	if err != nil {
		return fmt.Errorf("cannot update reply: %w", err)
	}
	if updated, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("cannot update reply: %w", err)
	} else if updated != 1 {
		return fmt.Errorf("reply %d changed during update", current.Seq)
	}
	return nil
}

// repliesIn 按序号升序返回处于 state 的全部回复快照：先读完序号、结果集随之关闭，再在同一事务中逐条读取。
func repliesIn(ctx context.Context, tx *sql.Tx, state queue.State) ([]Reply, error) {
	rows, err := tx.QueryContext(ctx, "SELECT seq FROM replies WHERE state = ? ORDER BY seq", string(state))
	if err != nil {
		return nil, fmt.Errorf("cannot read replies: %w", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("cannot read replies: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read replies: %w", err)
	}
	var replies []Reply
	for _, seq := range seqs {
		reply, err := getReply(ctx, tx, seq)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, nil
}

// getReply 通过 q 按序号读取回复快照，时间换算为 UTC；回复不存在时返回 ErrNotFound，
// 回复状态或派发前任务状态不在已知集合时返回错误而不是静默接受。
func getReply(ctx context.Context, q rowQuerier, seq int64) (Reply, error) {
	var r Reply
	var resumeState, rejectReason sql.NullString
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx,
		"SELECT seq, task_id, state, resume_state, reject_reason, created_at, updated_at FROM replies WHERE seq = ?", seq).
		Scan(&r.Seq, &r.TaskID, &r.State, &resumeState, &rejectReason, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Reply{}, fmt.Errorf("reply %d: %w", seq, ErrNotFound)
	}
	if err != nil {
		return Reply{}, fmt.Errorf("cannot read reply: %w", err)
	}
	if !r.State.Valid() {
		return Reply{}, fmt.Errorf("reply %d has unknown state %q", seq, r.State)
	}
	r.ResumeState = task.State(resumeState.String)
	if resumeState.Valid && !r.ResumeState.Valid() {
		return Reply{}, fmt.Errorf("reply %d has unknown resume state %q", seq, r.ResumeState)
	}
	r.RejectReason = rejectReason.String
	r.CreatedAt = time.UnixMilli(createdAt).UTC()
	r.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return r, nil
}
