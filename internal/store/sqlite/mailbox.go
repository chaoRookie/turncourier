// Package sqlite 保存 IMAP 收取游标与被拒来信的元数据：游标在同一 UIDVALIDITY 下只进不退，UIDVALIDITY 变化时整体重置；
// 被拒来信只记录元数据与原因码，不含主题与正文，也从不回信（防回环与反向散射），按 UID 或 Message-ID 查找以免重新判定，
// 并按时间分批清理。这里还定义来信字段（Message-ID、账户与发件人地址、文件夹）的校验规则，存储各处与调用方共用同一份。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Cursor 是某个文件夹的收取游标；LastUID 为 0 表示该 UIDVALIDITY 下尚未处理任何邮件。
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// ErrCursorRegression 表示在 UIDVALIDITY 不变时试图把 LastUID 调小。
var ErrCursorRegression = errors.New("fetch cursor would move backwards")

// RejectReason 是被拒来信的原因码；合法取值只由下列常量定义，前 13 个来自 4a，其后 7 个由 4b 增补（主题标签缺失、出现多次或损坏，
// 令牌已被更新的通知取代，回环刹车与邮件暂停，新正文为空）。
type RejectReason string

const (
	RejectAutoReply          RejectReason = "auto_reply"
	RejectBounce             RejectReason = "bounce"
	RejectSenderNotAllowed   RejectReason = "sender_not_allowed"
	RejectTooLarge           RejectReason = "too_large"
	RejectMalformed          RejectReason = "malformed"
	RejectParseUncertain     RejectReason = "parse_uncertain"
	RejectThreadMismatch     RejectReason = "thread_mismatch"
	RejectSubjectTag         RejectReason = "subject_tag_invalid"
	RejectTokenMissing       RejectReason = "token_missing"
	RejectTokenInvalid       RejectReason = "token_invalid"
	RejectTokenExpired       RejectReason = "token_expired"
	RejectTaskUnknown        RejectReason = "task_unknown"
	RejectMessageConflict    RejectReason = "message_conflict"
	RejectSubjectTagMissing  RejectReason = "subject_tag_missing"
	RejectSubjectTagMultiple RejectReason = "subject_tag_multiple"
	RejectSubjectTagDamaged  RejectReason = "subject_tag_damaged"
	RejectTokenSuperseded    RejectReason = "token_superseded"
	RejectRateLimited        RejectReason = "rate_limited"
	RejectMailPaused         RejectReason = "mail_paused"
	RejectEmptyBody          RejectReason = "empty_body"
)

// knownRejectReasons 是 RecordRejection 接受的原因码，即上列全部常量。数据库只约束形状（1–40 个 [a-z_] 字符），
// 增补原因码只需在这里与上面的常量中同时加上，不需要迁移。
var knownRejectReasons = map[RejectReason]bool{
	RejectAutoReply:          true,
	RejectBounce:             true,
	RejectSenderNotAllowed:   true,
	RejectTooLarge:           true,
	RejectMalformed:          true,
	RejectParseUncertain:     true,
	RejectThreadMismatch:     true,
	RejectSubjectTag:         true,
	RejectTokenMissing:       true,
	RejectTokenInvalid:       true,
	RejectTokenExpired:       true,
	RejectTaskUnknown:        true,
	RejectMessageConflict:    true,
	RejectSubjectTagMissing:  true,
	RejectSubjectTagMultiple: true,
	RejectSubjectTagDamaged:  true,
	RejectTokenSuperseded:    true,
	RejectRateLimited:        true,
	RejectMailPaused:         true,
	RejectEmptyBody:          true,
}

const (
	// maxRejectionsLimit 是 Rejections 一次最多返回的条数。
	maxRejectionsLimit = 1000
	// maxPruneRejections 是 PruneRejections 一次最多删除的条数；多出的留给下一次，一次清理不会因垃圾邮件突发而长时间占用写锁。
	maxPruneRejections = 10000
)

// rejectedBeforeResetQuery 按 Message-ID 查找同一账户与文件夹中 UIDVALIDITY 不同的被拒记录。它走 0003 的
// inbound_rejections_by_message 部分索引：message_id = ? 蕴含该索引的条件 message_id IS NOT NULL。测试据此核对查询计划。
const rejectedBeforeResetQuery = "SELECT EXISTS (SELECT 1 FROM inbound_rejections WHERE account = ? AND folder = ? AND message_id = ? AND uid_validity <> ?)"

// Rejection 是一封被拒来信的元数据，不含主题与正文。ID 与 ReceivedAt 由存储填写：RecordRejection 忽略输入中的这两个字段，
// 分别取数据库分配的 ID 与存储时钟的当前时间（UTC 毫秒）。
type Rejection struct {
	ID          int64
	Account     string
	Folder      string
	UIDValidity uint32
	UID         uint32
	MessageID   string // 可为空
	Sender      string // 调用方规范化后的发件人地址，可为空
	Reason      RejectReason
	TaskID      string // 能识别任务时填写，可为空
	ReceivedAt  time.Time
}

