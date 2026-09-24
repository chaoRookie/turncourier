// Package sqlite 持久化待发通知：创建时只以密文保存内容，按到期顺序串行领取，记录投递结果；投递结果不确定时只标记为
// UNCERTAIN，等待本地核对或凭「已发送」中的副本核对为已送达（D7），从不自动重发。另提供 4b 收发循环所需的查找、「最新通知」、
// 线程引用与发信计数。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/security/payload"
	"github.com/chaoRookie/turncourier/internal/task"
)

// NewNotification 是创建待发通知的输入。Content 是 4b 渲染通知所需的内容（格式由 4b 定义），只以密文落盘。
type NewNotification struct {
	TaskID  string
	Event   string        // turn_completed、waiting_input、waiting_approval、failed
	Domain  string        // 机器人地址的域名，用作 Message-ID 右侧；1–253 个 [a-z0-9.-] 字符
	TTL     time.Duration // 令牌有效期，调用方传 config.Security.TokenTTL；1h–720h
	Content []byte        // 1 字节到 payload.MaxPlaintext
}

// Notification 是待发通知的持久化快照，不含内容。
type Notification struct {
	ID                 int64
	TaskID             string
	Owner              string // 任务的 owner，与 TaskID、TokenExpiresAt 一起构成令牌的 Claims
	Event              string
	NID                [12]byte
	MessageID          string // 我方 Message-ID，创建时生成，重试不变
	DeliveredMessageID string // 实际投递的 Message-ID；来源由 L1 决定（D4），未知时为空
	TokenKeyID         uint8
	TokenExpiresAt     time.Time
	State              queue.OutboxState
	AbandonReason      string // rejected、task_closed、expired、manual
	Attempts           int
	NotBefore          time.Time
	SentAt             time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

var (
	// ErrPayloadKeyUnavailable 表示没有正文密钥，或密文的 key_id 与当前密钥不符、该 kid 未登记为 active。
	ErrPayloadKeyUnavailable = errors.New("payload key is not available")
	// ErrPayloadMissing 表示待处理的通知或回复没有正文行（例如 0002 之前写入的回复）。
	ErrPayloadMissing = errors.New("pending payload is missing")
	// ErrNoSendableNotification 表示已有通知在发送中，或没有到期的 PENDING 通知。
	ErrNoSendableNotification = errors.New("no sendable notification")
	// ErrTaskNotNotifiable 表示任务已关闭，不再创建通知。
	ErrTaskNotNotifiable = errors.New("task does not accept notifications")
	// ErrDeliveredMessageIDConflict 表示实际投递的 Message-ID 已记录为别的值，或已属于另一条通知。
	ErrDeliveredMessageIDConflict = errors.New("delivered message id conflict")
)

const (
	// minNotificationTTL 与 maxNotificationTTL 是令牌有效期的上下界，与配置 security.token_ttl 的允许范围一致。
	minNotificationTTL = time.Hour
	maxNotificationTTL = 720 * time.Hour
	// maxRequeueDelay 是放回队列时下次可发送时间最多推迟的时长。
	maxRequeueDelay = 24 * time.Hour
	// expiryMargin：令牌在这段时间内就会过期的 PENDING 通知在领取前被放弃，通知发出后用户已来不及回复。
	expiryMargin = 10 * time.Minute
	// nidLen 是通知 ID（nid）的字节数；messageIDRandomLen 是我方 Message-ID 随机部分的字节数，编码为 24 位 base32。
	nidLen             = 12
	messageIDRandomLen = 15
	// maxOwnerLen 是 owner 的字节数上限，与 token.Claims 要求的 1–255 字节一致：令牌 MAC 以 uint16 记下 owner 的字节数，
	// 超出这个范围的 owner 签不出令牌。存储不导入 token 包，这里重复它的取值。
	maxOwnerLen = 255
	// maxThreadReferences 是 ThreadReferences 的 limit 上限：一封通知的 References 至多列出 20 个此前的实际投递 ID。
	maxThreadReferences = 20
	// maxNotificationList 是 AbandonedSince 与 SentWithoutDeliveredID 一页最多返回的条数；返回满这个数时，调用方以本页最后一行为游标接着取。
	maxNotificationList = 100
)

const (
	// abandonRejected 与 abandonManual 是调用方可以传给 AbandonNotification 的放弃原因：服务器永久拒绝、人工放弃。
	abandonRejected = "rejected"
	abandonManual   = "manual"
	// abandonTaskClosed 与 abandonExpired 只由存储自身写入：任务已关闭、令牌将在 expiryMargin 内过期。
	abandonTaskClosed = "task_closed"
	abandonExpired    = "expired"
)

// notificationEvents 是可以创建通知的事件，与 notifications.event 的 CHECK 约束一致。
var notificationEvents = map[string]bool{
	"turn_completed":   true,
	"waiting_input":    true,
	"waiting_approval": true,
	"failed":           true,
}

var (
	// taskIDPattern 是任务 ID 的形式：10 位小写 Crockford base32；Go 正则的 $ 只匹配文本末尾，末尾换行同样被拒绝。
	taskIDPattern = regexp.MustCompile("^[" + crockfordAlphabet + "]{10}$")
	// domainPattern 是机器人地址域名的形式：1–253 个小写字母、数字、点或连字符。
	domainPattern = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)
)

