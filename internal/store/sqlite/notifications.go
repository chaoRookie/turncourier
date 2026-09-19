// Package sqlite 持久化待发通知：创建时只以密文保存内容，按到期顺序串行领取，记录投递结果；投递结果不确定时只标记为
// UNCERTAIN 等待本地核对，从不自动重发。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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

const (
	// notificationByID 与 notificationByNID 是 getNotification 的查找条件。
	notificationByID  = "n.id = ?"
	notificationByNID = "n.nid = ?"
)

// validate 在事务开始前检查事件、域名、有效期、内容长度与任务 ID 的形式；错误文本不含内容。
func (n NewNotification) validate() error {
	switch {
	case !notificationEvents[n.Event]:
		return fmt.Errorf("invalid notification: unknown event %q", n.Event)
	case !domainPattern.MatchString(n.Domain):
		return errors.New("invalid notification: domain must be 1-253 characters of [a-z0-9.-]")
	case n.TTL < minNotificationTTL || n.TTL > maxNotificationTTL:
		return errors.New("invalid notification: token ttl must be 1h-720h")
	case len(n.Content) == 0 || len(n.Content) > payload.MaxPlaintext:
		return errors.New("invalid notification: content must be 1 byte to 1 MiB")
	case !taskIDPattern.MatchString(n.TaskID):
		return errors.New("invalid notification: task id must be 10 lowercase Crockford base32 characters")
	}
	return nil
}

// CreateNotification 在一个事务中：确认任务存在且未关闭（FAILED 任务允许，用于发送失败通知）；读取令牌用途的 active kid
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
// 正常路径不会产生这种行，这是兜底）。有改动时提交后执行检查点。第二个事务：已有 SENDING 通知时返回
// ErrNoSendableNotification；取 not_before 不晚于当前时间、按 (not_before, id) 最早的 PENDING 通知；读出并解密内容，
// 失败时不领取（返回 ErrPayloadMissing、ErrPayloadKeyUnavailable 或包装 payload.ErrDecrypt 的错误）；
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
	err = tx.QueryRowContext(ctx, "SELECT id FROM notifications WHERE state = ? AND not_before <= ? ORDER BY not_before, id LIMIT 1",
		string(queue.OutboxPending), now).Scan(&id)
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
	content, err := s.openNotificationPayload(ctx, tx, pending)
	if err != nil {
		return Notification{}, nil, err
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

// MarkNotificationSent 在 SMTP 返回 250 后把 SENDING 改为 SENT 并记录 sent_at；内容由触发器删除，提交后执行检查点。
// 任务在发送期间被关闭时照样记为 SENT：邮件已经发出。
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) (Notification, error) {
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, _ Task, now int64) error {
		return markSent(ctx, tx, n, queue.OutboxSending, now)
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
// delay 超出范围时在开始事务前报错。
func (s *Store) RequeueNotification(ctx context.Context, id int64, delay time.Duration) (Notification, error) {
	if delay < 0 || delay > maxRequeueDelay {
		return Notification{}, errors.New("invalid notification requeue: delay must be 0-24h")
	}
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, t Task, now int64) error {
		return putBackNotification(ctx, tx, n, queue.OutboxSending, t, now+delay.Milliseconds(), now)
	})
}