// FetchCursor 读取 (account, folder) 的游标；没有记录时返回 ErrNotFound。account 与 folder 按 checkMailbox 的规则校验，
// 不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) FetchCursor(ctx context.Context, account, folder string) (Cursor, error) {
	if err := checkMailbox(account, folder); err != nil {
		return Cursor{}, fmt.Errorf("invalid fetch cursor: %w", err)
	}
	var c Cursor
	err := s.db.QueryRowContext(ctx, "SELECT uid_validity, last_uid FROM fetch_cursors WHERE account = ? AND folder = ?", account, folder).
		Scan(&c.UIDValidity, &c.LastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, fmt.Errorf("fetch cursor: %w", ErrNotFound)
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("cannot read fetch cursor: %w", err)
	}
	return c, nil
}

// AdvanceCursor 写入游标：UIDVALIDITY 与已有记录相同时 LastUID 只能增加或不变，否则返回 ErrCursorRegression 且不改动；
// UIDVALIDITY 不同或尚无记录时整体写入（重置）。调用方在一批邮件全部处理并提交后才调用。
// account、folder 与 UIDVALIDITY 在开始事务前校验，不合法时返回包装 ErrInvalidArgument 的错误。比较与写入由一条 UPSERT 完成：
// 冲突时只有 WHERE 成立才更新，受影响行数为 0 即是回退，因此多个进程并发写入同一游标也不会让它后退。
func (s *Store) AdvanceCursor(ctx context.Context, account, folder string, c Cursor) error {
	if err := checkMailbox(account, folder); err != nil {
		return fmt.Errorf("invalid fetch cursor: %w", err)
	}
	if c.UIDValidity == 0 {
		return invalidArgument("invalid fetch cursor: uid validity must be positive")
	}
	result, err := s.db.ExecContext(ctx,
		"INSERT INTO fetch_cursors (account, folder, uid_validity, last_uid, updated_at) VALUES (?, ?, ?, ?, ?) "+
			"ON CONFLICT (account, folder) DO UPDATE SET uid_validity = excluded.uid_validity, last_uid = excluded.last_uid, updated_at = excluded.updated_at "+
			"WHERE excluded.uid_validity <> fetch_cursors.uid_validity OR excluded.last_uid >= fetch_cursors.last_uid",
		account, folder, c.UIDValidity, c.LastUID, s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("cannot advance fetch cursor: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cannot advance fetch cursor: %w", err)
	}
	if written == 0 {
		return ErrCursorRegression
	}
	return nil
}

// RecordRejection 写入被拒来信；同一 (account, folder, uid_validity, uid) 已有记录时返回原记录 ID 与 duplicate=true，不覆盖，
// 也不再检查任务。原因码不在列表中、字段长度或空白不符、含 NUL 或非法 UTF-8 时在开始事务前返回包装 ErrInvalidArgument 的错误，
// 错误文本不回显地址与 Message-ID；TaskID 不存在时返回 ErrNotFound。received_at 取存储时钟的当前时间。
func (s *Store) RecordRejection(ctx context.Context, r Rejection) (id int64, duplicate bool, err error) {
	if err := r.validate(); err != nil {
		return 0, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略；命中已有记录时由它结束只读的事务。
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT id FROM inbound_rejections WHERE account = ? AND folder = ? AND uid_validity = ? AND uid = ?",
		r.Account, r.Folder, r.UIDValidity, r.UID).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("cannot read rejections: %w", err)
	}
	if r.TaskID != "" {
		if _, err := getTask(ctx, tx, r.TaskID); err != nil {
			return 0, false, err
		}
	}
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO inbound_rejections (account, folder, uid_validity, uid, message_id, sender, reason, task_id, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		r.Account, r.Folder, r.UIDValidity, r.UID, optional(r.MessageID), optional(r.Sender), string(r.Reason), optional(r.TaskID), s.now().UnixMilli()).
		Scan(&id); err != nil {
		return 0, false, fmt.Errorf("cannot record rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("cannot commit rejection: %w", err)
	}
	return id, false, nil
}