// getNotification 与 listNotifications 的查找条件，接在 notificationSelect 之后；参数由调用方按占位符顺序给出。
const (
	notificationByID  = "n.id = ?"
	notificationByNID = "n.nid = ?"
	// notificationByMessageID 与 notificationByDeliveredID 按我方 Message-ID 与实际投递 ID 查找，两列都有 UNIQUE 约束。
	notificationByMessageID   = "n.message_id = ?"
	notificationByDeliveredID = "n.delivered_message_id = ?"
	// latestAttempted 取任务的 SENT 通知与 updated_at 不早于给定时刻的 UNCERTAIN 通知中，发出时刻最晚、相同时 id 较大的一条：
	// SENT 的 sent_at 非空（表约束），UNCERTAIN 的 sent_at 为空，coalesce 因此分别取 sent_at 与进入 UNCERTAIN 的时刻 updated_at。
	// 参数依次为任务 ID、SENT、UNCERTAIN 与时刻；子查询没有结果时条件为 NULL，查找返回 ErrNotFound。
	latestAttempted = "n.id = (SELECT id FROM notifications WHERE task_id = ? AND (state = ? OR (state = ? AND updated_at >= ?)) " +
		"ORDER BY coalesce(sent_at, updated_at) DESC, id DESC LIMIT 1)"
	// latestSent 取任务的 SENT 通知中 sent_at 最晚、相同时 id 较大的一条；参数依次为任务 ID 与 SENT。
	latestSent = "n.id = (SELECT id FROM notifications WHERE task_id = ? AND state = ? ORDER BY sent_at DESC, id DESC LIMIT 1)"
	// abandonedSince 取原因为给定两者之一、位于游标 (updated_at, id) 之后的 ABANDONED 通知，按 (updated_at, id) 升序：updated_at 晚于
	// 给定时刻，或恰为该时刻且 id 大于给定的 id。参数依次为 ABANDONED、两个原因、时刻、同一时刻、id 与上限。
	abandonedSince = "n.state = ? AND n.abandon_reason IN (?, ?) AND (n.updated_at > ? OR (n.updated_at = ? AND n.id > ?)) " +
		"ORDER BY n.updated_at, n.id LIMIT ?"
	// sentWithoutDelivered 取 sent_at 早于给定时刻、仍没有实际投递 ID、id 大于给定 id 的 SENT 通知，按 id 升序；参数依次为 SENT、时刻、
	// id 与上限。
	sentWithoutDelivered = "n.state = ? AND n.delivered_message_id IS NULL AND n.sent_at < ? AND n.id > ? ORDER BY n.id LIMIT ?"
)

// validate 在事务开始前检查事件、域名、有效期、内容长度与任务 ID 的形式，不合法时返回包装 ErrInvalidArgument 的错误；错误文本不含内容。
func (n NewNotification) validate() error {
	switch {
	case !notificationEvents[n.Event]:
		return invalidArgument("invalid notification: unknown event %q", n.Event)
	case !domainPattern.MatchString(n.Domain):
		return invalidArgument("invalid notification: domain must be 1-253 characters of [a-z0-9.-]")
	case n.TTL < minNotificationTTL || n.TTL > maxNotificationTTL:
		return invalidArgument("invalid notification: token ttl must be 1h-720h")
	case len(n.Content) == 0 || len(n.Content) > payload.MaxPlaintext:
		return invalidArgument("invalid notification: content must be 1 byte to 1 MiB")
	case !taskIDPattern.MatchString(n.TaskID):
		return invalidArgument("invalid notification: task id must be 10 lowercase Crockford base32 characters")
	}
	return nil
}