// AbandonNotification 把 PENDING、SENDING 或 UNCERTAIN 通知改为 ABANDONED；reason 只能是 rejected 或 manual
// （task_closed 与 expired 只由存储自身写入），其他取值在开始事务前报错。内容由触发器删除，提交后执行检查点。
func (s *Store) AbandonNotification(ctx context.Context, id int64, reason string) (Notification, error) {
	if reason != abandonRejected && reason != abandonManual {
		return Notification{}, fmt.Errorf("invalid notification abandon reason %q: must be rejected or manual", reason)
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

// ResolveUncertainNotification 记录本地核对结果：delivered 为 true 时 UNCERTAIN 改为 SENT；为 false 时改回 PENDING
// （not_before 为当前时间），任务已关闭时改为 ABANDONED(task_closed)。进入 SENT 或 ABANDONED 时内容由触发器删除，
// 提交后执行检查点。系统从不自动调用它。
func (s *Store) ResolveUncertainNotification(ctx context.Context, id int64, delivered bool) (Notification, error) {
	return s.changeNotification(ctx, id, func(ctx context.Context, tx *sql.Tx, n Notification, t Task, now int64) error {
		if delivered {
			return markSent(ctx, tx, n, queue.OutboxUncertain, now)
		}
		return putBackNotification(ctx, tx, n, queue.OutboxUncertain, t, now, now)
	})
}

// RecordDeliveredMessageID 为 SENT 通知记录实际投递的 Message-ID（3–998 个字符，不含 NUL）；同值重复记录不改动，
// 已有不同值或该值已属于另一条通知时返回 ErrDeliveredMessageIDConflict；通知不是 SENT 时返回 queue.ErrInvalidOutboxTransition。
// 值的长度按 Unicode 字符计，与表约束一致，不合法时在开始事务前报错。
func (s *Store) RecordDeliveredMessageID(ctx context.Context, id int64, messageID string) (Notification, error) {
	if length := utf8.RuneCountInString(messageID); length < 3 || length > 998 || strings.ContainsRune(messageID, 0) {
		return Notification{}, errors.New("invalid notification delivered message id: must be 3-998 characters without NUL")
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
			messageID, now, n.ID, string(queue.OutboxSent))
		return requireOneRow(result, err, n.ID)
	})
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

// markSent 把处于 from 的通知改为 SENT 并以 now 记录 sent_at。
func markSent(ctx context.Context, tx *sql.Tx, n Notification, from queue.OutboxState, now int64) error {
	next, err := nextNotification(n, from, queue.OutboxDelivered)
	if err != nil {
		return err
	}
	next.SentAt = time.UnixMilli(now).UTC()
	return updateNotification(ctx, tx, n, next, now)
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

// openNotificationPayload 读出并解密通知 n 的内容：正文行缺失返回 ErrPayloadMissing；没有正文密钥、密文的 key_id 与当前密钥不符
// 或该 kid 未登记为 active 返回 ErrPayloadKeyUnavailable；无法解密返回包装 payload.ErrDecrypt 的错误。错误文本不含内容与密文。
func (s *Store) openNotificationPayload(ctx context.Context, tx *sql.Tx, n Notification) ([]byte, error) {
	var keyID uint8
	var sealed []byte
	err := tx.QueryRowContext(ctx, "SELECT key_id, sealed FROM notification_payloads WHERE notification_id = ?", n.ID).Scan(&keyID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("notification %d: %w", n.ID, ErrPayloadMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read notification content: %w", err)
	}
	key, err := s.activePayloadKey(ctx, tx)
	if err != nil {
		return nil, err
	}
	if keyID != key.ID() {
		return nil, fmt.Errorf("%w: notification %d is sealed with key %d, not %d", ErrPayloadKeyUnavailable, n.ID, keyID, key.ID())
	}
	content, err := key.Open(payload.KindNotification, n.TaskID, n.ID, sealed)
	if err != nil {
		return nil, fmt.Errorf("cannot open notification %d: %w", n.ID, err)
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

// getNotification 通过 q 按 where（notificationByID 或 notificationByNID）读取通知快照，owner 取自所属任务，时间换算为 UTC；
// 不存在时返回 ErrNotFound，状态名不在已知集合时返回错误而不是静默接受。
func getNotification(ctx context.Context, q rowQuerier, where string, arg any) (Notification, error) {
	var n Notification
	var nid []byte
	var delivered, reason sql.NullString
	var sentAt sql.NullInt64
	var expiresAt, notBefore, createdAt, updatedAt int64
	err := q.QueryRowContext(ctx,
		"SELECT n.id, n.task_id, t.owner, n.event, n.nid, n.message_id, n.delivered_message_id, n.token_kid, n.token_expires_at, "+
			"n.state, n.abandon_reason, n.attempts, n.not_before, n.sent_at, n.created_at, n.updated_at "+
			"FROM notifications n JOIN tasks t ON t.id = n.task_id WHERE "+where, arg).
		Scan(&n.ID, &n.TaskID, &n.Owner, &n.Event, &nid, &n.MessageID, &delivered, &n.TokenKeyID, &expiresAt,
			&n.State, &reason, &n.Attempts, &notBefore, &sentAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, fmt.Errorf("notification: %w", ErrNotFound)
	}
	if err != nil {
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