// Rejections 按 (received_at, id) 升序返回 received_at 晚于 since 的被拒来信（按毫秒比较，恰在 since 的不返回），
// 至多 limit 条（1–1000），越界时在查询前返回包装 ErrInvalidArgument 的错误。它只适合查看某一时刻之后的前 limit 条，不能用来逐页读完：
// 同一毫秒可能有多条记录，以上一页最后一条的 ReceivedAt 作为下一页的 since 会漏掉与它同一毫秒的其余记录。
// 原因码按原样返回，不按本版本的列表校验：它只用于展示，较新版本增补的原因码仍可读出。
func (s *Store) Rejections(ctx context.Context, since time.Time, limit int) ([]Rejection, error) {
	if limit < 1 || limit > maxRejectionsLimit {
		return nil, invalidArgument("invalid rejections query: limit must be 1-1000")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, account, folder, uid_validity, uid, message_id, sender, reason, task_id, received_at FROM inbound_rejections "+
			"WHERE received_at > ? ORDER BY received_at, id LIMIT ?", since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("cannot read rejections: %w", err)
	}
	defer rows.Close()
	var rejections []Rejection
	for rows.Next() {
		var r Rejection
		var messageID, sender, taskID sql.NullString
		var receivedAt int64
		if err := rows.Scan(&r.ID, &r.Account, &r.Folder, &r.UIDValidity, &r.UID, &messageID, &sender, &r.Reason, &taskID, &receivedAt); err != nil {
			return nil, fmt.Errorf("cannot read rejections: %w", err)
		}
		r.MessageID, r.Sender, r.TaskID = messageID.String, sender.String, taskID.String
		r.ReceivedAt = time.UnixMilli(receivedAt).UTC()
		rejections = append(rejections, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read rejections: %w", err)
	}
	return rejections, nil
}

// RejectionExists 报告同一 (account, folder, uidValidity, uid) 是否已有被拒记录，供验证流水线跳过已经判定过的来信：
// 批因基础设施错误被重新交付时，已拒绝的来信不会因时间窗移动等原因被翻案。账户与文件夹按 checkMailbox 校验，UIDVALIDITY 与 UID
// 须为正，不合法时在查询前返回包装 ErrInvalidArgument 的错误。
func (s *Store) RejectionExists(ctx context.Context, account, folder string, uidValidity, uid uint32) (bool, error) {
	if err := checkMailbox(account, folder); err != nil {
		return false, fmt.Errorf("invalid rejection query: %w", err)
	}
	if uidValidity == 0 || uid == 0 {
		return false, invalidArgument("invalid rejection query: uid validity and uid must be positive")
	}
	return s.exists(ctx, "rejections",
		"SELECT EXISTS (SELECT 1 FROM inbound_rejections WHERE account = ? AND folder = ? AND uid_validity = ? AND uid = ?)",
		account, folder, uidValidity, uid)
}

// RejectedBeforeReset 报告同一账户与文件夹中是否有 Message-ID 相同、而 UIDVALIDITY 不同于 uidValidity 的被拒记录：UIDVALIDITY 重置后的
// 全量补扫中 UID 都变了，Message-ID 没变，靠它认出早已拒绝的来信。只认 UIDVALIDITY 不同的记录：同一 UIDVALIDITY 下的来信由
// RejectionExists 按 UID 判定，若 Message-ID 相同也算，伪造者就能借一条被拒记录挡掉同一 UIDVALIDITY 下 Message-ID 相同的合法回复。
// 没有 Message-ID 的被拒记录不参与。账户与文件夹按 checkMailbox、UIDVALIDITY 须为正、Message-ID 按 ValidMessageID 校验，
// 不合法时在查询前返回包装 ErrInvalidArgument 的错误，错误文本不回显取值。
func (s *Store) RejectedBeforeReset(ctx context.Context, account, folder string, uidValidity uint32, messageID string) (bool, error) {
	if err := checkMailbox(account, folder); err != nil {
		return false, fmt.Errorf("invalid rejection query: %w", err)
	}
	if uidValidity == 0 {
		return false, invalidArgument("invalid rejection query: uid validity must be positive")
	}
	if err := checkMessageID(messageID); err != nil {
		return false, fmt.Errorf("invalid rejection query: %w", err)
	}
	return s.exists(ctx, "rejections", rejectedBeforeResetQuery, account, folder, messageID, uidValidity)
}

// PruneRejections 删除 received_at 早于 before（按毫秒，恰在 before 的保留）的被拒记录，先删最早的，一次至多 maxPruneRejections 条，
// 多出的留给下一次；返回删除条数。运行中的 App 每天以「当前时间减 30 天」调用一次（被拒记录保留 30 天）：30 天不短于令牌有效期的
// 上限 720 小时，记录被清理时其中的令牌都已过期，重新判定只会以 token_expired 或更早的原因码拒绝，「拒绝不翻案」因此在清理之后
// 仍然成立；改动这两个数中的任何一个，都要重新核对这一点。
func (s *Store) PruneRejections(ctx context.Context, before time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM inbound_rejections WHERE id IN (SELECT id FROM inbound_rejections WHERE received_at < ? ORDER BY received_at, id LIMIT ?)",
		before.UnixMilli(), maxPruneRejections)
	if err != nil {
		return 0, fmt.Errorf("cannot prune rejections: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cannot prune rejections: %w", err)
	}
	return int(deleted), nil
}