// CreateNotification 在一个事务中：确认任务存在且未关闭（FAILED 任务允许，用于发送失败通知）；确认任务的 owner 在
// token.Claims 要求的 1–255 字节内，否则这条通知永远签不出令牌（返回包装 ErrInvalidArgument 的错误：重试也不会成功）；读取令牌用途的 active kid
// （没有时返回包装 ErrNotFound 的错误）；确认正文密钥的 kid 已登记为 active；从随机源生成 12 字节 nid 与 15 字节
// Message-ID 随机部分；以 PENDING、attempts 0、not_before 与 created_at 为当前时间、token_expires_at 为当前时间加 TTL
// 插入通知行；再用 (KindNotification, TaskID, id) 加密内容并插入 notification_payloads。nid 或 Message-ID 冲突时报错，不重试。
func (s *Store) CreateNotification(ctx context.Context, n NewNotification) (Notification, error) {
	if err := n.validate(); err != nil {
		return Notification{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getTask(ctx, tx, n.TaskID)
	if err != nil {
		return Notification{}, err
	}
	if current.State == task.Closed {
		return Notification{}, fmt.Errorf("%w: task %q is %s", ErrTaskNotNotifiable, n.TaskID, current.State)
	}
	// owner 来自任务行，不在 NewNotification 中；它超出令牌 Claims 的范围时这条通知永远签不出令牌，因此不创建。
	// 错误文本不含 owner。
	if length := len(current.Owner); length == 0 || length > maxOwnerLen {
		return Notification{}, invalidArgument("invalid notification: owner of task %q must be 1-%d bytes", n.TaskID, maxOwnerLen)
	}
	tokenKID, err := activeKeyID(ctx, tx, KeyPurposeToken)
	if err != nil {
		return Notification{}, err
	}
	key, err := s.activePayloadKey(ctx, tx)
	if err != nil {
		return Notification{}, err
	}
	var random [nidLen + messageIDRandomLen]byte
	if _, err := io.ReadFull(s.random, random[:]); err != nil {
		return Notification{}, fmt.Errorf("cannot generate notification id: %w", err)
	}
	messageID := "<tc." + instanceEncoding.EncodeToString(random[nidLen:]) + "@" + n.Domain + ">"
	now := s.now().UnixMilli()
	var id int64
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO notifications (task_id, event, nid, message_id, token_kid, token_expires_at, state, attempts, not_before, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?) RETURNING id",
		n.TaskID, n.Event, random[:nidLen], messageID, tokenKID, now+n.TTL.Milliseconds(), string(queue.OutboxPending), now, now, now).Scan(&id); err != nil {
		return Notification{}, fmt.Errorf("cannot create notification: %w", err)
	}
	sealed, err := key.Seal(payload.KindNotification, n.TaskID, id, n.Content)
	if err != nil {
		return Notification{}, fmt.Errorf("cannot seal notification: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO notification_payloads (notification_id, key_id, sealed) VALUES (?, ?, ?)", id, key.ID(), sealed); err != nil {
		return Notification{}, fmt.Errorf("cannot store notification content: %w", err)
	}
	created, err := getNotification(ctx, tx, notificationByID, id)
	if err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, fmt.Errorf("cannot commit notification: %w", err)
	}
	return created, nil
}

// ClaimNextNotification 分两个事务执行。第一个事务把以下 PENDING 通知改为 ABANDONED（经 queue.NextOutbox 校验）：
// token_expires_at 不晚于当前时间加 10 分钟的（expired，令牌发出后已来不及回复）；所属任务已 CLOSED 的（task_closed，
// 正常路径不会产生这种行，这是兜底）。有改动时提交后执行检查点；第一个事务出错时整体回滚并返回错误，不进行领取。
// 第二个事务：已有 SENDING 通知时返回 ErrNoSendableNotification；取 not_before 不晚于当前时间、token_expires_at 晚于当前时间
// 加 10 分钟、按 (not_before, id) 最早的 PENDING 通知（两个事务之间被放回 PENDING 的将过期通知因此不会被领取，留待下次领取时放弃）；
// 读出并解密内容，失败时不领取，返回该通知未改动的 PENDING 快照（内容为 nil，调用方可据此告警或调用
// AbandonNotification(manual)）与 ErrPayloadMissing、ErrPayloadKeyUnavailable 或包装 payload.ErrDecrypt 的错误；
// 成功时改为 SENDING、attempts 加 1，返回快照与明文内容。从不领取 UNCERTAIN 通知。
// IMMEDIATE 事务使多个进程的并发领取串行执行，加上 notifications_one_sending 索引，全局至多一条 SENDING。
func (s *Store) ClaimNextNotification(ctx context.Context) (Notification, []byte, error) {
	if err := s.abandonUnsendable(ctx); err != nil {
		return Notification{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, nil, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	var sending int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM notifications WHERE state = ?", string(queue.OutboxSending)).Scan(&sending); err != nil {
		return Notification{}, nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	if sending > 0 {
		return Notification{}, nil, fmt.Errorf("%w: a notification is being sent", ErrNoSendableNotification)
	}
	now := s.now().UnixMilli()
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM notifications WHERE state = ? AND not_before <= ? AND token_expires_at > ? ORDER BY not_before, id LIMIT 1",
		string(queue.OutboxPending), now, now+expiryMargin.Milliseconds()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, nil, fmt.Errorf("%w: no pending notification is due", ErrNoSendableNotification)
	}
	if err != nil {
		return Notification{}, nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	pending, err := getNotification(ctx, tx, notificationByID, id)
	if err != nil {
		return Notification{}, nil, err
	}
	content, err := s.openPayload(ctx, tx, payload.KindNotification, "notification",
		"SELECT key_id, sealed FROM notification_payloads WHERE notification_id = ?", pending.TaskID, pending.ID)
	if err != nil {
		return pending, nil, err
	}
	next, err := nextNotification(pending, queue.OutboxPending, queue.OutboxClaim)
	if err != nil {
		return Notification{}, nil, err
	}
	next.Attempts++
	if err := updateNotification(ctx, tx, pending, next, now); err != nil {
		return Notification{}, nil, err
	}
	claimed, err := getNotification(ctx, tx, notificationByID, id)
	if err != nil {
		return Notification{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, nil, fmt.Errorf("cannot commit notification claim: %w", err)
	}
	return claimed, content, nil
}

// abandonUnsendable 是 ClaimNextNotification 的第一个事务：放弃令牌将在 expiryMargin 内过期（expired）与所属任务已关闭
// （task_closed）的 PENDING 通知；有改动时提交并执行检查点，否则回滚。
func (s *Store) abandonUnsendable(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略；没有改动时由它结束事务。
	defer tx.Rollback()
	now := s.now().UnixMilli()
	expired, err := abandonPending(ctx, tx, abandonExpired, now, "token_expires_at <= ?", now+expiryMargin.Milliseconds())
	if err != nil {
		return err
	}
	closed, err := abandonPending(ctx, tx, abandonTaskClosed, now, "task_id IN (SELECT id FROM tasks WHERE state = ?)", string(task.Closed))
	if err != nil {
		return err
	}
	if expired+closed == 0 {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cannot commit abandoned notifications: %w", err)
	}
	truncateWAL(ctx, s.db)
	return nil
}

// abandonPending 在事务 tx 中把满足 where 条件的 PENDING 通知改为 ABANDONED(reason)，目标状态经 queue.NextOutbox 计算；
// 内容由触发器删除。返回受影响的行数；where 只来自本包的常量。
func abandonPending(ctx context.Context, tx *sql.Tx, reason string, now int64, where string, args ...any) (int64, error) {
	abandoned, err := queue.NextOutbox(queue.OutboxPending, queue.OutboxAbandon)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE notifications SET state = ?, abandon_reason = ?, updated_at = ? WHERE state = ? AND "+where,
		append([]any{string(abandoned), reason, now, string(queue.OutboxPending)}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("cannot abandon notifications: %w", err)
	}
	abandonedRows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cannot abandon notifications: %w", err)
	}
	return abandonedRows, nil
}

// MarkNotificationSent 在 SMTP 返回 250 后把 SENDING 改为 SENT，以当前时间记录 sent_at；内容由触发器删除，提交后执行检查点。
// 任务在发送期间被关闭时照样记为 SENT：邮件已经发出。
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) (Notification, error) {
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		return markSent(ctx, tx, n, queue.OutboxSending, time.UnixMilli(now).UTC(), now)
	})
}

// MarkNotificationUncertain 在 SMTP 已进入提交阶段、结束标记可能已写出但没有得到响应时（smtp.ErrUncertain）把 SENDING 改为 UNCERTAIN；内容保留。
func (s *Store) MarkNotificationUncertain(ctx context.Context, id int64) (Notification, error) {
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		next, err := nextNotification(n, queue.OutboxSending, queue.OutboxMarkUncertain)
		if err != nil {
			return err
		}
		return updateNotification(ctx, tx, n, next, now)
	})
}

// RequeueNotification 在确定未投递时把 SENDING 改回 PENDING，not_before 设为当前时间加 delay（0 到 24 小时）；
// 任务已关闭时改为 ABANDONED(task_closed)，内容由触发器删除，提交后执行检查点（与 ResolveUncertainNotification 一致）。
// delay 超出范围时在开始事务前返回包装 ErrInvalidArgument 的错误。
func (s *Store) RequeueNotification(ctx context.Context, id int64, delay time.Duration) (Notification, error) {
	if delay < 0 || delay > maxRequeueDelay {
		return Notification{}, invalidArgument("invalid notification requeue: delay must be 0-24h")
	}
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, t Task, now int64) error {
		return putBackNotification(ctx, tx, n, queue.OutboxSending, t, now+delay.Milliseconds(), now)
	})
}

// AbandonNotification 把 PENDING、SENDING 或 UNCERTAIN 通知改为 ABANDONED；reason 只能是 rejected 或 manual
// （task_closed 与 expired 只由存储自身写入），其他取值在开始事务前返回包装 ErrInvalidArgument 的错误。内容由触发器删除，提交后执行检查点。
func (s *Store) AbandonNotification(ctx context.Context, id int64, reason string) (Notification, error) {
	if reason != abandonRejected && reason != abandonManual {
		return Notification{}, invalidArgument("invalid notification abandon reason %q: must be rejected or manual", reason)
	}
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		// 三种来源状态都可以放弃，由 queue.NextOutbox 拒绝终态，因此来源状态取通知的当前状态。
		next, err := nextNotification(n, n.State, queue.OutboxAbandon)
		if err != nil {
			return err
		}
		next.AbandonReason = reason
		return updateNotification(ctx, tx, n, next, now)
	})
}

// ResolveUncertainNotification 记录本地核对结果：delivered 为 true 时 UNCERTAIN 改为 SENT，sent_at 取通知进入 UNCERTAIN 的时刻
// （核对前的 updated_at）而不是核对时刻，与 D7 的副本核对（ResolveUncertainAsDelivered）相同：发出顺序与发信计数因此不随核对而变，
// 较早进入 UNCERTAIN、在较新的通知发出之后才核对的通知不会因此成为最新的 SENT 通知（否则对较新通知的回复会被误判为
// token_superseded），也不会在核对时刻再计入一次发信；updated_at 为核对时刻。为 false 时改回 PENDING（not_before 为当前时间），
// 任务已关闭时改为 ABANDONED(task_closed)。进入 SENT 或 ABANDONED 时内容由触发器删除，提交后执行检查点。
// 由本地核对调用；D7 的副本证据改走 ResolveUncertainAsDelivered。
func (s *Store) ResolveUncertainNotification(ctx context.Context, id int64, delivered bool) (Notification, error) {
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, t Task, now int64) error {
		if delivered {
			return resolveDelivered(ctx, tx, n, now)
		}
		return putBackNotification(ctx, tx, n, queue.OutboxUncertain, t, now, now)
	})
}

// RecordDeliveredMessageID 为 SENT 通知记录实际投递的 Message-ID；同值重复记录不改动，已有不同值或该值已属于另一条通知时返回
// ErrDeliveredMessageIDConflict；通知不是 SENT 时返回 queue.ErrInvalidOutboxTransition。值按 ValidMessageID 校验（3–998 个字符的
// 合法 UTF-8、不含 NUL，长度按 Unicode 字符计，与表约束一致；理由同 checkMailbox：SQLite 的 length() 对非法 UTF-8 的计数与 Go 不同，
// 否则 Go 端判为合规的取值会在开始事务之后才被 CHECK 约束拒绝，或者带着非法字节落盘），不合法时在开始事务前返回包装
// ErrInvalidArgument 的错误。
func (s *Store) RecordDeliveredMessageID(ctx context.Context, id int64, messageID string) (Notification, error) {
	if !ValidMessageID(messageID) {
		return Notification{}, invalidArgument("invalid notification delivered message id: must be 3-998 characters of valid UTF-8 without NUL")
	}
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		if n.State != queue.OutboxSent {
			return fmt.Errorf("%w: notification %d is %s, recording the delivered message id requires %s",
				queue.ErrInvalidOutboxTransition, n.ID, n.State, queue.OutboxSent)
		}
		if n.DeliveredMessageID == messageID {
			return nil
		}
		if n.DeliveredMessageID != "" {
			return fmt.Errorf("%w: notification %d already has a different delivered message id", ErrDeliveredMessageIDConflict, n.ID)
		}
		return recordDeliveredID(ctx, tx, n.ID, messageID, now)
	})
}

// ResolveUncertainAsDelivered 按 D7 凭「已发送」中的副本把 UNCERTAIN 通知核对为已送达：在一个事务中改为 SENT 并记下实际投递的
// Message-ID，状态与实际投递 ID 同时生效。与本地核对（ResolveUncertainNotification）相同，sent_at 取通知进入 UNCERTAIN 的时刻（核对前的
// updated_at）而不是核对时刻，通知的发出顺序与发信计数因此不随核对而变（LatestAttemptedNotification、LatestSentNotification 与
// CountNotificationsSince 都依赖这一点）；updated_at 为核对时刻。
// deliveredID 按 ValidMessageID 校验，不合法时在开始事务前返回包装 ErrInvalidArgument 的错误。通知不是 UNCERTAIN 时返回
// queue.ErrInvalidOutboxTransition，实际投递 ID 已属于别的通知时返回 ErrDeliveredMessageIDConflict，两种情况都不改动数据。
// 内容由触发器删除，提交后执行检查点。解除的方向始终是「已送达」，找不到副本的 UNCERTAIN 仍等待本地核对，从不自动重发。
func (s *Store) ResolveUncertainAsDelivered(ctx context.Context, id int64, deliveredID string) (Notification, error) {
	if !ValidMessageID(deliveredID) {
		return Notification{}, invalidArgument("invalid notification delivered message id: must be 3-998 characters of valid UTF-8 without NUL")
	}
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		// 表约束只允许 SENT 通知记录实际投递 ID，因此先改状态、再在同一事务中写入它；冲突或失败时整个事务回滚。
		if err := resolveDelivered(ctx, tx, n, now); err != nil {
			return err
		}
		return recordDeliveredID(ctx, tx, n.ID, deliveredID, now)
	})
}