// validate 在事务开始前检查原因码与各字段，规则与表约束一致；MessageID、Sender 与 TaskID 可为空（写为 NULL），非空时 MessageID 按
// ValidMessageID、Sender 按 ValidSender 校验。不合法时返回包装 ErrInvalidArgument 的错误，错误文本不含地址与 Message-ID。
func (r Rejection) validate() error {
	if err := checkMailbox(r.Account, r.Folder); err != nil {
		return fmt.Errorf("invalid rejection: %w", err)
	}
	switch {
	case !knownRejectReasons[r.Reason]:
		return invalidArgument("invalid rejection: unknown reason %q", r.Reason)
	case r.UIDValidity == 0 || r.UID == 0:
		return invalidArgument("invalid rejection: uid validity and uid must be positive")
	case r.TaskID != "" && !taskIDPattern.MatchString(r.TaskID):
		return invalidArgument("invalid rejection: task id must be empty or a 10-character task id")
	case r.MessageID != "" && !ValidMessageID(r.MessageID):
		return invalidArgument("invalid rejection: message id must be empty or 3-998 characters of valid UTF-8 without NUL")
	case r.Sender != "" && !ValidSender(r.Sender):
		return invalidArgument("invalid rejection: sender must be empty or 3-254 characters of valid UTF-8 without whitespace or NUL")
	}
	return nil
}

// ValidMessageID 报告 id 是否符合存储对 Message-ID 的规则：3–998 个字符（按 Unicode 字符计）的合法 UTF-8，不含 NUL。
// RecordReply、RecordRejection、RecordDeliveredMessageID、ResolveUncertainAsDelivered 与按 Message-ID 查询的接口都按这同一条规则校验，
// 调用方可以在查询与写入之前用它检查来信中的字段；合法 UTF-8 与不含 NUL 的理由见 checkMailbox。空字符串不合法
// （RecordRejection 把空值当作缺失，不经此规则）。
func ValidMessageID(id string) bool {
	length := utf8.RuneCountInString(id)
	return length >= 3 && length <= 998 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

// ValidSender 报告 sender 是否符合存储对规范化地址的规则（validAddress，与账户相同），即 RecordRejection 接受的发件人。
// 空字符串不合法（RecordRejection 把空值当作缺失，不经此规则）。
func ValidSender(sender string) bool {
	return validAddress(sender)
}

// validAddress 报告 address 是否为 3–254 个字符（按 Unicode 字符计）、不含空白、不含 NUL 的合法 UTF-8；账户与被拒记录的发件人
// 都是调用方规范化后的地址，按这同一条规则校验。
func validAddress(address string) bool {
	length := utf8.RuneCountInString(address)
	return length >= 3 && length <= 254 && !strings.ContainsFunc(address, unicode.IsSpace) &&
		utf8.ValidString(address) && !strings.ContainsRune(address, 0)
}

// checkMessageID 按 ValidMessageID 检查 Message-ID；不合法时返回输入校验错误，文本不回显取值。
func checkMessageID(id string) error {
	if !ValidMessageID(id) {
		return invalidArgument("message id must be 3-998 characters of valid UTF-8 without NUL")
	}
	return nil
}

// checkAccount 按 validAddress 检查机器人账户；不合法时返回输入校验错误，文本不回显取值。
func checkAccount(account string) error {
	if !validAddress(account) {
		return invalidArgument("account must be 3-254 characters of valid UTF-8 without whitespace or NUL")
	}
	return nil
}

// checkMailbox 检查账户与文件夹：账户按 checkAccount，文件夹为 1–255 个字符、可以含空格，长度按 Unicode 字符计。
// 两者都须为合法 UTF-8 且不含 NUL：只有这样按字符计的长度才与表约束中 SQLite 的 length() 一致（length() 遇到 NUL 即停止计数，
// 遇到非法 UTF-8 时可能把多个字节计为一个字符），否则 Go 端判为合规的取值会在执行 SQL 后才被 CHECK 约束拒绝。
// 游标、被拒来信与入站回复按这同一条规则校验：否则一个文件夹名可以记录回复却推不动它的游标。不合法时返回输入校验错误，文本不含取值。
func checkMailbox(account, folder string) error {
	if err := checkAccount(account); err != nil {
		return err
	}
	if length := utf8.RuneCountInString(folder); length < 1 || length > 255 || !utf8.ValidString(folder) || strings.ContainsRune(folder, 0) {
		return invalidArgument("folder must be 1-255 characters of valid UTF-8 without NUL")
	}
	return nil
}

// optional 把空字符串写为 NULL，用于可为空的列。
func optional(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