// recordDeliveredID 在事务 tx 中为 SENT 通知 id 写入实际投递的 Message-ID：该值已属于另一条通知时返回 ErrDeliveredMessageIDConflict，
// 错误文本只含那条通知的 id，不含 Message-ID；只写入仍为空的列，受影响行数不为 1 时返回错误使事务回滚。
func recordDeliveredID(ctx context.Context, tx *sql.Tx, id int64, messageID string, now int64) error {
	var owner int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM notifications WHERE delivered_message_id = ?", messageID).Scan(&owner)
	if err == nil {
		return fmt.Errorf("%w: the delivered message id belongs to notification %d", ErrDeliveredMessageIDConflict, owner)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cannot read notifications: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE notifications SET delivered_message_id = ?, updated_at = ? WHERE id = ? AND state = ? AND delivered_message_id IS NULL",
		messageID, now, id, string(queue.OutboxSent))
	return requireOneRow(result, err, id)
}

// RecoverSendingNotifications 在发送进程启动时调用：把所有 SENDING 改为 UNCERTAIN，按 id 升序返回全部 UNCERTAIN 供本地核对，
// 从不重新发送；恢复后再次调用不改动任何数据，返回相同的结果。与 RecoverInFlight 相同，只能由唯一的发送进程在开始发送之前调用。
func (s *Store) RecoverSendingNotifications(ctx context.Context) ([]Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	sending, err := notificationsIn(ctx, tx, queue.OutboxSending)
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	for _, n := range sending {
		next, err := nextNotification(n, queue.OutboxSending, queue.OutboxMarkUncertain)
		if err != nil {
			return nil, err
		}
		if err := updateNotification(ctx, tx, n, next, now); err != nil {
			return nil, err
		}
	}
	uncertain, err := notificationsIn(ctx, tx, queue.OutboxUncertain)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cannot commit notification recovery: %w", err)
	}
	return uncertain, nil
}

// NotificationByNID 按 nid 读取通知，供 4b 验证令牌时取得 Claims；不存在时返回 ErrNotFound。
func (s *Store) NotificationByNID(ctx context.Context, nid [12]byte) (Notification, error) {
	return getNotification(ctx, s.db, notificationByNID, nid[:])
}

// NotificationByMessageID 按我方 Message-ID 读取通知，供「已发送」副本对应回通知（4b Task 10）；不存在时返回 ErrNotFound。
// Message-ID 按 ValidMessageID 校验，不合法时在查询前返回包装 ErrInvalidArgument 的错误，错误文本不回显取值。
func (s *Store) NotificationByMessageID(ctx context.Context, messageID string) (Notification, error) {
	if err := checkMessageID(messageID); err != nil {
		return Notification{}, fmt.Errorf("invalid notification query: %w", err)
	}
	return getNotification(ctx, s.db, notificationByMessageID, messageID)
}

// NotificationByDeliveredID 按实际投递的 Message-ID 读取通知，供退信关联回通知（4b Task 9）；不存在（包括尚未记下）时返回
// ErrNotFound。deliveredID 按 ValidMessageID 校验，不合法时在查询前返回包装 ErrInvalidArgument 的错误，错误文本不回显取值。
func (s *Store) NotificationByDeliveredID(ctx context.Context, deliveredID string) (Notification, error) {
	if err := checkMessageID(deliveredID); err != nil {
		return Notification{}, fmt.Errorf("invalid notification query: %w", err)
	}
	return getNotification(ctx, s.db, notificationByDeliveredID, deliveredID)
}

// LatestAttemptedNotification 返回任务的 SENT 通知与 updated_at 不早于 uncertainAfter（按毫秒，恰在其上的计入）的 UNCERTAIN 通知中，
// 发出时刻 COALESCE(sent_at, updated_at) 最晚的一条（相同时取 id 较大者）；没有时返回 ErrNotFound。它是 D7 中 token_superseded 所比较的
// 「最新一条已发出通知」：UNCERTAIN 多半已经送达，尚无缺失证据时按进入 UNCERTAIN 的时刻计入。uncertainAfter 由调用方按 D7 的缺失证据
// 给出——最近一次成功补扫「已发送」的开始时刻减 10 分钟，还没有成功补扫时为零值（零值早于任何记录的时刻，全部 UNCERTAIN 都计入）；
// updated_at 早于它的 UNCERTAIN 通知已有缺失证据，不再计入。按发出时刻而不是 id 排序：重新排队会推迟较早通知的 not_before，
// 它可能在较新的通知之后才发出，用户最后看到的是它；核对为已送达的通知（本地核对与 D7 的副本核对都如此）沿用进入 UNCERTAIN 的时刻，
// 顺序不随核对而变。任务 ID 不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) LatestAttemptedNotification(ctx context.Context, taskID string, uncertainAfter time.Time) (Notification, error) {
	if err := checkTaskID(taskID); err != nil {
		return Notification{}, fmt.Errorf("invalid notification query: %w", err)
	}
	return getNotification(ctx, s.db, latestAttempted, taskID, string(queue.OutboxSent), string(queue.OutboxUncertain), uncertainAfter.UnixMilli())
}

// LatestSentNotification 返回任务的 SENT 通知中 sent_at 最晚的一条（相同时取 id 较大者）；没有时返回 ErrNotFound。核对为已送达的通知
// 按进入 UNCERTAIN 的时刻参与（sent_at 沿用该时刻）。验证流水线据此判断有没有比令牌所属通知更新、确已发出的通知（4b Task 9 第 13 步）。
// 任务 ID 不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) LatestSentNotification(ctx context.Context, taskID string) (Notification, error) {
	if err := checkTaskID(taskID); err != nil {
		return Notification{}, fmt.Errorf("invalid notification query: %w", err)
	}
	return getNotification(ctx, s.db, latestSent, taskID, string(queue.OutboxSent))
}

// ThreadReferences 返回任务中 id 小于 beforeID、已记下实际投递 ID 的 SENT 通知的实际投递 ID，按 sent_at 升序（相同时按 id）取最后
// limit 个（1–20）：一个任务的通知属于一个邮件线程，通知 beforeID 的 References 列出它们，In-Reply-To 取最后一个。没有时返回空。
// 任务 ID 不合法或 limit 越界时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) ThreadReferences(ctx context.Context, taskID string, beforeID int64, limit int) ([]string, error) {
	if err := checkTaskID(taskID); err != nil {
		return nil, fmt.Errorf("invalid notification query: %w", err)
	}
	if limit < 1 || limit > maxThreadReferences {
		return nil, invalidArgument("invalid notification query: limit must be 1-%d", maxThreadReferences)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT delivered_message_id FROM (SELECT delivered_message_id, sent_at, id FROM notifications "+
			"WHERE task_id = ? AND id < ? AND state = ? AND delivered_message_id IS NOT NULL ORDER BY sent_at DESC, id DESC LIMIT ?) "+
			"ORDER BY sent_at, id",
		taskID, beforeID, string(queue.OutboxSent), limit)
	if err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	defer rows.Close()
	var references []string
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return nil, fmt.Errorf("cannot read notifications: %w", err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	return references, nil
}

// CountNotificationsSince 返回状态为 SENT 或 UNCERTAIN、且发出时刻 COALESCE(sent_at, updated_at) 不早于 since（按毫秒，恰在 since 的
// 计入）的通知数，即 D8 全局发信上限 M 所计的「滚动窗口内已发出的通知」。SENT 按 sent_at 计，记下实际投递 ID 改动 updated_at 不影响；
// 核对为已送达的通知（本地核对与 D7 的副本核对都如此）按进入 UNCERTAIN 的时刻计（sent_at 沿用该时刻），不在核对时刻再计一次。
func (s *Store) CountNotificationsSince(ctx context.Context, since time.Time) (int, error) {
	return s.count(ctx, "notifications", "SELECT count(*) FROM notifications WHERE state IN (?, ?) AND coalesce(sent_at, updated_at) >= ?",
		string(queue.OutboxSent), string(queue.OutboxUncertain), since.UnixMilli())
}

// AbandonedSince 返回原因为 expired 或 task_closed、位于游标 (since, afterID) 之后的 ABANDONED 通知——updated_at 晚于 since，或恰为
// since（按毫秒）且 id 大于 afterID——按 (updated_at, id) 升序，至多 100 条；发送循环据此在本地提示「令牌将过期而放弃」与「任务已关闭
// 而放弃」（4a「风险与后续」要求 4b 在本地提示）。同一轮内以上一页最后一行的 (updated_at, id) 作为下一页的游标，返回满 100 条时
// 接着取，直到不足 100 条：同一毫秒放弃的通知多于 100 条时（例如关闭一个积压很多通知的任务）也因此不重不漏，而只以时刻作游标，
// 要么重复返回那一毫秒，要么漏掉其中第 100 条之后的。跨轮不要直接以上一轮最后一行作游标：同一毫秒内读取之后才放弃的、id 更小的通知
// 会落在它之前；下一轮应从上一轮最后一行那一毫秒的 id 0 起重读，按已提示过的通知 ID 去重（4b Task 11）。游标的前提是通知按
// updated_at 的先后被放弃：放弃都以当前时间写 updated_at，ABANDONED 是终态，之后 updated_at 不再改变；时钟回拨期间放弃的通知落在
// 游标之前，不会返回。
func (s *Store) AbandonedSince(ctx context.Context, since time.Time, afterID int64) ([]Notification, error) {
	at := since.UnixMilli()
	return s.listNotifications(ctx, abandonedSince, string(queue.OutboxAbandoned), abandonExpired, abandonTaskClosed, at, at, afterID, maxNotificationList)
}

// SentWithoutDeliveredID 返回 sent_at 早于 sentBefore（按毫秒，恰在 sentBefore 的不返回）、仍没有实际投递 ID、id 大于 afterID 的 SENT
// 通知，按 id 升序，至多 100 条；供「「已发送」中找不到通知副本」告警（4b Task 10）。afterID 只用于同一轮内逐页取完：第一页为 0，
// 返回满 100 条时以本页最后一行的 id 接着取——关闭「保存到已发送」后这类通知只增不减，不带游标时一次只能取到最旧的 100 条。
// 跨轮不要以已报告的最大 id 作游标，而应每轮从 0 起重读、按已报告的通知 ID 去重：通知越过 sentBefore 的先后与 id 的先后不一致——
// id 较小的通知可能因重新排队而较晚发出，也可能在较晚时才被本地核对为已送达（sent_at 回溯到进入 UNCERTAIN 的时刻）——
// 以最大 id 作游标时，这样的通知永远不会被报告。
func (s *Store) SentWithoutDeliveredID(ctx context.Context, sentBefore time.Time, afterID int64) ([]Notification, error) {
	return s.listNotifications(ctx, sentWithoutDelivered, string(queue.OutboxSent), sentBefore.UnixMilli(), afterID, maxNotificationList)
}

// HasNotifications 报告库中是否有过任何通知（任何状态，包括已放弃与已发出的）；收取循环据此判断首次运行时是否跳过历史邮件：
// 还没有任何通知时不可能有合法回复。
func (s *Store) HasNotifications(ctx context.Context) (bool, error) {
	return s.exists(ctx, "notifications", "SELECT EXISTS (SELECT 1 FROM notifications)")
}

// changeNotification 在一个事务中读取通知及其任务，交给 change 经状态机计算并写入变化，提交后返回通知的最新快照。
// 通知不存在时返回 ErrNotFound；change 返回错误时事务回滚，数据保持不变。通知因此进入 SENT 或 ABANDONED 时，
// 内容已由触发器删除，提交后执行检查点。
func (s *Store) changeNotification(ctx context.Context, id int64, change func(ctx context.Context, tx *sql.Tx, n Notification, t Task, now int64) error) (Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getNotification(ctx, tx, notificationByID, id)
	if err != nil {
		return Notification{}, err
	}
	owner, err := getTask(ctx, tx, current.TaskID)
	if err != nil {
		return Notification{}, err
	}
	if err := change(ctx, tx, current, owner, s.now().UnixMilli()); err != nil {
		return Notification{}, err
	}
	updated, err := getNotification(ctx, tx, notificationByID, id)
	if err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, fmt.Errorf("cannot commit notification: %w", err)
	}
	if updated.State != current.State && (updated.State == queue.OutboxSent || updated.State == queue.OutboxAbandoned) {
		truncateWAL(ctx, s.db)
	}
	return updated, nil
}

// markSent 把处于 from 的通知改为 SENT，sent_at 记为 sentAt，updated_at 记为 now。
func markSent(ctx context.Context, tx *sql.Tx, n Notification, from queue.OutboxState, sentAt time.Time, now int64) error {
	next, err := nextNotification(n, from, queue.OutboxDelivered)
	if err != nil {
		return err
	}
	next.SentAt = sentAt
	return updateNotification(ctx, tx, n, next, now)
}

// resolveDelivered 把 UNCERTAIN 通知 n 核对为已送达（SENT），本地核对与 D7 的副本核对共用它：sent_at 取通知进入 UNCERTAIN 的时刻，
// 即核对前的 updated_at（UNCERTAIN 通知的 updated_at 只在进入 UNCERTAIN 时写入），而不是核对时刻 now，发出顺序与发信计数因此不随
// 核对而变；updated_at 记为 now。通知不是 UNCERTAIN 时返回 queue.ErrInvalidOutboxTransition。
func resolveDelivered(ctx context.Context, tx *sql.Tx, n Notification, now int64) error {
	return markSent(ctx, tx, n, queue.OutboxUncertain, n.UpdatedAt, now)
}

// putBackNotification 处理确认未投递、处于 from 的通知：任务已关闭时改为 ABANDONED(task_closed)，已关闭任务的通知不再回到 PENDING；
// 否则改回 PENDING，下次可发送时间为 notBefore。
func putBackNotification(ctx context.Context, tx *sql.Tx, n Notification, from queue.OutboxState, t Task, notBefore, now int64) error {
	if t.State == task.Closed {
		next, err := nextNotification(n, from, queue.OutboxAbandon)
		if err != nil {
			return err
		}
		next.AbandonReason = abandonTaskClosed
		return updateNotification(ctx, tx, n, next, now)
	}
	next, err := nextNotification(n, from, queue.OutboxRequeue)
	if err != nil {
		return err
	}
	next.NotBefore = time.UnixMilli(notBefore).UTC()
	return updateNotification(ctx, tx, n, next, now)
}

// nextNotification 确认通知处于该操作要求的来源状态 from，再用 queue.NextOutbox 计算 event 之后的状态，返回改写了状态的副本。
// 状态机允许、但不属于该操作的来源状态（例如对 UNCERTAIN 通知调用 MarkNotificationSent）同样按非法转移拒绝。
func nextNotification(n Notification, from queue.OutboxState, event queue.OutboxEvent) (Notification, error) {
	if n.State != from {
		return Notification{}, fmt.Errorf("%w: notification %d is %s, %s requires %s", queue.ErrInvalidOutboxTransition, n.ID, n.State, event, from)
	}
	to, err := queue.NextOutbox(from, event)
	if err != nil {
		return Notification{}, err
	}
	n.State = to
	return n, nil
}

// updateNotification 以读取时的状态为条件，把通知改写为 next 的状态、放弃原因、尝试次数、下次可发送时间与发送时间。
// IMMEDIATE 事务已在读取前取得写锁，状态条件只作防御；受影响行数不为 1 时返回错误使事务回滚。
func updateNotification(ctx context.Context, tx *sql.Tx, current, next Notification, now int64) error {
	var sentAt sql.NullInt64
	if !next.SentAt.IsZero() {
		sentAt = sql.NullInt64{Int64: next.SentAt.UnixMilli(), Valid: true}
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE notifications SET state = ?, abandon_reason = ?, attempts = ?, not_before = ?, sent_at = ?, updated_at = ? WHERE id = ? AND state = ?",
		string(next.State), sql.NullString{String: next.AbandonReason, Valid: next.AbandonReason != ""}, next.Attempts,
		next.NotBefore.UnixMilli(), sentAt, now, current.ID, string(current.State))
	return requireOneRow(result, err, current.ID)
}

// requireOneRow 检查针对通知 id 的更新语句恰好改动一行；出错或未命中时返回错误使事务回滚。
func requireOneRow(result sql.Result, err error, id int64) error {
	if err != nil {
		return fmt.Errorf("cannot update notification: %w", err)
	}
	if updated, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("cannot update notification: %w", err)
	} else if updated != 1 {
		return fmt.Errorf("notification %d changed during update", id)
	}
	return nil
}

// activePayloadKey 返回存储持有的正文密钥；没有密钥，或其 kid 未登记为 active 时返回 ErrPayloadKeyUnavailable。
func (s *Store) activePayloadKey(ctx context.Context, q rowQuerier) (*payload.Key, error) {
	if s.payloadKey == nil {
		return nil, fmt.Errorf("%w: no payload key was loaded", ErrPayloadKeyUnavailable)
	}
	state, err := keyStateOf(ctx, q, KeyPurposePayload, s.payloadKey.ID())
	if errors.Is(err, ErrNotFound) || (err == nil && state != KeyActive) {
		return nil, fmt.Errorf("%w: payload key %d is not registered as active", ErrPayloadKeyUnavailable, s.payloadKey.ID())
	}
	if err != nil {
		return nil, err
	}
	return s.payloadKey, nil
}

// openPayload 在事务 tx 中按 query（以 id 为唯一参数，选出 key_id 与 sealed）读出并解密一条正文，回复与通知共用这套错误划分：
// 正文行缺失返回 ErrPayloadMissing；没有正文密钥、密文的 key_id 与当前密钥不符或该 kid 未登记为 active 返回 ErrPayloadKeyUnavailable；
// 无法解密返回包装 payload.ErrDecrypt 的错误。name 只用于错误文本，错误文本不含正文与密文。
func (s *Store) openPayload(ctx context.Context, tx *sql.Tx, kind payload.Kind, name, query, taskID string, id int64) ([]byte, error) {
	var keyID uint8
	var sealed []byte
	err := tx.QueryRowContext(ctx, query, id).Scan(&keyID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s %d: %w", name, id, ErrPayloadMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s content: %w", name, err)
	}
	key, err := s.activePayloadKey(ctx, tx)
	if err != nil {
		return nil, err
	}
	if keyID != key.ID() {
		return nil, fmt.Errorf("%w: %s %d is sealed with key %d, not %d", ErrPayloadKeyUnavailable, name, id, keyID, key.ID())
	}
	content, err := key.Open(kind, taskID, id, sealed)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s %d: %w", name, id, err)
	}
	return content, nil
}

// notificationsIn 按 id 升序返回处于 state 的全部通知快照：先读完 id、结果集随之关闭，再在同一事务中逐条读取。
func notificationsIn(ctx context.Context, tx *sql.Tx, state queue.OutboxState) ([]Notification, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM notifications WHERE state = ? ORDER BY id", string(state))
	if err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cannot read notifications: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	var notifications []Notification
	for _, id := range ids {
		n, err := getNotification(ctx, tx, notificationByID, id)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, n)
	}
	return notifications, nil
}

// notificationSelect 是读取通知快照的 SELECT 与 FROM 子句，owner 取自所属任务；调用方在其后接上查找条件
// （可带 ORDER BY 与 LIMIT），条件只来自本包的常量。
const notificationSelect = "SELECT n.id, n.task_id, t.owner, n.event, n.nid, n.message_id, n.delivered_message_id, n.token_kid, n.token_expires_at, " +
	"n.state, n.abandon_reason, n.attempts, n.not_before, n.sent_at, n.created_at, n.updated_at " +
	"FROM notifications n JOIN tasks t ON t.id = n.task_id WHERE "

// rowScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan 方法，使单行读取与列表读取共用同一套列与换算。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanNotification 按 notificationSelect 的列读出一条通知快照，时间换算为 UTC；读取失败时返回包装原错误（含 sql.ErrNoRows）的错误，
// 状态名不在已知集合时返回错误而不是静默接受。
func scanNotification(row rowScanner) (Notification, error) {
	var n Notification
	var nid []byte
	var delivered, reason sql.NullString
	var sentAt sql.NullInt64
	var expiresAt, notBefore, createdAt, updatedAt int64
	if err := row.Scan(&n.ID, &n.TaskID, &n.Owner, &n.Event, &nid, &n.MessageID, &delivered, &n.TokenKeyID, &expiresAt,
		&n.State, &reason, &n.Attempts, &notBefore, &sentAt, &createdAt, &updatedAt); err != nil {
		return Notification{}, fmt.Errorf("cannot read notification: %w", err)
	}
	if !n.State.Valid() {
		return Notification{}, fmt.Errorf("notification %d has unknown state %q", n.ID, n.State)
	}
	// 表约束保证 nid 恰为 12 字节。
	copy(n.NID[:], nid)
	n.DeliveredMessageID = delivered.String
	n.AbandonReason = reason.String
	if sentAt.Valid {
		n.SentAt = time.UnixMilli(sentAt.Int64).UTC()
	}
	n.TokenExpiresAt = time.UnixMilli(expiresAt).UTC()
	n.NotBefore = time.UnixMilli(notBefore).UTC()
	n.CreatedAt = time.UnixMilli(createdAt).UTC()
	n.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return n, nil
}

// getNotification 通过 q 读取满足查找条件 where（本包的常量，参数为 args）的一条通知快照；不存在时返回 ErrNotFound，
// 状态名不在已知集合时返回错误而不是静默接受。
func getNotification(ctx context.Context, q rowQuerier, where string, args ...any) (Notification, error) {
	n, err := scanNotification(q.QueryRowContext(ctx, notificationSelect+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, fmt.Errorf("notification: %w", ErrNotFound)
	}
	return n, err
}

// listNotifications 读取满足查找条件 where（本包的常量，含 ORDER BY 与 LIMIT，参数为 args）的通知快照列表，没有时返回空。
// 结果集在一个查询中读完，期间不发出其他查询：存储只有一个连接，嵌套的查询会一直等待这个连接。
func (s *Store) listNotifications(ctx context.Context, where string, args ...any) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx, notificationSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	defer rows.Close()
	var notifications []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read notifications: %w", err)
	}
	return notifications, nil
}
